// The signal path's half of card S6-01.
//
// The internal API does not cut a clip. Its whole job is that something is armed
// to cut one, on every signal it applies — because the deadline moves with the
// event: an open event is armed at its ceiling, and the close is what replaces
// that with the post-roll deadline. A signal that was *refused* arms nothing:
// the event did not move, so the deadline did not.
//
// The assembler is a spy here. `media.ClipBuilder`'s own suite (card S6-01,
// `go test ./media -run Clip`) proves what arming does; this suite proves the
// call is made, by the route the worker actually posts to.

package api

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"nvr/server/models"
)

// clipSpy is the assembler as the signal path sees it: a list of the events it
// was asked to schedule, in the order the signals arrived, each with the state
// the event was in at that moment.
type clipSpy struct {
	mu        sync.Mutex
	scheduled []clipSpyCall
	cancelled []string
}

type clipSpyCall struct {
	ID      string
	Open    bool
	EndedAt time.Time
}

func (s *clipSpy) Schedule(ev *models.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev == nil {
		s.scheduled = append(s.scheduled, clipSpyCall{ID: "<nil>"})
		return
	}
	s.scheduled = append(s.scheduled, clipSpyCall{ID: ev.ID, Open: ev.Open, EndedAt: ev.EndedAt})
}

func (s *clipSpy) Cancel(eventID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = append(s.cancelled, eventID)
}

func (s *clipSpy) calls() []clipSpyCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]clipSpyCall{}, s.scheduled...)
}

func (s *clipSpy) cancelCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cancelled)
}

// TestClipAssemblyIsArmedByEveryAppliedSignal walks one event through open,
// extend and close over the route the worker posts to, and asserts the
// assembler heard each one — including the close, which is the signal that turns
// "someday, if this event ever ends" into "in POST_ROLL_SECONDS".
func TestClipAssemblyIsArmedByEveryAppliedSignal(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	spy := &clipSpy{}
	f.Handlers.Clips = spy

	key := "clip-wiring-1"
	for _, action := range []string{models.SignalActionOpen, models.SignalActionExtend, models.SignalActionClose} {
		s := signal(cam.ID, key, action)
		status, body, raw := postSignals(t, f, []map[string]any{s}, testWorkerToken)
		if status != http.StatusOK {
			t.Fatalf("%s signal: got %d %s, want 200", action, status, raw)
		}
		if got := repliedEvents(t, body)[0]["error"]; got != nil {
			t.Fatalf("%s signal was refused: %v", action, got)
		}
	}

	calls := spy.calls()
	if len(calls) != 3 {
		t.Fatalf("%d assembly(ies) armed for a 3-signal event, want 3: %+v", len(calls), calls)
	}
	eventID := calls[0].ID
	for i, c := range calls {
		if c.ID == "" || c.ID != eventID {
			t.Fatalf("call %d armed event %q, want the one event %q", i, c.ID, eventID)
		}
	}
	if !calls[0].Open || !calls[1].Open {
		t.Fatalf("open and extend armed an event that was still open: %+v", calls[:2])
	}
	if calls[2].Open {
		t.Fatalf("the close armed an event still marked open: %+v", calls[2])
	}
	if !calls[2].EndedAt.After(calls[0].EndedAt) {
		t.Fatalf("the close's deadline (%s) does not follow the open's (%s): the post-roll would be measured from the wrong instant",
			calls[2].EndedAt.Format(time.RFC3339), calls[0].EndedAt.Format(time.RFC3339))
	}
}

// TestClipAssemblyIsNotArmedForARefusedSignal: an unknown camera and a signal
// that arrives after the event closed are both refusals the worker cannot retry
// away, and in both cases no event row moved. Arming an assembly for them would
// spend an ffmpeg on an event that does not exist, or re-cut a clip whose window
// is already final.
func TestClipAssemblyIsNotArmedForARefusedSignal(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	spy := &clipSpy{}
	f.Handlers.Clips = spy

	// Unknown camera.
	unknown := signal("00000000-0000-0000-0000-000000000000", "clip-wiring-unknown", models.SignalActionOpen)
	status, body, raw := postSignals(t, f, []map[string]any{unknown}, testWorkerToken)
	if status != http.StatusOK {
		t.Fatalf("unknown-camera signal: got %d %s, want 200", status, raw)
	}
	if got := repliedEvents(t, body)[0]["error"]; got != "unknown camera" {
		t.Fatalf("unknown camera answered %v, want \"unknown camera\"", got)
	}
	if n := len(spy.calls()); n != 0 {
		t.Fatalf("a refused signal armed %d assembly(ies), want 0", n)
	}

	// Close, then a late extend on the same key.
	key := "clip-wiring-late"
	for _, action := range []string{models.SignalActionOpen, models.SignalActionClose} {
		if status, _, raw := postSignals(t, f, []map[string]any{signal(cam.ID, key, action)}, testWorkerToken); status != http.StatusOK {
			t.Fatalf("%s signal: got %d %s", action, status, raw)
		}
	}
	before := len(spy.calls())
	late := signal(cam.ID, key, models.SignalActionExtend)
	status, body, raw = postSignals(t, f, []map[string]any{late}, testWorkerToken)
	if status != http.StatusOK {
		t.Fatalf("late extend: got %d %s, want 200", status, raw)
	}
	if got := repliedEvents(t, body)[0]["error"]; got != "event closed" {
		t.Fatalf("late extend answered %v, want \"event closed\"", got)
	}
	if n := len(spy.calls()); n != before {
		t.Fatalf("a signal applied to a closed event armed %d more assembly(ies), want 0", n-before)
	}
}
