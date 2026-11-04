// S6-08's hook in this package: the assembly telling someone the row changed.
//
// The clip builder does not know about the live feed and must not: what the console
// receives is the event view, and the view belongs to the API. So the builder calls a
// callback with the finished row, and `main.go` wires the two together. These tests
// are about the callback — that it fires, with the row as it now reads, in both the
// case where a clip arrived and the case where none is coming.

package media

import (
	"context"
	"sync"
	"testing"
	"time"

	"nvr/server/models"
)

// notifySpy is the callback as a test sees it: the rows it was handed, in order.
type notifySpy struct {
	mu   sync.Mutex
	seen []*models.Event
}

func (s *notifySpy) hook() func(*models.Event) {
	return func(ev *models.Event) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.seen = append(s.seen, ev)
	}
}

func (s *notifySpy) calls() []*models.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*models.Event{}, s.seen...)
}

func TestClipAssemblyNotifiesTheLiveFeedWithTheFinishedRow(t *testing.T) {
	// The assembly lands on a timer, not in a request, so nothing would otherwise
	// tell a console that the clip it has been waiting for now exists.
	fx := newClipFixture(t)
	cam := fx.camera(t, "porch")
	start := fx.Clock.Now().Add(-30 * time.Minute)
	end := start.Add(20 * time.Second)
	fx.segment(t, cam.ID, cam.Slug, start, start.Add(5*time.Minute), models.SegmentKindContinuous)
	ev := fx.event(t, cam.ID, "k-notify", start, end, false)

	spy := &notifySpy{}
	fx.B.OnAssembled = spy.hook()

	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	calls := spy.calls()
	if len(calls) != 1 {
		t.Fatalf("the assembly notified %d time(s), want 1", len(calls))
	}
	row := calls[0]
	if row.ID != ev.ID {
		t.Errorf("the callback was handed event %q, want %q", row.ID, ev.ID)
	}
	// The row must carry the clip, not the state from before the write: the payload
	// the console receives is built from this, and `has_clip` is what lights up the
	// player.
	if row.LocalClipPath == "" {
		t.Error("the notified row has no clip path, so the console would be told nothing changed")
	}
	if row.ClipSeconds <= 0 {
		t.Errorf("the notified row carries %.2fs, want the clip's length", row.ClipSeconds)
	}

	// And the row on disk agrees: the callback is not the only thing that learned.
	stored, err := fx.St.Events.GetByID(ev.ID)
	if err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if stored.LocalClipPath != row.LocalClipPath || stored.ClipSeconds != row.ClipSeconds {
		t.Errorf("the notified row (%q, %.2fs) is not the row on disk (%q, %.2fs)",
			row.LocalClipPath, row.ClipSeconds, stored.LocalClipPath, stored.ClipSeconds)
	}
}

func TestClipAssemblyNotifiesTheLiveFeedWhenNoClipIsComing(t *testing.T) {
	// The empty-window case is an answer too. A console whose player has been
	// waiting on this event needs to hear "there will be no clip" as much as it
	// needs the clip itself, and without the notification it waits until someone
	// reloads the wall.
	fx := newClipFixture(t)
	cam := fx.camera(t, "porch")
	start := fx.Clock.Now().Add(-30 * time.Minute)
	end := start.Add(20 * time.Second)
	// No segments: the window is empty.
	ev := fx.event(t, cam.ID, "k-notify-empty", start, end, false)

	spy := &notifySpy{}
	fx.B.OnAssembled = spy.hook()

	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble over an empty window: %v", err)
	}

	calls := spy.calls()
	if len(calls) != 1 {
		t.Fatalf("the empty window notified %d time(s), want 1", len(calls))
	}
	if calls[0].LocalClipPath != "" {
		t.Errorf("the notified row names %q, want no clip path", calls[0].LocalClipPath)
	}
	if len(fx.Log.containing("clip skipped: no segments for window")) == 0 {
		t.Error("the empty-window log line is gone, and with it the operator's only explanation")
	}
}

func TestClipAssemblyWithoutACallbackStillFilesTheClip(t *testing.T) {
	// A builder with no callback is every test in this file that came before S6-08,
	// and a build with no live feed. The hook is optional, and its absence must not
	// change what lands on the row.
	fx := newClipFixture(t)
	cam := fx.camera(t, "porch")
	start := fx.Clock.Now().Add(-30 * time.Minute)
	end := start.Add(20 * time.Second)
	fx.segment(t, cam.ID, cam.Slug, start, start.Add(5*time.Minute), models.SegmentKindContinuous)
	ev := fx.event(t, cam.ID, "k-notify-nil", start, end, false)

	if err := fx.B.Assemble(context.Background(), ev.ID); err != nil {
		t.Fatalf("assemble with no callback: %v", err)
	}

	stored, err := fx.St.Events.GetByID(ev.ID)
	if err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if stored.LocalClipPath == "" {
		t.Error("the clip was not filed on the row when nobody was listening")
	}
}

func TestClipAssemblyDoesNotNotifyWhenTheRowIsGone(t *testing.T) {
	// An event deleted while its assembly was armed has no row to notify about, and
	// the assembly's own answer is a stated failure. A callback fired with a row
	// that never existed would put a card back on a wall that had just removed one.
	fx := newClipFixture(t)
	spy := &notifySpy{}
	fx.B.OnAssembled = spy.hook()

	if err := fx.B.Assemble(context.Background(), "00000000-0000-4000-8000-000000000000"); err == nil {
		t.Error("assembling an event that is not there reported success")
	}
	if calls := spy.calls(); len(calls) != 0 {
		t.Errorf("a failed assembly notified %d time(s), want none", len(calls))
	}
}
