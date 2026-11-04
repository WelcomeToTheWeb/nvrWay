package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"nvr/server/models"
)

// ---------------------------------------------------------------------------
// Storage backends
// ---------------------------------------------------------------------------

// StorageBackendStore provides persistence for storage destinations.
type StorageBackendStore struct {
	db *gorm.DB
}

// Create inserts a backend. Secrets arrive already sealed by the caller
// (`storage/seal.go`): this layer never sees plaintext, so no log, no query log
// and no backup taken by this layer can contain one.
func (s *StorageBackendStore) Create(b *models.StorageBackend) error {
	if b.ID == "" {
		b.ID = uuid.NewString()
	}
	enabled := b.Enabled
	if err := s.db.Create(b).Error; err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: backend name %q already exists", b.Name)
		}
		return err
	}
	// `enabled` defaults to true in the schema and gorm fills the struct with
	// it, so the value wanted is captured above: a backend created disabled
	// stays disabled.
	if !enabled {
		b.Enabled = false
		return writeZeroColumns(s.db, b, map[string]any{"enabled": false})
	}
	return nil
}

// GetByID fetches a backend.
func (s *StorageBackendStore) GetByID(id string) (*models.StorageBackend, error) {
	var b models.StorageBackend
	if err := s.db.First(&b, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

// GetByName fetches a backend by its unique name.
func (s *StorageBackendStore) GetByName(name string) (*models.StorageBackend, error) {
	var b models.StorageBackend
	if err := s.db.First(&b, "name = ?", name).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

// GetDefault returns the backend new jobs land on. When an installation has
// exactly one backend it is the default: asking an operator who has configured
// one destination to also tick "default" is a setup step with no answer in it.
func (s *StorageBackendStore) GetDefault() (*models.StorageBackend, error) {
	var b models.StorageBackend
	if err := s.db.Where("enabled = ? AND is_default = ?", true, true).First(&b).Error; err == nil {
		return &b, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	var all []models.StorageBackend
	if err := s.db.Where("enabled = ?", true).Order("created_at ASC").Find(&all).Error; err != nil {
		return nil, err
	}
	if len(all) == 1 {
		return &all[0], nil
	}
	if len(all) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return nil, gorm.ErrRecordNotFound
}

// ListAll returns every backend, oldest first.
func (s *StorageBackendStore) ListAll() ([]models.StorageBackend, error) {
	var backends []models.StorageBackend
	if err := s.db.Order("created_at ASC").Find(&backends).Error; err != nil {
		return nil, err
	}
	return backends, nil
}

// Update writes the mutable fields. The secret columns are written only when
// the caller supplied a value: a PUT that omits a secret (the console never
// reads secrets back) must not clear the credential it did not send.
func (s *StorageBackendStore) Update(b *models.StorageBackend, sealPassword, sealAccessKey, sealSecretKey, sealPrivateKey bool) error {
	updates := map[string]any{
		"name":         b.Name,
		"kind":         b.Kind,
		"endpoint":     b.Endpoint,
		"region":       b.Region,
		"bucket":       b.Bucket,
		"prefix":       b.Prefix,
		"username":     b.Username,
		"insecure_tls": b.InsecureTLS,
		"root_path":    b.RootPath,
		"enabled":      b.Enabled,
		"is_default":   b.IsDefault,
		"updated_at":   time.Now().UTC(),
	}
	if sealPassword {
		updates["password"] = b.Password
	}
	if sealAccessKey {
		updates["access_key"] = b.AccessKey
	}
	if sealSecretKey {
		updates["secret_key"] = b.SecretKey
	}
	if sealPrivateKey {
		updates["private_key_path"] = b.PrivateKeyPath
	}
	result := s.db.Model(&models.StorageBackend{}).Where("id = ?", b.ID).Updates(updates)
	if result.Error != nil {
		if isUniqueViolation(result.Error) {
			return fmt.Errorf("store: backend name %q already exists", b.Name)
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// SetDefault makes one backend the default and clears the flag on the rest, in
// one transaction: two rows claiming to be the default is a queue that splits
// its output across destinations, which no operator asked for.
func (s *StorageBackendStore) SetDefault(id string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var b models.StorageBackend
		if err := tx.First(&b, "id = ?", id).Error; err != nil {
			return err
		}
		if !b.Enabled {
			return errors.New("store: a disabled backend cannot be the default")
		}
		if err := tx.Model(&models.StorageBackend{}).Where("id <> ?", id).
			Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&models.StorageBackend{}).Where("id = ?", id).
			Update("is_default", true).Error
	})
}

// SetEnabled stops or starts this backend. Disabled means new jobs stop being
// claimed; in-flight jobs finish (Step 7's uploader rule, stated here because
// it is what the column means).
func (s *StorageBackendStore) SetEnabled(id string, enabled bool) error {
	result := s.db.Model(&models.StorageBackend{}).Where("id = ?", id).Updates(map[string]any{
		"enabled":    enabled,
		"updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// Delete removes a backend. Jobs pointing at it are answered by the handler's
// check, not here: a queue with nowhere to send is a fact the operator has to
// see, so the rows stay and the queue says why it is not moving.
func (s *StorageBackendStore) Delete(id string) error {
	result := s.db.Delete(&models.StorageBackend{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Storage jobs
// ---------------------------------------------------------------------------

// StorageJobStore provides persistence for the upload queue.
type StorageJobStore struct {
	db *gorm.DB
}

// JobInput is one enqueue request.
type JobInput struct {
	// ID is the segment's or event's id: an object has one job row, so a
	// tick-driven queue cannot grow a pile of duplicate uploads.
	ID        string
	Kind      string
	Target    string
	BackendID string
	Action    string
	Bytes     int64
}

// Enqueue records (or renews) the object's next obligation to the tier.
//
// Enqueuing an object that already has a job **replaces** it: same object, new
// action. That is the queue's shape — upload, then delete_local, then
// delete_remote — and it is why the sweeper can hand an object to the queue
// repeatedly without the table becoming the record of every tick.
func (s *StorageJobStore) Enqueue(in JobInput) error {
	if in.ID == "" || in.BackendID == "" {
		return errors.New("storage: job id and backend_id are required")
	}
	if !models.ValidJobKind(in.Kind) {
		return fmt.Errorf("storage: job kind %q is not a job kind", in.Kind)
	}
	if !models.ValidJobAction(in.Action) {
		return fmt.Errorf("storage: job action %q is not a job action", in.Action)
	}
	now := time.Now().UTC()
	job := &models.StorageJob{
		ID:            in.ID,
		Kind:          in.Kind,
		Target:        in.Target,
		BackendID:     in.BackendID,
		Action:        in.Action,
		State:         models.JobStatePending,
		Attempts:      0,
		Bytes:         in.Bytes,
		NextAttemptAt: &now,
	}
	return s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"kind":       job.Kind,
			"target":     job.Target,
			"backend_id": job.BackendID,
			"action":     job.Action,
			"state":      job.State,
			"attempts":   0,
			"worker_id":  "",
			"last_error": "",
			"bytes":      job.Bytes,
			// `remote_key` is deliberately **not** reset here. It is a fact about
			// the object, not about the obligation: once the bytes have reached
			// the tier the object has an address there, and the next action —
			// `delete_local`, then `delete_remote` — is meaningless without it.
			// Clearing it on re-enqueue would make the sweeper's own chain
			// self-defeating, and the row would say "uploaded, address unknown".
			// `Requeue` below *does* clear it, because moving an object to a
			// different backend really does invalidate the address.
			"next_attempt_at": now,
			"updated_at":      now,
		}),
	}).Create(job).Error
}

// GetByID fetches a job.
func (s *StorageJobStore) GetByID(id string) (*models.StorageJob, error) {
	var job models.StorageJob
	if err := s.db.First(&job, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

// QueueCounts is the queue's shape in one line: the numbers the WebSocket
// message carries, so an operator reads "3 pending, 1 uploading, 0 failed"
// instead of inferring it from logs.
type QueueCounts struct {
	Pending   int64  `json:"pending"`
	Uploading int64  `json:"uploading"`
	Failed    int64  `json:"failed"`
	Uploaded  int64  `json:"uploaded"`
	LastError string `json:"last_error"`
}

// Counts tallies the queue, optionally for one backend.
func (s *StorageJobStore) Counts(backendID string) (*QueueCounts, error) {
	counts := &QueueCounts{}
	for state, dst := range map[string]*int64{
		models.JobStatePending:   &counts.Pending,
		models.JobStateUploading: &counts.Uploading,
		models.JobStateFailed:    &counts.Failed,
		models.JobStateUploaded:  &counts.Uploaded,
	} {
		query := s.db.Model(&models.StorageJob{}).Where("state = ?", state)
		if backendID != "" {
			query = query.Where("backend_id = ?", backendID)
		}
		if err := query.Count(dst).Error; err != nil {
			return nil, err
		}
	}
	var last models.StorageJob
	lastQuery := s.db.Model(&models.StorageJob{}).Where("last_error <> ''").Order("updated_at DESC")
	if backendID != "" {
		lastQuery = lastQuery.Where("backend_id = ?", backendID)
	}
	if err := lastQuery.First(&last).Error; err == nil {
		counts.LastError = last.LastError
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return counts, nil
}

// ListByState returns jobs in a state, oldest first, for the queue endpoint.
func (s *StorageJobStore) ListByState(state string, limit, offset int) ([]models.StorageJob, error) {
	if state != "" && !models.ValidJobState(state) {
		// `GET /api/storage/queue?state=` takes one of four values. A state that
		// is not one of them answers an error the handler turns into a 400:
		// read as an empty queue it is a queue page that looks healthy while the
		// operator typed the wrong word.
		return nil, fmt.Errorf("storage: %q is not a job state", state)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var jobs []models.StorageJob
	query := s.db.Model(&models.StorageJob{})
	if state != "" {
		query = query.Where("state = ?", state)
	}
	if err := query.Order("created_at ASC").Limit(limit).Offset(offset).Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}

// ClaimNext takes the oldest due pending job and marks it `uploading` for
// `workerID`, answering gorm.ErrRecordNotFound when the queue is empty.
//
// Postgres takes the row with `FOR UPDATE SKIP LOCKED`, so N ticks never claim
// the same object. sqlite has no such statement — and only one connection is
// open, so the claim is a SELECT then an UPDATE guarded by the state it read,
// inside one transaction. The guard is the point: a claim that does not check
// the state it read hands two workers the same object.
//
// A job whose backend is disabled is not claimable: `PUT
// /api/storage/backends/:id/enabled` with false stops new claims, and a worker
// that claimed anyway would write to the tier the operator just turned off.
// In-flight jobs are untouched — they are already claimed.
func (s *StorageJobStore) ClaimNext(ctx context.Context, workerID string) (*models.StorageJob, error) {
	now := time.Now().UTC()

	if s.db.Dialector.Name() == "postgres" {
		var job models.StorageJob
		err := s.db.WithContext(ctx).Raw(`
			UPDATE storage_jobs
			   SET state = ?, worker_id = ?, attempts = attempts + 1, updated_at = ?
			 WHERE id = (
			       SELECT job.id FROM storage_jobs AS job
			         JOIN storage_backends AS be ON be.id = job.backend_id
			        WHERE job.state = ? AND (job.next_attempt_at IS NULL OR job.next_attempt_at <= ?)
			          AND be.enabled = TRUE
			        ORDER BY job.created_at
			        LIMIT 1
			        FOR UPDATE SKIP LOCKED
			  )
			RETURNING *`,
			models.JobStateUploading, workerID, now, models.JobStatePending, now).Scan(&job).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, gorm.ErrRecordNotFound
		}
		if err != nil {
			return nil, err
		}
		if job.ID == "" {
			return nil, gorm.ErrRecordNotFound
		}
		return &job, nil
	}

	var claimed *models.StorageJob
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job models.StorageJob
		err := tx.Model(&models.StorageJob{}).
			Joins("JOIN storage_backends ON storage_backends.id = storage_jobs.backend_id").
			Where("storage_jobs.state = ? AND (storage_jobs.next_attempt_at IS NULL OR storage_jobs.next_attempt_at <= ?) AND storage_backends.enabled = ?",
				models.JobStatePending, now, true).
			Order("storage_jobs.created_at ASC").
			Select("storage_jobs.*").
			First(&job).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return gorm.ErrRecordNotFound
		}
		if err != nil {
			return err
		}
		result := tx.Model(&models.StorageJob{}).
			Where("id = ? AND state = ?", job.ID, models.JobStatePending).
			Updates(map[string]any{
				"state":      models.JobStateUploading,
				"worker_id":  workerID,
				"attempts":   job.Attempts + 1,
				"updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			// Someone else took it between the read and the write.
			return gorm.ErrRecordNotFound
		}
		job.State = models.JobStateUploading
		job.WorkerID = workerID
		job.Attempts++
		claimed = &job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// MarkUploaded lands a job: the remote key, the byte count, and the timestamp
// the sweeper and the playback proxy read.
func (s *StorageJobStore) MarkUploaded(id, remoteKey string, bytes int64) error {
	result := s.db.Model(&models.StorageJob{}).Where("id = ?", id).Updates(map[string]any{
		"state":      models.JobStateUploaded,
		"remote_key": remoteKey,
		"bytes":      bytes,
		"last_error": "",
		"updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// MarkRetry puts a failed job back in the queue with the next attempt time the
// caller computed, and is the place a job stops: at models.JobMaxAttempts it
// becomes `failed`, which is a number on the queue page, not a log line.
func (s *StorageJobStore) MarkRetry(id, lastError string, nextAttempt time.Time) error {
	var job models.StorageJob
	if err := s.db.First(&job, "id = ?", id).Error; err != nil {
		return err
	}
	state := models.JobStatePending
	if job.Attempts >= models.JobMaxAttempts {
		state = models.JobStateFailed
	}
	return s.db.Model(&models.StorageJob{}).Where("id = ?", id).Updates(map[string]any{
		"state":           state,
		"last_error":      lastError,
		"next_attempt_at": nextAttempt,
		"updated_at":      time.Now().UTC(),
	}).Error
}

// MarkFailed stops a job now rather than at JobMaxAttempts, for the failures a
// retry cannot fix: the local file is gone, the backend row is gone, the job
// names an action no worker implements. Left in `pending` such a job would be
// retried for days and would keep the queue's `pending` number moving, which is
// the number an operator watches to tell whether the tier is healthy.
func (s *StorageJobStore) MarkFailed(id, lastError string) error {
	result := s.db.Model(&models.StorageJob{}).Where("id = ?", id).Updates(map[string]any{
		"state":      models.JobStateFailed,
		"last_error": lastError,
		"updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// Requeue re-shelves every uploaded object of a kind onto another backend: the
// answer to "we moved the tier" that does not require re-recording anything.
func (s *StorageJobStore) Requeue(backendID, kind string) (int64, error) {
	// The destination has to exist: re-shelving onto a backend that is not there
	// produces a queue that fails every job against a row the operator cannot
	// see.
	var backend models.StorageBackend
	if err := s.db.First(&backend, "id = ?", backendID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, fmt.Errorf("storage: backend %q not found", backendID)
		}
		return 0, err
	}
	now := time.Now().UTC()
	result := s.db.Model(&models.StorageJob{}).
		Where("state = ? AND kind = ?", models.JobStateUploaded, kind).
		Updates(map[string]any{
			"state":           models.JobStatePending,
			"backend_id":      backendID,
			"attempts":        0,
			"remote_key":      "",
			"last_error":      "",
			"next_attempt_at": now,
			"updated_at":      now,
		})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// PruneUploaded deletes finished job rows past a window. The queue is the
// upload log while it is running; after 30 days it is history, and the sweep
// that prunes it is the same one that prunes the audit ledger.
func (s *StorageJobStore) PruneUploaded(olderThan time.Time) (int64, error) {
	result := s.db.Where("state = ? AND updated_at < ?", models.JobStateUploaded, olderThan).
		Delete(&models.StorageJob{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// ReleaseStuck returns jobs left `uploading` by a crash to `pending`, so a
// worker that died holding a job does not leave it claimed forever.
func (s *StorageJobStore) ReleaseStuck(olderThan time.Time) (int64, error) {
	result := s.db.Model(&models.StorageJob{}).
		Where("state = ? AND updated_at < ?", models.JobStateUploading, olderThan).
		Updates(map[string]any{
			"state":      models.JobStatePending,
			"worker_id":  "",
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}
