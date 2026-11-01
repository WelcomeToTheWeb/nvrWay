package main

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/store"
)

// newRouter builds the HTTP surface.
//
// Step 1 mounts only what exists: `/health` for the container healthcheck, and
// a readiness line that answers the one question a deployment can ask before
// auth, media and storage exist — is the database reachable and migrated.
//
// `ctx` is the server lifecycle context: Steps 2–7 start their background
// workers (recorder, uploader, sweeper, clip builder) from it, and it is
// cancelled at shutdown, so no worker outlives the process that owns it.
//
// Step 2 replaces this with `api.NewHandlers(ctx, st, jwtAuth, hub, cfg)`,
// which mounts the auth routes, the role middleware and the rate limiter, and
// Steps 3–7 add the camera, event, segment, storage and internal worker routes
// to that handler set. The route table is `docs/api.md`.
func newRouter(ctx context.Context, cfg *config.Config, st *store.Store) *gin.Engine {
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
			"local_admin": cfg.LocalAdminEnabled(),
		}
		if !dbOK {
			status = http.StatusServiceUnavailable
			body["status"] = "database unreachable"
		}
		c.JSON(status, body)
	})

	return router
}
