package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

// The paths a handler reaches when something is wrong: an id that is not there,
// a job whose backend was deleted, a queue page past the end, a column with
// malformed JSON in it. These are the answers the API turns into status codes,
// so they are asserted at the store, where they are decided.

func TestUpdatesNameTheRowThatIsNotThere(t *testing.T) {
	st := newTestStore(t)
	missing := "2f7a2a2a-0000-0000-0000-000000000000"

	// Every "PUT /api/v1/…/:id" whose id is not in the table answers 404. The
	// handlers read these answers, so every update path must distinguish "saved"
	// from "there was nothing to save".
	if err := st.Cameras.Update(&models.Camera{ID: missing, Slug: "ghost", Name: "Ghost"}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("camera update: %v", err)
	}
	if err := st.Cameras.UpdateZones(missing, "[]"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("zones update: %v", err)
	}
	if err := st.Users.Update(&models.User{ID: missing, Username: "ghost"}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("user update: %v", err)
	}
	if err := st.Users.SetPassword(missing, "argon2id$x"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("set password: %v", err)
	}
	if err := st.Webhooks.Update(&models.Webhook{ID: missing, Name: "ghost", URL: "https://x"}, false); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("webhook update: %v", err)
	}
	if err := st.SSOProviders.Update(&models.SSOProvider{ID: missing, Name: "ghost", ClientID: "c", Issuer: "https://x"}, false); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("provider update: %v", err)
	}
	if err := st.StorageBackends.Update(&models.StorageBackend{ID: missing, Name: "ghost", Kind: models.StorageKindFS}, false, false, false, false); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("backend update: %v", err)
	}
	if err := st.StorageBackends.SetEnabled(missing, false); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("set enabled: %v", err)
	}
	if err := st.Roles.UpdateRole(&models.Role{ID: missing, Name: "ghost"}); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("role update: %v", err)
	}
	if err := st.Sessions.Revoke(missing); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("session revoke: %v", err)
	}
	if err := st.StorageJobs.MarkUploaded(missing, "k", 1); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("job mark uploaded: %v", err)
	}
	if err := st.StorageJobs.MarkRetry(missing, "boom", time.Now().UTC()); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("job mark retry: %v", err)
	}
}

func TestRoleGradesNameTheThingThatIsMissing(t *testing.T) {
	st := newTestStore(t)
	user := &models.User{Username: "watcher", Email: "w@example", PasswordHash: "argon2id$x", Status: models.UserStatusActive}
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// The role editor's two failure modes: a role name that is not in the role
	// table, and a user id that is not in the user table. A payload naming a role
	// that does not exist must not half-apply.
	err := st.Users.SetRoles(user.ID, []string{models.RoleViewer, "auditor"})
	if err == nil {
		t.Fatal("a role name that does not exist must be refused")
	}
	roles, err := st.Users.RolesFor(user.ID)
	if err != nil {
		t.Fatalf("roles for user: %v", err)
	}
	if len(roles) != 0 {
		t.Fatalf("expected the refusal to apply nothing, got %v", roles)
	}

	if err := st.Users.SetRoles("2f7a2a2a-0000-0000-0000-000000000000", []string{models.RoleViewer}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for a user that is not there, got %v", err)
	}

	// An empty role list is a stated policy: the account is created without
	// roles, which the login handler answers "no roles".
	if err := st.Users.SetRoles(user.ID, nil); err != nil {
		t.Fatalf("clear roles: %v", err)
	}
	roles, err = st.Users.RolesFor(user.ID)
	if err != nil {
		t.Fatalf("roles for user: %v", err)
	}
	if len(roles) != 0 {
		t.Fatalf("expected no roles, got %v", roles)
	}
}

func TestQueuePagesPastTheEnd(t *testing.T) {
	st := newTestStore(t)
	backend := &models.StorageBackend{Name: "tier-one", Kind: models.StorageKindFS, RootPath: "/mnt/nas", Enabled: true}
	if err := st.StorageBackends.Create(backend); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	camera := seedCamera(t, st, "front-door")

	for i := 0; i < 5; i++ {
		seg := &models.Segment{
			CameraID: camera.ID, Generation: 1, Kind: models.SegmentKindContinuous,
			Path:    "/var/lib/nvr/segments/" + camera.Slug + "-1-" + itoa(i+1) + ".mp4",
			StartTS: minutesAgo(50 - 5*i), EndTS: minutesAgo(45 - 5*i), Seconds: 5, Bytes: 100,
		}
		if err := st.Segments.Create(seg); err != nil {
			t.Fatalf("create segment: %v", err)
		}
		if err := st.StorageJobs.Enqueue(JobInput{
			ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
			BackendID: backend.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// The queue page is `?state=&limit=&offset=`, and a page past the end is an
	// empty page, not an error: the console walks pages and stops when one comes
	// back empty.
	first, err := st.StorageJobs.ListByState(models.JobStatePending, 2, 0)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("expected a page of two, got %d", len(first))
	}
	last, err := st.StorageJobs.ListByState(models.JobStatePending, 2, 4)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(last) != 1 {
		t.Fatalf("expected the tail of the queue, got %d", len(last))
	}
	past, err := st.StorageJobs.ListByState(models.JobStatePending, 2, 20)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(past) != 0 {
		t.Fatalf("expected an empty page, got %d", len(past))
	}
	if _, err := st.StorageJobs.ListByState("shipped", 10, 0); err == nil {
		t.Fatal("a state that is not a job state must be refused, not read as an empty queue")
	}

	// The counts are the numbers the WS payload carries, and they are per backend.
	counts, err := st.StorageJobs.Counts(backend.ID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Pending != 5 || counts.Uploading != 0 || counts.Failed != 0 {
		t.Fatalf("expected 5 pending on this backend, got %+v", counts)
	}
	other, err := st.StorageJobs.Counts("2f7a2a2a-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if other.Pending != 0 {
		t.Fatalf("a backend with no rows reports nothing, got %+v", other)
	}
}

func TestQueueCountsReportTheLastFailure(t *testing.T) {
	st := newTestStore(t)
	backend := &models.StorageBackend{Name: "tier-one", Kind: models.StorageKindFS, RootPath: "/mnt/nas", Enabled: true}
	if err := st.StorageBackends.Create(backend); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	camera := seedCamera(t, st, "front-door")
	seg := &models.Segment{
		CameraID: camera.ID, Generation: 1, Kind: models.SegmentKindContinuous,
		Path: camera.Slug + "-1-1.mp4", StartTS: minutesAgo(10), EndTS: minutesAgo(5), Seconds: 5, Bytes: 100,
	}
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: backend.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// "Why is nothing landing" is answered by the queue's last error, which the
	// storage.queue payload carries.
	job, err := st.StorageJobs.ClaimNext(ctx(), "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.StorageJobs.MarkRetry(job.ID, "sftp: connection closed by remote host", time.Now().UTC()); err != nil {
		t.Fatalf("mark retry: %v", err)
	}
	counts, err := st.StorageJobs.Counts(backend.ID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Pending != 1 {
		t.Fatalf("a retry goes back to the queue, got %+v", counts)
	}
	if counts.LastError == "" {
		t.Fatal("expected the queue to carry the last failure")
	}
}

func TestClaimNextIsBlindToDisabledAndUnknownBackends(t *testing.T) {
	st := newTestStore(t)
	backend := &models.StorageBackend{Name: "tier-one", Kind: models.StorageKindFS, RootPath: "/mnt/nas", Enabled: true}
	if err := st.StorageBackends.Create(backend); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	camera := seedCamera(t, st, "front-door")

	seg := &models.Segment{
		CameraID: camera.ID, Generation: 1, Kind: models.SegmentKindContinuous,
		Path: camera.Slug + "-1-1.mp4", StartTS: minutesAgo(10), EndTS: minutesAgo(5), Seconds: 5, Bytes: 100,
	}
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: backend.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// `PUT /api/storage/backends/:id/enabled` with false stops new claims: the
	// queue is deliberately parked, and a worker that claims anyway would write
	// to the backend the operator just turned off.
	if err := st.StorageBackends.SetEnabled(backend.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := st.StorageJobs.ClaimNext(ctx(), "worker-1"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected an empty queue with the backend disabled, got %v", err)
	}
	if err := st.StorageBackends.SetEnabled(backend.ID, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if _, err := st.StorageJobs.ClaimNext(ctx(), "worker-1"); err != nil {
		t.Fatalf("expected the job to be claimable again, got %v", err)
	}
}

func TestUploadedJobStampsCarryTheRemoteKeyAndBytes(t *testing.T) {
	st := newTestStore(t)
	backend := &models.StorageBackend{Name: "tier-one", Kind: models.StorageKindFS, RootPath: "/mnt/nas", Enabled: true}
	if err := st.StorageBackends.Create(backend); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	camera := seedCamera(t, st, "front-door")
	seg := &models.Segment{
		CameraID: camera.ID, Generation: 1, Kind: models.SegmentKindContinuous,
		Path: camera.Slug + "-1-1.mp4", StartTS: minutesAgo(10), EndTS: minutesAgo(5), Seconds: 5, Bytes: 4096,
	}
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: backend.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	job, err := st.StorageJobs.ClaimNext(ctx(), "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.StorageJobs.MarkUploaded(job.ID, "nvr/front-door/2026/01/02/x.mp4", 4096); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}
	loaded, err := st.StorageJobs.GetByID(job.ID)
	if err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if loaded.State != models.JobStateUploaded || loaded.RemoteKey != "nvr/front-door/2026/01/02/x.mp4" {
		t.Fatalf("expected the landed job with its key, got %+v", loaded)
	}
	if loaded.Bytes != 4096 {
		t.Fatalf("the queue page reports the size that landed, got %d", loaded.Bytes)
	}
	if !loaded.UpdatedAt.After(job.UpdatedAt) {
		t.Fatal("the row is stamped when it lands: the sweeper prunes uploaded rows by age")
	}

	// The segment row carries the same key: the sweeper's remote delete reads
	// segments.remote_key, and a job that landed without stamping the row leaves
	// the object unreachable for deletion.
	if err := st.Segments.MarkUploaded(seg.ID, loaded.RemoteKey); err != nil {
		t.Fatalf("mark segment: %v", err)
	}
	reloaded, err := st.Segments.GetByID(seg.ID)
	if err != nil {
		t.Fatalf("reload segment: %v", err)
	}
	if !reloaded.Uploaded() || reloaded.RemoteKey != loaded.RemoteKey {
		t.Fatalf("expected the segment stamped with the same key, got %+v", reloaded)
	}
}

func TestEventsReadMalformedObjectColumnsAsNoObjects(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	event, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: "cam:person:porch", CameraID: camera.ID, Action: models.SignalActionOpen,
		Severity: 2, TS: minutesAgo(5),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A column written by hand, or by a release whose shape differed, must not
	// make the event list unreadable: the row is served without boxes.
	if err := st.DB.Model(&models.Event{}).Where("id = ?", event.ID).
		Update("objects_json", "not json at all").Error; err != nil {
		t.Fatalf("spoil the column: %v", err)
	}
	loaded, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(loaded.Objects()) != 0 {
		t.Fatal("expected no boxes from a column that is not JSON")
	}

	list, err := st.Events.List(EventFilter{CameraID: camera.ID}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected the row to be listable, got %d", len(list))
	}

	// A column of the wrong JSON shape (an object where an array belongs) is the
	// same case.
	if err := st.DB.Model(&models.Event{}).Where("id = ?", event.ID).
		Update("objects_json", `{"class":"person"}`).Error; err != nil {
		t.Fatalf("spoil the column: %v", err)
	}
	loaded, err = st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(loaded.Objects()) != 0 {
		t.Fatal("expected no boxes from a column of the wrong shape")
	}
}

func TestEventListPagingWalksTheWholeTable(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	for i := 0; i < 6; i++ {
		if _, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
			EventKey: "cam:person:z" + itoa(i), CameraID: camera.ID, Action: models.SignalActionOpen,
			Severity: 1 + i%3, TS: minutesAgo(60 - 10*i),
		}); err != nil {
			t.Fatalf("open: %v", err)
		}
	}

	seen := map[string]bool{}
	for offset := 0; ; offset += 2 {
		page, err := st.Events.List(EventFilter{CameraID: camera.ID, Limit: 2}, offset)
		if err != nil {
			t.Fatalf("list page %d: %v", offset, err)
		}
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			if seen[event.ID] {
				t.Fatalf("event %q appeared on two pages", event.ID)
			}
			seen[event.ID] = true
		}
		if offset > 20 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 6 {
		t.Fatalf("expected the walk to visit all six events, got %d", len(seen))
	}
}

func TestAuditStoreStandsAlone(t *testing.T) {
	// The sweeper prunes the ledger; NewAuditStore lets it be built and tested
	// over a database without constructing a whole installation.
	st := newTestStore(t)
	ledger := NewAuditStore(st.DB)

	if err := ledger.Record(&models.Audit{
		ActorUsername: "admin", Action: models.AuditCameraCreate,
		TargetType: "camera", TargetID: "cam-1", Outcome: models.AuditOutcomeOK,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err := ledger.List(AuditFilter{Action: models.AuditCameraCreate})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the row the same store wrote, got %d", len(rows))
	}
	count, err := ledger.Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1, got %d", count)
	}
	pruned, err := ledger.PruneOlderThan(time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected the row pruned by a cutoff in the future, got %d", pruned)
	}
}

func TestWebhookDeliveryListHonoursTheLimit(t *testing.T) {
	st := newTestStore(t)
	hook := &models.Webhook{Name: "oncall", URL: "https://hooks.example/nvr", Enabled: true}
	if err := st.Webhooks.Create(hook); err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := st.WebhookDeliveries.Create(&models.WebhookDelivery{
			WebhookID: hook.ID, Event: "event.close", Payload: "{}",
			Status: models.DeliveryStatusFailed, Attempt: i + 1, Error: "connection refused",
		}); err != nil {
			t.Fatalf("create delivery: %v", err)
		}
	}

	rows, err := st.WebhookDeliveries.ListForWebhook(hook.ID, 3)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected the panel's page of three, got %d", len(rows))
	}
	all, err := st.WebhookDeliveries.ListForWebhook(hook.ID, 0)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("expected every attempt kept, got %d", len(all))
	}
	if _, err := st.WebhookDeliveries.ListForWebhook("2f7a2a2a-0000-0000-0000-000000000000", 10); err != nil {
		t.Fatalf("an endpoint with no history has an empty history, not an error: %v", err)
	}
}

func TestWebhookCreateRefusesAnEndpointWithoutAName(t *testing.T) {
	st := newTestStore(t)

	// The unique name is what the settings page lists and what the delivery
	// history is read from; an endpoint without one cannot be shown.
	if err := st.Webhooks.Create(&models.Webhook{URL: "https://hooks.example/nvr", Enabled: true}); err == nil {
		t.Fatal("expected an endpoint with no name to be refused")
	}
	// A row whose name is taken is refused with the store's collision, so the
	// handler answers 409 rather than 500.
	named := &models.Webhook{Name: "oncall", URL: "https://hooks.example/nvr", Enabled: true}
	if err := st.Webhooks.Create(named); err != nil {
		t.Fatalf("create: %v", err)
	}
	dup := &models.Webhook{Name: "oncall", URL: "https://other.example", Enabled: true}
	if err := st.Webhooks.Create(dup); err == nil {
		t.Fatal("expected the unique name to be refused")
	}
}
