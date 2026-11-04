package api

// The storage surface (admin only): the destinations, the connection test, and
// the queue the uploader walks.
//
// Four rules this file carries.
//
//   - **A secret has no path out of the API.** Every secret column is
//     `json:"-"` on the model, and the view below repeats the rule by naming
//     what is *set* rather than what is stored — including `private_key_path`,
//     which is sealed because which key file an installation uses is
//     information. A write that omits a secret keeps the stored one, which is
//     what lets the console round-trip a row it cannot read back.
//   - **The API validates presence, the driver validates shape.** A missing
//     bucket or a `fs` row with no `root_path` is refused here with a 422 naming
//     the field, because that is the moment the operator is looking at the form.
//     What an endpoint string may *look like*, whether a prefix repeats the
//     endpoint's path, whether a WebDAV container is the first segment — that is
//     the driver's call, made at open, and the test route carries the answer
//     back. Duplicating the driver's grammar here would give one rule two
//     authors, and the two would drift the first time a driver learned something
//     new.
//   - **A connection test answers with one of four names, verbatim.** They are
//     the storage package's sentinel messages, not a table in this file, so
//     there is no second place for them to drift. A refusal that is not one of
//     the four answers `connection_failed` with the driver's sentence beside it:
//     that is not a fifth state, it is the honest absence of a name.
//   - **Saving does not test the tier.** An S3 endpoint that is briefly down
//     must not stop an operator from adding a second destination, and the
//     installation keeps recording to the others meanwhile. The test route is
//     the explicit act, and the Settings page runs it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/models"
	"nvr/server/storage"
)

const (
	// tierProbeTimeout caps the two routes that reach someone else's server. A
	// Settings page that hangs for two minutes because a bucket's DNS name does
	// not resolve is a Settings page nobody uses.
	tierProbeTimeout = 15 * time.Second

	// prefixListMax caps the "verify this path" answer. The route exists to show
	// that a prefix the operator typed is really there, which is a question about
	// the first few keys; a tier holding a million objects should not answer it
	// by streaming a million strings into a browser.
	prefixListMax = 500

	queueListDefault = 50
	queueListMax     = 200

	// connectionFailed is the test route's answer when the tier's refusal is not
	// one of the four named states. `reason` carries the sentence.
	connectionFailed = "connection_failed"
)

// registerStorageRoutes mounts the admin-only storage surface.
func (h *Handlers) registerStorageRoutes(admin *gin.RouterGroup) {
	admin.GET("/storage/backends", h.ListStorageBackends)
	admin.POST("/storage/backends", h.CreateStorageBackend)
	admin.PUT("/storage/backends/:id", h.UpdateStorageBackend)
	admin.DELETE("/storage/backends/:id", h.DeleteStorageBackend)
	admin.POST("/storage/backends/:id/test", h.TestStorageBackend)
	admin.PUT("/storage/backends/:id/default", h.SetStorageBackendDefault)
	admin.PUT("/storage/backends/:id/enabled", h.SetStorageBackendEnabled)
	admin.POST("/storage/backends/:id/prefixes", h.ListStoragePrefixes)
	admin.POST("/storage/backends/:id/requeue", h.RequeueStorageBackend)

	admin.GET("/storage/queue", h.ListStorageQueue)
}

// storageBackendView is a backend as the Settings page reads it. No secret is in
// it; the `*_set` flags are all the API concedes, and they are per column
// because the form has one field per column and needs to know which ones already
// hold something.
type storageBackendView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Endpoint    string `json:"endpoint"`
	Region      string `json:"region"`
	Bucket      string `json:"bucket"`
	Prefix      string `json:"prefix"`
	Username    string `json:"username"`
	InsecureTLS bool   `json:"insecure_tls"`
	RootPath    string `json:"root_path"`
	Enabled     bool   `json:"enabled"`
	IsDefault   bool   `json:"is_default"`

	SecretSet         bool `json:"secret_set"`
	PasswordSet       bool `json:"password_set"`
	AccessKeySet      bool `json:"access_key_set"`
	SecretKeySet      bool `json:"secret_key_set"`
	PrivateKeyPathSet bool `json:"private_key_path_set"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (h *Handlers) storageBackendView(b *models.StorageBackend) storageBackendView {
	return storageBackendView{
		ID:          b.ID,
		Name:        b.Name,
		Kind:        b.Kind,
		Endpoint:    b.Endpoint,
		Region:      b.Region,
		Bucket:      b.Bucket,
		Prefix:      b.Prefix,
		Username:    b.Username,
		InsecureTLS: b.InsecureTLS,
		RootPath:    b.RootPath,
		Enabled:     b.Enabled,
		IsDefault:   b.IsDefault,

		SecretSet:         b.SecretSet(),
		PasswordSet:       b.Password != "",
		AccessKeySet:      b.AccessKey != "",
		SecretKeySet:      b.SecretKey != "",
		PrivateKeyPathSet: b.PrivateKeyPath != "",

		CreatedAt: b.CreatedAt.UTC().Format(timeLayout),
		UpdatedAt: b.UpdatedAt.UTC().Format(timeLayout),
	}
}

// storageBackendInput is the write payload, pointers throughout: an omitted
// field keeps its stored value, and for a secret that is the whole reason the
// console can edit a row whose secrets it cannot read.
//
// An omitted secret and an explicitly empty one are **different acts**. Omitted
// keeps what is stored; `{"password":""}` clears it. The pointer is what tells
// them apart, and a console that sends every field on every save would wipe a
// password the operator never touched — which is why the form sends only what
// changed.
type storageBackendInput struct {
	Name           *string `json:"name"`
	Kind           *string `json:"kind"`
	Endpoint       *string `json:"endpoint"`
	Region         *string `json:"region"`
	Bucket         *string `json:"bucket"`
	Prefix         *string `json:"prefix"`
	Username       *string `json:"username"`
	Password       *string `json:"password"`
	AccessKey      *string `json:"access_key"`
	SecretKey      *string `json:"secret_key"`
	PrivateKeyPath *string `json:"private_key_path"`
	InsecureTLS    *bool   `json:"insecure_tls"`
	RootPath       *string `json:"root_path"`
	Enabled        *bool   `json:"enabled"`
	IsDefault      *bool   `json:"is_default"`
}

// ListStorageBackends answers every backend, enabled or not.
//
// GET /api/storage/backends (admin)
func (h *Handlers) ListStorageBackends(c *gin.Context) {
	backends, err := h.St.StorageBackends.ListAll()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the storage backends are unreadable")
		return
	}
	out := make([]storageBackendView, 0, len(backends))
	for i := range backends {
		out = append(out, h.storageBackendView(&backends[i]))
	}
	h.audit(c, models.AuditStorageRead, http.StatusOK,
		AuditTarget{Type: "collection", ID: "storage_backends"}, map[string]any{"backends": len(out)})
	c.JSON(http.StatusOK, gin.H{"backends": out, "count": len(out)})
}

// CreateStorageBackend adds a destination.
//
// POST /api/storage/backends (admin)
func (h *Handlers) CreateStorageBackend(c *gin.Context) {
	var in storageBackendInput
	if err := c.BindJSON(&in); err != nil {
		jsonError(c, http.StatusBadRequest, "the backend body is not JSON this route understands")
		return
	}

	backend := &models.StorageBackend{Enabled: true}
	writes := applyStorageInput(backend, &in)

	if problem := storageKindProblem(backend); problem != "" {
		h.audit(c, models.AuditStorageCreate, http.StatusUnprocessableEntity,
			AuditTarget{Type: "storage_backend", ID: backend.Name}, map[string]any{"reason": problem})
		jsonError(c, http.StatusUnprocessableEntity, problem)
		return
	}
	if backend.IsDefault && !backend.Enabled {
		jsonError(c, http.StatusUnprocessableEntity, "a disabled backend cannot be the default")
		return
	}
	if err := h.sealBackend(backend); err != nil {
		h.sealFailure(c, models.AuditStorageCreate, backend, err)
		return
	}

	if err := h.St.StorageBackends.Create(backend); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			h.audit(c, models.AuditStorageCreate, http.StatusConflict,
				AuditTarget{Type: "storage_backend", ID: backend.Name}, map[string]any{"name": backend.Name})
			jsonError(c, http.StatusConflict, "backend name taken: "+backend.Name)
			return
		}
		jsonError(c, http.StatusInternalServerError, "the backend row could not be written")
		return
	}

	fresh, err := h.St.StorageBackends.GetByID(backend.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the backend row is unreadable after the write")
		return
	}
	h.audit(c, models.AuditStorageCreate, http.StatusCreated,
		AuditTarget{Type: "storage_backend", ID: fresh.ID}, map[string]any{
			"name":       fresh.Name,
			"kind":       fresh.Kind,
			"endpoint":   fresh.Endpoint,
			"enabled":    fresh.Enabled,
			"is_default": fresh.IsDefault,
			"secrets":    sealedColumns(writes),
		})
	c.JSON(http.StatusCreated, h.storageBackendView(fresh))
}

// UpdateStorageBackend changes a destination. A secret the payload omits keeps
// its stored value; a secret the payload sends empty is cleared.
//
// PUT /api/storage/backends/:id (admin)
func (h *Handlers) UpdateStorageBackend(c *gin.Context) {
	backend, ok := h.storageBackendByID(c)
	if !ok {
		return
	}
	before := *backend

	var in storageBackendInput
	if err := c.BindJSON(&in); err != nil {
		jsonError(c, http.StatusBadRequest, "the backend body is not JSON this route understands")
		return
	}
	// The kind is not movable: the sealed columns a row carries depend on it, and
	// re-shelving an s3 row's credentials onto an sftp row would leave secrets
	// that mean something else.
	if in.Kind != nil && strings.TrimSpace(*in.Kind) != before.Kind {
		jsonError(c, http.StatusUnprocessableEntity, "kind cannot change on an existing backend: delete it and add the other one")
		return
	}
	writes := applyStorageInput(backend, &in)

	if problem := storageKindProblem(backend); problem != "" {
		h.audit(c, models.AuditStorageUpdate, http.StatusUnprocessableEntity,
			AuditTarget{Type: "storage_backend", ID: backend.ID}, map[string]any{"reason": problem})
		jsonError(c, http.StatusUnprocessableEntity, problem)
		return
	}
	if backend.IsDefault && !backend.Enabled {
		jsonError(c, http.StatusUnprocessableEntity, "a disabled backend cannot be the default")
		return
	}
	if err := h.sealBackend(backend); err != nil {
		h.sealFailure(c, models.AuditStorageUpdate, backend, err)
		return
	}

	// The flags say which secret columns to rewrite, and they come from what the
	// payload *sent*, not from what the row holds after sealing. Reading them off
	// the row would lose the one case that matters: a PUT that sends
	// `{"password":""}` to clear a password leaves the column empty, so a flag
	// computed from "is it non-empty" would say "do not write it" and the old
	// secret would survive the operator deleting it.
	if err := h.St.StorageBackends.Update(backend,
		writes.password, writes.accessKey, writes.secretKey, writes.privateKeyPath); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no such backend")
			return
		}
		if strings.Contains(err.Error(), "already exists") {
			jsonError(c, http.StatusConflict, "backend name taken: "+backend.Name)
			return
		}
		jsonError(c, http.StatusInternalServerError, "the backend row could not be written")
		return
	}

	// Clearing the other rows' flags is the store's transaction, not a second
	// write here: two rows claiming to be the default is a queue that splits its
	// output across destinations, which no operator asked for.
	if backend.IsDefault {
		if err := h.St.StorageBackends.SetDefault(backend.ID); err != nil {
			jsonError(c, http.StatusInternalServerError, "the default flag could not be moved")
			return
		}
	}

	fresh, err := h.St.StorageBackends.GetByID(backend.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the backend row is unreadable after the write")
		return
	}
	h.audit(c, models.AuditStorageUpdate, http.StatusOK,
		AuditTarget{Type: "storage_backend", ID: fresh.ID}, map[string]any{
			"name_before":     before.Name,
			"name":            fresh.Name,
			"endpoint_before": before.Endpoint,
			"endpoint":        fresh.Endpoint,
			"enabled_before":  before.Enabled,
			"enabled":         fresh.Enabled,
			"secrets":         sealedColumns(writes),
		})
	c.JSON(http.StatusOK, h.storageBackendView(fresh))
}

// DeleteStorageBackend removes a destination. The queue rows that pointed at it
// stay, and the response counts them, because "the queue is not moving" is a
// question whose answer is a number the operator has to be able to see.
//
// DELETE /api/storage/backends/:id (admin)
func (h *Handlers) DeleteStorageBackend(c *gin.Context) {
	backend, ok := h.storageBackendByID(c)
	if !ok {
		return
	}

	if err := h.St.StorageBackends.Delete(backend.ID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no such backend")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the backend row could not be deleted")
		return
	}

	var orphans int64
	if err := h.St.DB.Model(&models.StorageJob{}).Where("backend_id = ?", backend.ID).
		Count(&orphans).Error; err != nil {
		orphans = -1
	}
	h.audit(c, models.AuditStorageDelete, http.StatusOK,
		AuditTarget{Type: "storage_backend", ID: backend.ID}, map[string]any{
			"name":          backend.Name,
			"kind":          backend.Kind,
			"orphaned_jobs": orphans,
		})
	c.JSON(http.StatusOK, gin.H{
		"deleted": backend.Name,
		// The rows stay on purpose: a queue with nowhere to send is a fact the
		// operator has to see, not a detail to clean up behind their back.
		"orphaned_jobs": orphans,
	})
}

// TestStorageBackend asks the tier whether it is there, with the credentials
// stored for it.
//
// POST /api/storage/backends/:id/test (admin)
//
// The answer is `{"ok":true}` or `{"ok":false,"error":"<name>","reason":"…"}`.
// The four names are the storage package's sentinel messages and nothing else is
// invented: a refusal the drivers have no name for answers `connection_failed`
// with the driver's own sentence in `reason`, which is where the fix is.
func (h *Handlers) TestStorageBackend(c *gin.Context) {
	backend, ok := h.storageBackendByID(c)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), tierProbeTimeout)
	defer cancel()

	b, err := h.openBackend(ctx, backend)
	if err == nil {
		err = b.Ping(ctx)
	}
	if err != nil {
		name := storage.ErrorCode(err)
		if name == "" {
			name = connectionFailed
		}
		h.audit(c, models.AuditStorageTest, http.StatusOK,
			AuditTarget{Type: "storage_backend", ID: backend.ID}, map[string]any{
				"kind": backend.Kind, "ok": false, "error": name,
			})
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": name, "reason": err.Error()})
		return
	}

	h.audit(c, models.AuditStorageTest, http.StatusOK,
		AuditTarget{Type: "storage_backend", ID: backend.ID},
		map[string]any{"kind": backend.Kind, "ok": true})
	c.JSON(http.StatusOK, gin.H{"ok": true, "kind": backend.Kind, "name": backend.Name})
}

// SetStorageBackendDefault makes one destination the one new objects go to.
//
// PUT /api/storage/backends/:id/default (admin)
func (h *Handlers) SetStorageBackendDefault(c *gin.Context) {
	backend, ok := h.storageBackendByID(c)
	if !ok {
		return
	}
	if err := h.St.StorageBackends.SetDefault(backend.ID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no such backend")
			return
		}
		if strings.Contains(err.Error(), "disabled") {
			// The store's rule, in the operator's words: the default is where the
			// queue puts things, and a disabled backend takes nothing.
			jsonError(c, http.StatusUnprocessableEntity, "a disabled backend cannot be the default")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the default flag could not be written")
		return
	}
	h.audit(c, models.AuditStorageDefault, http.StatusOK,
		AuditTarget{Type: "storage_backend", ID: backend.ID}, map[string]any{"name": backend.Name})
	c.JSON(http.StatusOK, gin.H{"default": backend.Name})
}

// SetStorageBackendEnabled stops or starts new claims.
//
// PUT /api/storage/backends/:id/enabled (admin)
//
// `false` is a pause, not a cancellation: the queue stops being drained and
// starts again when the flag returns. Jobs already claimed finish — the claim is
// the gate, and a worker that re-checked the flag mid-flight would strand the job
// in `uploading`.
func (h *Handlers) SetStorageBackendEnabled(c *gin.Context) {
	backend, ok := h.storageBackendByID(c)
	if !ok {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.BindJSON(&in); err != nil {
		jsonError(c, http.StatusBadRequest, "the body is not JSON this route understands")
		return
	}
	if in.Enabled == nil {
		jsonError(c, http.StatusUnprocessableEntity, `enabled is required: {"enabled":true|false}`)
		return
	}

	if err := h.St.StorageBackends.SetEnabled(backend.ID, *in.Enabled); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no such backend")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the enabled flag could not be written")
		return
	}

	fresh, err := h.St.StorageBackends.GetByID(backend.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the backend row is unreadable after the write")
		return
	}
	h.audit(c, models.AuditStorageEnable, http.StatusOK,
		AuditTarget{Type: "storage_backend", ID: backend.ID}, map[string]any{
			"name":           fresh.Name,
			"enabled":        fresh.Enabled,
			"enabled_before": backend.Enabled,
			"is_default":     fresh.IsDefault,
		})
	c.JSON(http.StatusOK, gin.H{
		"name":    fresh.Name,
		"enabled": fresh.Enabled,
		// The flag the operator just switched and the flag the queue reads: a
		// disabled *default* backend means new objects have nowhere to go, and
		// the console should be able to say that without a second request.
		"is_default": fresh.IsDefault,
	})
}

// ListStoragePrefixes is the Settings page's "verify this path": it asks the
// tier what is actually under the prefix the operator typed.
//
// POST /api/storage/backends/:id/prefixes?prefix= (admin)
//
// POST rather than GET because it reaches someone else's server, which is the
// same reason the connection test is a POST: it is an act with a cost, not a
// read of a row.
func (h *Handlers) ListStoragePrefixes(c *gin.Context) {
	backend, ok := h.storageBackendByID(c)
	if !ok {
		return
	}
	prefix := c.Query("prefix")

	ctx, cancel := context.WithTimeout(c.Request.Context(), tierProbeTimeout)
	defer cancel()

	b, err := h.openBackend(ctx, backend)
	if err == nil {
		var keys []string
		keys, err = b.List(ctx, prefix)
		if err == nil {
			truncated := len(keys) > prefixListMax
			if truncated {
				keys = keys[:prefixListMax]
			}
			h.audit(c, models.AuditStoragePrefix, http.StatusOK,
				AuditTarget{Type: "storage_backend", ID: backend.ID},
				map[string]any{"prefix": prefix, "keys": len(keys)})
			c.JSON(http.StatusOK, gin.H{
				"prefix": prefix, "keys": keys, "count": len(keys), "truncated": truncated,
			})
			return
		}
	}

	name := storage.ErrorCode(err)
	if name == "" {
		name = connectionFailed
	}
	h.audit(c, models.AuditStoragePrefix, http.StatusOK,
		AuditTarget{Type: "storage_backend", ID: backend.ID},
		map[string]any{"prefix": prefix, "ok": false, "error": name})
	c.JSON(http.StatusOK, gin.H{"ok": false, "prefix": prefix, "error": name, "reason": err.Error()})
}

// RequeueStorageBackend re-shelves every uploaded object of one kind onto this
// backend: the answer to "we moved the tier" that does not require re-recording
// anything.
//
// POST /api/storage/backends/:id/requeue?kind=segment|clip|snapshot (admin)
func (h *Handlers) RequeueStorageBackend(c *gin.Context) {
	backend, ok := h.storageBackendByID(c)
	if !ok {
		return
	}
	kind := strings.TrimSpace(c.Query("kind"))
	if !models.ValidJobKind(kind) {
		jsonError(c, http.StatusBadRequest, "kind must be segment, clip or snapshot")
		return
	}

	n, err := h.St.StorageJobs.Requeue(backend.ID, kind)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			jsonError(c, http.StatusNotFound, "no such backend")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the queue could not be re-shelved")
		return
	}
	h.audit(c, models.AuditStorageRequeue, http.StatusOK,
		AuditTarget{Type: "storage_backend", ID: backend.ID},
		map[string]any{"kind": kind, "requeued": n})
	c.JSON(http.StatusOK, gin.H{"requeued": n, "kind": kind, "backend": backend.Name})
}

// storageJobView is one queue row. `last_error` travels because the queue page
// is where a failed upload is discovered, and a row that says `failed` without
// saying why sends the operator to the logs.
type storageJobView struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Action      string `json:"action"`
	State       string `json:"state"`
	Target      string `json:"target"`
	RemoteKey   string `json:"remote_key"`
	BackendID   string `json:"backend_id"`
	BackendName string `json:"backend_name"`
	Attempts    int    `json:"attempts"`
	Bytes       int64  `json:"bytes"`
	LastError   string `json:"last_error"`
	NextAttempt string `json:"next_attempt_at"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// ListStorageQueue answers the upload queue, oldest first.
//
// GET /api/storage/queue?state=pending|uploading|uploaded|failed&limit=&offset=
// (admin)
//
// A `state` that is not one of the four answers 400 rather than an empty page:
// read as an empty queue it is a queue page that looks healthy while the
// operator typed the wrong word.
func (h *Handlers) ListStorageQueue(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state != "" && !models.ValidJobState(state) {
		jsonError(c, http.StatusBadRequest,
			"state must be pending, uploading, uploaded or failed")
		return
	}
	limit := queueListDefault
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			jsonError(c, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		if n > queueListMax {
			n = queueListMax
		}
		limit = n
	}
	offset := 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			jsonError(c, http.StatusBadRequest, "offset must be zero or a positive number")
			return
		}
		offset = n
	}

	jobs, err := h.St.StorageJobs.ListByState(state, limit, offset)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the queue is unreadable")
		return
	}
	// The same probe-the-next-page rule as the event timeline: the store clamps
	// its limit, so asking for one more row than the page would silently answer
	// `truncated: false` at exactly the ceiling.
	truncated := false
	if len(jobs) == limit {
		more, err := h.St.StorageJobs.ListByState(state, 1, offset+limit)
		if err != nil {
			jsonError(c, http.StatusInternalServerError, "the queue is unreadable")
			return
		}
		truncated = len(more) > 0
	}

	names, err := h.backendNames()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the storage backends are unreadable")
		return
	}
	counts, err := h.St.StorageJobs.Counts("")
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the queue counts are unreadable")
		return
	}

	out := make([]storageJobView, 0, len(jobs))
	for i := range jobs {
		out = append(out, h.storageJobView(&jobs[i], names))
	}
	c.JSON(http.StatusOK, gin.H{
		"jobs":      out,
		"count":     len(out),
		"state":     state,
		"limit":     limit,
		"offset":    offset,
		"truncated": truncated,
		// The totals are for the whole queue, not the page: the queue page's
		// headline numbers are "what is stuck", and a page of 50 out of 4000
		// would otherwise report four.
		"counts": counts,
	})
}

func (h *Handlers) storageJobView(job *models.StorageJob, names map[string]string) storageJobView {
	next := ""
	if job.NextAttemptAt != nil {
		next = job.NextAttemptAt.UTC().Format(timeLayout)
	}
	return storageJobView{
		ID:          job.ID,
		Kind:        job.Kind,
		Action:      job.Action,
		State:       job.State,
		Target:      job.Target,
		RemoteKey:   job.RemoteKey,
		BackendID:   job.BackendID,
		BackendName: names[job.BackendID],
		Attempts:    job.Attempts,
		Bytes:       job.Bytes,
		LastError:   job.LastError,
		NextAttempt: next,
		CreatedAt:   job.CreatedAt.UTC().Format(timeLayout),
		UpdatedAt:   job.UpdatedAt.UTC().Format(timeLayout),
	}
}

// backendNames is the queue page's id→name map, in one read. A queue that
// answers bare uuids for the destination is a page the operator cannot read, and
// one read per page beats one per row.
func (h *Handlers) backendNames() (map[string]string, error) {
	backends, err := h.St.StorageBackends.ListAll()
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(backends))
	for i := range backends {
		names[backends[i].ID] = backends[i].Name
	}
	return names, nil
}

// storageBackendByID resolves `:id`. The id is used rather than the name because
// the name is editable, and a route that moved a backend by its name would stop
// working the moment the operator renamed the thing they were editing.
func (h *Handlers) storageBackendByID(c *gin.Context) (*models.StorageBackend, bool) {
	id := strings.TrimSpace(c.Param("id"))
	backend, err := h.St.StorageBackends.GetByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			jsonError(c, http.StatusNotFound, "no such backend")
			return nil, false
		}
		jsonError(c, http.StatusInternalServerError, "the storage backends are unreadable")
		return nil, false
	}
	return backend, true
}

// openBackend turns a row into a live destination. The seal key is the process's
// own; a row sealed with a different one fails here with `ErrBadKey`, which is
// deliberately not one of the four connection names — the server and the
// credential may both be fine, and only this process cannot open the row.
func (h *Handlers) openBackend(ctx context.Context, row *models.StorageBackend) (storage.Backend, error) {
	if h.sealErr != nil {
		return nil, fmt.Errorf("the storage seal key is unusable: %w", h.sealErr)
	}
	return storage.OpenWithSettings(ctx, row, h.sealKey, h.St.Settings)
}

// sealBackend seals the row's secret columns in place, so the store never sees
// plaintext and a database backup stays readable.
func (h *Handlers) sealBackend(row *models.StorageBackend) error {
	if h.sealErr != nil {
		return h.sealErr
	}
	var err error
	if row.Password, err = storage.Seal(row.Password, h.sealKey); err != nil {
		return fmt.Errorf("the password could not be sealed: %w", err)
	}
	if row.AccessKey, err = storage.Seal(row.AccessKey, h.sealKey); err != nil {
		return fmt.Errorf("the access key could not be sealed: %w", err)
	}
	if row.SecretKey, err = storage.Seal(row.SecretKey, h.sealKey); err != nil {
		return fmt.Errorf("the secret key could not be sealed: %w", err)
	}
	if row.PrivateKeyPath, err = storage.Seal(row.PrivateKeyPath, h.sealKey); err != nil {
		return fmt.Errorf("the private key path could not be sealed: %w", err)
	}
	return nil
}

// sealFailure answers the one storage write that can fail before it reaches the
// tier: the installation's own seal key is unusable, so no row can be written
// without losing its secrets.
func (h *Handlers) sealFailure(c *gin.Context, action string, row *models.StorageBackend, err error) {
	h.audit(c, action, http.StatusInternalServerError,
		AuditTarget{Type: "storage_backend", ID: row.Name}, map[string]any{"reason": err.Error()})
	jsonError(c, http.StatusInternalServerError, "this installation's storage key is unusable: "+err.Error())
}

// storageKindProblem is the presence check, per kind. The shape of an endpoint
// and the rules a prefix must obey belong to the driver, and the test route
// carries those answers.
func storageKindProblem(row *models.StorageBackend) string {
	name := strings.TrimSpace(row.Name)
	endpoint := strings.TrimSpace(row.Endpoint)
	root := strings.TrimSpace(row.RootPath)

	switch {
	case name == "":
		return "name is required"
	case !models.ValidStorageKind(row.Kind):
		return "kind must be one of s3, webdav, sftp, fs"
	case row.Kind == models.StorageKindFS:
		// Checked first, because `fs` is the one kind addressed only by a path:
		// it has no endpoint, and a rule that asks "does this kind need a root?"
		// before asking which kind it is would demand a host:port from a
		// directory. `KindNeedsRoot` is true for both fs and sftp precisely
		// because they both write under a root — it is not a statement about
		// being *addressed* by one.
		if root == "" {
			return "root_path is required for an fs backend"
		}
	case models.KindNeedsBucket(row.Kind):
		if endpoint == "" {
			return "endpoint is required for an s3 backend"
		}
		if strings.TrimSpace(row.Bucket) == "" {
			return "bucket is required for an s3 backend"
		}
	case row.Kind == models.StorageKindWebDAV:
		if endpoint == "" {
			return "endpoint is required for a webdav backend"
		}
	case models.KindNeedsRoot(row.Kind):
		// sftp: an endpoint to dial and a root to stay inside.
		if endpoint == "" {
			return fmt.Sprintf("endpoint is required for a %s backend", row.Kind)
		}
		if root == "" {
			return fmt.Sprintf("root_path is required for a %s backend", row.Kind)
		}
	}
	return ""
}

// secretWrite names which secret columns a payload addressed. It is the store's
// four seal flags in one value, because four bare booleans in a call are a
// mistake waiting to be reordered.
type secretWrite struct {
	password       bool
	accessKey      bool
	secretKey      bool
	privateKeyPath bool
}

// sealedColumns names which secret columns a write touched, for the ledger. The
// ledger records that a credential was addressed, never what it was — and it
// records the clears too, because "the password was removed" is exactly the fact
// a ledger row is for.
func sealedColumns(writes secretWrite) []string {
	var columns []string
	if writes.password {
		columns = append(columns, "password")
	}
	if writes.accessKey {
		columns = append(columns, "access_key")
	}
	if writes.secretKey {
		columns = append(columns, "secret_key")
	}
	if writes.privateKeyPath {
		columns = append(columns, "private_key_path")
	}
	return columns
}

// applyStorageInput merges the payload onto the row and reports which secret
// columns it addressed. A secret that arrives non-empty is copied onto the row
// for sealing; one that arrives empty clears the column; one that is absent
// leaves the stored value alone.
func applyStorageInput(row *models.StorageBackend, in *storageBackendInput) secretWrite {
	writes := secretWrite{}
	if in.Name != nil {
		row.Name = strings.TrimSpace(*in.Name)
	}
	if in.Kind != nil {
		row.Kind = strings.TrimSpace(*in.Kind)
	}
	if in.Endpoint != nil {
		row.Endpoint = strings.TrimSpace(*in.Endpoint)
	}
	if in.Region != nil {
		row.Region = strings.TrimSpace(*in.Region)
	}
	if in.Bucket != nil {
		row.Bucket = strings.TrimSpace(*in.Bucket)
	}
	if in.Prefix != nil {
		row.Prefix = strings.TrimSpace(*in.Prefix)
	}
	if in.Username != nil {
		row.Username = strings.TrimSpace(*in.Username)
	}
	if in.Password != nil {
		row.Password = strings.TrimSpace(*in.Password)
		writes.password = true
	}
	if in.AccessKey != nil {
		row.AccessKey = strings.TrimSpace(*in.AccessKey)
		writes.accessKey = true
	}
	if in.SecretKey != nil {
		row.SecretKey = strings.TrimSpace(*in.SecretKey)
		writes.secretKey = true
	}
	if in.PrivateKeyPath != nil {
		row.PrivateKeyPath = strings.TrimSpace(*in.PrivateKeyPath)
		writes.privateKeyPath = true
	}
	if in.InsecureTLS != nil {
		row.InsecureTLS = *in.InsecureTLS
	}
	if in.RootPath != nil {
		row.RootPath = strings.TrimSpace(*in.RootPath)
	}
	if in.Enabled != nil {
		row.Enabled = *in.Enabled
	}
	if in.IsDefault != nil {
		row.IsDefault = *in.IsDefault
	}
	return writes
}
