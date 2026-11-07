package main

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"nvr/server/api"
	"nvr/server/config"
	"nvr/server/go2rtc"
	"nvr/server/models"
	"nvr/server/sidecar"
	"nvr/server/store"
	"nvr/server/ws"
)

// newRouter builds the HTTP surface.
//
// `/health` is the container healthcheck's line and `/readyz` is the deploy
// script's: both exist from Step 1 and neither is part of the API's auth surface.
// Everything else is mounted by the handler set (`api.Handlers`), which is where
// each later step adds its routes, so the auth middleware, the rate limiter and
// the audit ledger apply to all of them. The route table is `docs/api.md`.
//
// `ctx` is the server lifecycle context: Steps 2–7 start their background
// workers (recorder, uploader, sweeper, clip builder) from it, and it is
// cancelled at shutdown, so no worker outlives the process that owns it.
func newRouter(ctx context.Context, cfg *config.Config, st *store.Store) (*gin.Engine, *api.Handlers) {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	// The container healthcheck (`wget -q --spider /health`) asks for liveness:
	// the process is up and answering. It deliberately does not touch the
	// database, so a healthcheck cannot be the thing that restarts a server out
	// from under a recording.
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":     "ok",
			"version":    config.Version,
			"git_commit": config.GitCommit,
		})
	})

	// Readiness is the question a deploy script asks, and it has an answer
	// here because the store is the only thing installed at this stage.
	// Step 9 extends this into `/api/healthz` with the detector heartbeat
	// ("fresh" / "stale") — detector liveness is data the server answers with,
	// not a port it probes.
	router.GET("/readyz", func(c *gin.Context) {
		dbOK := true
		if sqlDB, err := st.DB.DB(); err != nil || sqlDB.Ping() != nil {
			dbOK = false
		}
		cameras, detecting, _ := st.Cameras.Count()
		// The password way in is a fact about the accounts, not about the
		// environment: the env says what the last boot was asked to seed, the rows
		// say what the installation has. Reporting only the env is how "the console
		// shows a password form and no password works" becomes a mystery.
		localAccounts, _ := st.Users.CountLocalLogin()
		status := http.StatusOK
		body := gin.H{
			"status":            "ok",
			"database":          dbOK,
			"version":           config.Version,
			"git_commit":        config.GitCommit,
			"cameras":           cameras,
			"cameras_detecting": detecting,
			// The retention promise, reported by the process that has to keep it.
			"retention_days": gin.H{
				"local":  cfg.LocalRetentionDays,
				"remote": cfg.RemoteRetentionDays,
				"events": cfg.EventRetentionDays,
				"audit":  models.AuditRetentionDays,
			},
			"local_admin":            localAccounts > 0,
			"local_admin_seed_asked": cfg.LocalAdminEnabled(),
		}
		if !dbOK {
			status = http.StatusServiceUnavailable
			body["status"] = "database unreachable"
		}
		c.JSON(status, body)
	})

	// Step 2: the handler set. This is the mount point every later step adds its
	// routes to — cameras, events, clips, storage and the internal worker API all
	// arrive inside `api.Handlers`, so the middleware, the rate limiter and the
	// ledger are the same for all of them.
	//
	// The hub is built here, once, before `Register` — the `/ws` route exists only
	// when a hub is there at mount time, and the recorder (main.go) takes the
	// same instance from `handlers.Hub`: one fan-out per process, so a segment
	// tick and an event row travel the same socket the console reads. The origin
	// allowlist is WS_ORIGINS, falling back to WEB_URL: the console's own origin
	// is always dialable, and an installation that puts the console elsewhere
	// says so explicitly.
	handlers := api.NewHandlers(ctx, st, nil, nil, cfg)
	origins := cfg.WSOrigins
	if origins == "" {
		origins = cfg.WebURL
	}
	handlers.Hub = ws.NewHub(handlers.JWT, []string{origins})

	// Step 3: the media sidecar client. The camera routes need it for the probe
	// and the snapshot, and every camera write pushes the rendered config through
	// it — so it is built here, once, and the boot step (main.go) applies the
	// config through the same handler set, which is what keeps it a single write
	// path (S3-05).
	handlers.Go2rtc = go2rtc.NewClient(cfg)

	// Step 3 (S3-07): making a camera write live means recreating the sidecar,
	// because go2rtc instantiates its streams at startup. The restarter is built
	// next to the client so they point at the same sidecar, and an installation
	// with no compose project configured gets the Noop — the writes report that
	// the stream goes live at the sidecar's next boot, instead of shelling out at
	// a path nobody set.
	handlers.Restarter = sidecar.NewRestarter(cfg, handlers.Go2rtc)

	handlers.Register(router)

	return router, handlers
}
