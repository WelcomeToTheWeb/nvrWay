package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"nvr/server/config"
	"nvr/server/store"
)

// The live feed is a route, and a route is only proven by the binary that
// serves it. P-12: the hub was never constructed in production — `newRouter`
// passed nil and every broadcast site guarded with `if h.Hub == nil` — so the
// fan-out was fully tested against fixture hubs while the shipped server
// silently had no `/ws` at all. This test pins the two facts the gap lacked:
// the process builds one hub, and the mount point mounts it.
func TestProductionRouterMountsTheLiveFeed(t *testing.T) {
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := st.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	cfg := &config.Config{JWTSecret: "router-test-secret", WebURL: "http://localhost:3000"}
	router, handlers := newRouter(context.Background(), cfg, st)
	if handlers.Hub == nil {
		t.Fatal("the production router must hand the process one hub (the recorder takes this same instance)")
	}

	// `/ws` without a token: the hub's token gate answers 401. A 404 is the
	// P-12 failure — the route was not mounted because the hub was nil.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatal("/ws answers 404: the live feed is not mounted in the production router")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/ws without a token: status = %d, want 401 from the hub's token gate", rec.Code)
	}
}
