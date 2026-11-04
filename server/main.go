package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // retention and event windows are computed in the operator's zone

	"nvr/server/config"
	"nvr/server/media"
	"nvr/server/models"
	"nvr/server/store"
	"nvr/server/webhooks"
)

// Boot order is the plan's (config → store → jwtAuth → sso →
// go2rtc → recorder → clip builder → storage → ws hub → api → sweeper); the
// packages after `store` arrive in Steps 2–7 and are inserted in this order,
// not appended at the end, because the order is the dependency graph:
//
//	Step 1  config.Load → store.New                     (this file)
//	Step 2  auth.NewJWTAuth → sso handlers → ws.NewHub
//	Step 3  go2rtc.NewClient → camera CRUD wiring
//	Step 4  media.NewRecorder
//	Step 6  media.NewClipBuilder → webhooks
//	Step 7  storage.OpenDefault → storage.NewUploader → sweeper.New
//	Step 2+ api.NewHandlers (mounts everything above)
//
// Every background worker takes `ctx`, cancelled at shutdown, and owns its
// process handles: ffmpeg children are closed on shutdown so a restart resumes
// recording instead of orphaning it.
func main() {
	cfg := config.Load()
	log.Printf("Starting nvrWay server %s (%s) on %s", config.Version, config.GitCommit, cfg.ServerPort)

	if err := cfg.EnsureDirs(); err != nil {
		log.Fatalf("Failed to create the media directories: %v", err)
	}

	st, err := store.New(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}
	if sqlDB, err := st.DB.DB(); err == nil {
		defer sqlDB.Close()
	}
	log.Println("Database connected and migrated")

	// The built-in roles and the break-glass administrator. This is the boot
	// that decides whether the installation has a way in, so the log says which
	// account it created and, when it created none, that is the state an operator
	// chose: the SSO-only installation, or an install with neither password nor
	// IdP, which the auth-mode summary below calls out by name.
	seeded, err := st.Bootstrap(cfg.BootstrapAdminUser, cfg.BootstrapAdminPassword)
	if err != nil {
		log.Fatalf("Failed to seed roles and the break-glass admin: %v", err)
	}
	if seeded {
		log.Printf("Seeded the break-glass admin account %q from BOOTSTRAP_ADMIN_PASSWORD (local account, admin role only); delete it once you have a way in you trust", cfg.BootstrapAdminUser)
	}

	// The recording and retention defaults, stated where an operator reads them.
	// "how long is my footage kept" and "is this camera recording" are questions
	// the log should answer at boot, and the answer is the *effective* default,
	// including the retention promise the sweeper will keep.
	log.Printf("Recording defaults: segment=%ds pre-roll=%ds post-roll=%ds event-max=%ds",
		cfg.SegmentSeconds, cfg.PreRollSeconds, cfg.PostRollSeconds, cfg.EventMaxSeconds)
	log.Printf("Retention defaults: local=%dd remote=%dd (0 = keep remote forever) events=%dd audit=%dd",
		cfg.LocalRetentionDays, cfg.RemoteRetentionDays, cfg.EventRetentionDays, models.AuditRetentionDays)
	log.Printf("Media tree: segments=%s clips=%s snapshots=%s", cfg.SegmentsDir, cfg.ClipsDir, cfg.SnapshotsDir)
	log.Printf("Detection: providers=%s (CPU is the verified baseline; CUDA needs the NVIDIA container toolkit on the host)",
		cfg.DetProviders)

	// How an operator gets in, stated as plainly as the recording policy: an
	// installation with neither a bootstrap password nor an OIDC provider has no
	// way in, and finding that out after the deploy is the worst time to learn it.
	switch {
	case cfg.LocalAdminEnabled() && cfg.OIDCConfigured():
		log.Printf("Auth: local admin %q (break-glass) + OIDC provider from OIDC_ISSUER", cfg.BootstrapAdminUser)
	case cfg.LocalAdminEnabled():
		log.Printf("Auth: local admin %q only (no OIDC_ISSUER set) — set OIDC_ISSUER to log in through your IdP", cfg.BootstrapAdminUser)
	case cfg.OIDCConfigured():
		log.Printf("Auth: OIDC provider from OIDC_ISSUER only — no local admin account will be created (BOOTSTRAP_ADMIN_PASSWORD is empty)")
	default:
		log.Printf("WARNING: Auth: no BOOTSTRAP_ADMIN_PASSWORD and no OIDC_ISSUER — this installation has no way in. Set one and restart.")
	}

	// The server lifecycle context: every background worker started with it
	// stops when shutdown cancels it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router, handlers := newRouter(ctx, cfg, st)

	// Step 3 (S3-05): the sidecar is probed, then the config rendered from the
	// camera rows is applied — through the same handler set the camera writes use,
	// so there is exactly one thing in this process that writes go2rtc's config.
	//
	// A failure is logged, not fatal. The sidecar being down is the most common
	// real-world state a deployment boots into (cameras on a PoE switch that is
	// still negotiating, a compose stack where go2rtc starts after the server), and
	// a server that refuses to start when that happens takes the API, the console
	// and the camera rows down with it — leaving the operator no way to see which
	// of the two is down. The line says what it found and what is running.
	if report, err := handlers.BootstrapGo2rtc(ctx); err != nil {
		log.Printf("WARNING: go2rtc at boot: %v (cameras keep their rows; the config is re-applied on the next camera save)", err)
	} else {
		log.Printf("go2rtc: %d streams, %d detection taps, config applied from %d active camera rows (%s)",
			report.Streams, report.Detected, report.Cameras, cfg.Go2rtcURL)
	}

	// Step 4 (S4-01): the recorder, inserted where the plan puts it in the boot
	// order — after the media sidecar it reads from, before the API that reports
	// what it wrote. The hub is nil for the same reason the router's is: the live
	// feed is wired to real media by S6-04, and a recorder with no hub records and
	// says nothing, which is a state and not a fault.
	//
	// Close is deferred so the ffmpeg children this process owns are terminated on
	// the way out. A child left writing is a recording a restart cannot resume.
	recorder := media.NewRecorder(cfg, st, nil)
	handlers.Recorder = recorder
	go recorder.Run(ctx)
	defer recorder.Close()
	log.Printf("Recorder: supervising %d camera row(s); segments are %ds; ffmpeg children are owned by this process",
		countCameraRows(st), cfg.SegmentSeconds)

	// Step 6 (S6-01): the clip assembler, in the plan's boot order — after the
	// recorder that writes the footage it cuts from, before the API that reports
	// what it produced. It takes the same lifecycle context: an assembly is a
	// short-lived ffmpeg, and a shutdown that cancels the context stops the cut
	// instead of leaving a child writing a file nobody filed.
	//
	// Nothing is awaited here. An event whose window has no footage is a clip that
	// never arrives, stated in the log and on the row, and a server does not stop
	// serving because a clip is missing.
	clips := media.NewClipBuilder(cfg, st)
	handlers.Clips = clips
	// The assembly is the one fact about a closed event that still moves, and it
	// lands on a timer rather than in a request, so the builder hands the finished
	// row to the live feed. Without this the console's player waits on a clip that
	// already exists until someone reloads the wall.
	clips.OnAssembled = handlers.BroadcastEventUpdate
	go clips.Run(ctx)
	defer clips.Close()
	log.Printf("Clip builder: assembling into %s, armed %ds after an event's last signal (pre-roll %ds, post-roll %ds, event max %ds)",
		cfg.ClipsDir, cfg.PostRollSeconds, cfg.PreRollSeconds, cfg.PostRollSeconds, cfg.EventMaxSeconds)

	// Step 6 (S6-06): the outbound notifier. It is one object owned by the process
	// because it owns the goroutines a close spawns and the context their retry
	// ladders run on — a per-request deliverer would mean a per-request ladder that
	// nothing can stop at shutdown.
	//
	// The base URL is the origin a *receiver* can dial. Left empty the payload
	// carries loopback URLs, which is worth a line at boot because it is the
	// configuration mistake that makes every webhook in the field look broken.
	notifier := webhooks.New(st, cfg.PublicBaseURL, sealKeyForNotifier(cfg))
	handlers.Webhooks = notifier
	if cfg.PublicBaseURL == "" {
		log.Println("Webhooks: PUBLIC_BASE_URL is unset, so the payload's snapshot and clip URLs point at http://localhost:8080")
	} else {
		log.Printf("Webhooks: delivering event closes signed, with the payload's URLs built from %s", cfg.PublicBaseURL)
	}
	defer func() {
		// A notification already in flight gets a grace to finish — the send that
		// reached the endpoint but not the database is the one an operator cannot
		// find out about. After that the ladder is cancelled: an endpoint that is
		// down should not add a minute to a shutdown the operator already asked for.
		done := make(chan struct{})
		go func() {
			notifier.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			log.Println("Webhooks: a delivery was still in flight; stopping its ladder")
		}
		notifier.Close()
	}()

	srv := &http.Server{
		Addr:    cfg.ServerPort,
		Handler: router,
		// Long timeouts: live view and clip streaming are long-lived responses,
		// and a write timeout on a stream reads to the operator as a broken
		// player. Liveness is the worker heartbeat and the WS hub's own reaper.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	log.Println("nvrWay server is running")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	received := <-quit
	log.Printf("Shutting down on %s...", received)

	cancel() // stop background workers
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}
	log.Println("nvrWay server exited")
}

// sealKeyForNotifier is the key that opens a webhook row's stored signing secret.
//
// `config.SealKey` cannot fail for a configuration the rest of the boot already
// accepted — an unparseable `NVR_SECRET_KEY` stops the storage routes too — but a
// failure here is not fatal and must not be silent: the notifier would send every
// delivery without a signature, and an operator wondering why the receiver rejects
// them should read the answer at boot rather than deduce it.
func sealKeyForNotifier(cfg *config.Config) [32]byte {
	key, err := cfg.SealKey()
	if err != nil {
		log.Printf("Webhooks: NVR_SECRET_KEY is unusable (%v); deliveries will be sent without a signature", err)
	}
	return key
}

// countCameraRows is the boot line's count of what the recorder will supervise.
// A store that is not answering reads as 0 here, which is the same honest answer
// the recorder's own poll gives: it says what it found, and the poll says more.
func countCameraRows(st *store.Store) int {
	rows, err := st.Cameras.ListAll()
	if err != nil {
		return 0
	}
	return len(rows)
}
