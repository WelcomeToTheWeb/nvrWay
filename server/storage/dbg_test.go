package storage

import (
	"context"
	"strings"
	"testing"
)

func TestDbgSaw(t *testing.T) {
	s := newWebdavTestServer(t, webdavTestUser, webdavTestPass)
	s.mkdir("/media")
	b := newWebdavBackend(t, s, "media", "")
	if err := b.Put(context.Background(), "nvr/contract/2026-11-04/front-door-0.mp4", strings.NewReader("x"), 1, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	want := "PUT " + webdavClean("/media/nvr/contract/2026-11-04/front-door-0.mp4")
	t.Logf("want %q", want)
	for _, got := range s.allRequests() {
		t.Logf("got  %q match=%v", got, got == want)
	}
}
