package storage

// The contract every driver is held to, in one place.
//
// A tier that passes its own driver's tests but not this list is a tier the
// uploader cannot use: the queue calls Put once, Get on the playback path,
// Stat before it reports an object to the console, Delete from the sweeper, and
// List from the Settings page's "verify this path". Those five behaviours are
// the same five on S3, WebDAV, SFTP and a directory, and the differences that
// are *not* the same (a WebDAV collection that has to be MKCOLed, an S3 bucket
// that has to exist) are asserted on the driver's own card.
//
// S7-03 introduces this file with the `fs` driver, and S7-04…S7-07 each run it
// against their own in-test server.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// contractObject is one object the contract puts, reads and removes.
type contractObject struct {
	key         string
	body        string
	contentType string
}

// assertBackendContract runs the shared expectations against a backend.
//
// `prefix` scopes the keys so two tests on one tier do not read each other's
// objects out of a List.
func assertBackendContract(t *testing.T, b Backend, prefix string) {
	t.Helper()
	ctx := context.Background()

	obj := contractObject{
		key:         prefix + "contract/2026/11/04/front-door-0.mp4",
		body:        "a short stand-in for an mp4: bytes that are not text",
		contentType: "video/mp4",
	}

	t.Run("put then get round-trips the bytes", func(t *testing.T) {
		if err := b.Put(ctx, obj.key, strings.NewReader(obj.body), int64(len(obj.body)), obj.contentType); err != nil {
			t.Fatalf("Put: %v", err)
		}
		rc, err := b.Get(ctx, obj.key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("reading the object: %v", err)
		}
		if string(got) != obj.body {
			t.Fatalf("Get read %d bytes %q, want %q", len(got), got, obj.body)
		}
	})

	t.Run("stat reports the size and the type it was given", func(t *testing.T) {
		info, err := b.Stat(ctx, obj.key)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != int64(len(obj.body)) {
			t.Fatalf("Stat size = %d, want %d", info.Size, len(obj.body))
		}
		if info.ContentType != obj.contentType {
			t.Fatalf("Stat content type = %q, want %q", info.ContentType, obj.contentType)
		}
		if info.Key == "" {
			t.Fatal("Stat returned an empty Key")
		}
		if info.ModTime.IsZero() {
			t.Fatal("Stat returned a zero ModTime")
		}
	})

	t.Run("put replaces what was there", func(t *testing.T) {
		replacement := obj.body + " and more bytes this time"
		if err := b.Put(ctx, obj.key, strings.NewReader(replacement), int64(len(replacement)), obj.contentType); err != nil {
			t.Fatalf("Put (replace): %v", err)
		}
		rc, err := b.Get(ctx, obj.key)
		if err != nil {
			t.Fatalf("Get after replace: %v", err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != replacement {
			t.Fatalf("Get after replace read %q, want %q", got, replacement)
		}
		info, err := b.Stat(ctx, obj.key)
		if err != nil {
			t.Fatalf("Stat after replace: %v", err)
		}
		if info.Size != int64(len(replacement)) {
			t.Fatalf("Stat size after replace = %d, want %d", info.Size, len(replacement))
		}
	})

	t.Run("list finds the key under its prefix", func(t *testing.T) {
		keys, err := b.List(ctx, prefix)
		if err != nil {
			t.Fatalf("List(%q): %v", prefix, err)
		}
		if !containsKey(keys, obj.key) {
			t.Fatalf("List(%q) = %v, want it to contain %q", prefix, keys, obj.key)
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, prefix) {
				t.Fatalf("List(%q) returned %q, which is outside the prefix", prefix, key)
			}
		}
	})

	t.Run("list of a prefix with nothing under it is empty, not an error", func(t *testing.T) {
		keys, err := b.List(ctx, prefix+"nothing-here-at-all/")
		if err != nil {
			t.Fatalf("List of an absent prefix: %v", err)
		}
		if len(keys) != 0 {
			t.Fatalf("List of an absent prefix = %v, want empty", keys)
		}
	})

	t.Run("delete removes it and a second delete says it is not there", func(t *testing.T) {
		if err := b.Delete(ctx, obj.key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := b.Get(ctx, obj.key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get after Delete: got %v, want ErrNotFound", err)
		}
		if _, err := b.Stat(ctx, obj.key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Stat after Delete: got %v, want ErrNotFound", err)
		}
		if err := b.Delete(ctx, obj.key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Delete of an absent object: got %v, want ErrNotFound", err)
		}
	})

	t.Run("get and stat of an object that never existed name the absence", func(t *testing.T) {
		missing := prefix + "contract/never-written.mp4"
		if _, err := b.Get(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get of an absent object: got %v, want ErrNotFound", err)
		}
		if _, err := b.Stat(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Stat of an absent object: got %v, want ErrNotFound", err)
		}
	})

	t.Run("a zero-byte object is an object", func(t *testing.T) {
		// A segment truncated by a crash is zero bytes, and the queue still has
		// to be able to shelve it: a driver that refuses an empty body leaves
		// the job retrying forever against a file that will never grow.
		empty := prefix + "contract/empty.mp4"
		if err := b.Put(ctx, empty, bytes.NewReader(nil), 0, "video/mp4"); err != nil {
			t.Fatalf("Put of an empty object: %v", err)
		}
		rc, err := b.Get(ctx, empty)
		if err != nil {
			t.Fatalf("Get of an empty object: %v", err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if len(got) != 0 {
			t.Fatalf("Get of an empty object read %q", got)
		}
		info, err := b.Stat(ctx, empty)
		if err != nil || info.Size != 0 {
			t.Fatalf("Stat of an empty object = %+v, %v", info, err)
		}
		if err := b.Delete(ctx, empty); err != nil {
			t.Fatalf("Delete of an empty object: %v", err)
		}
	})
}

func containsKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}
