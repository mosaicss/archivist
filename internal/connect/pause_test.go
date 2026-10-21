package connect

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Story 94.6: the daemon hooks for Pause, the records it no longer keeps
// and the ones it prunes.

func TestDecodePauseIsExact(t *testing.T) {
	in, err := Decode(UserSocket, []byte(`{"kind":"pause"}`))
	if err != nil || in.Kind != "pause" {
		t.Fatalf("got %+v %v", in, err)
	}
	for _, raw := range []string{
		`{"kind":"pause","correlationId":"p-1"}`,
		`{"kind":"pause","sessionId":"0f8fad5b-d9cb-469f-a165-70867728950e"}`,
		`{"kind":"pause"} {}`,
	} {
		if _, err := Decode(UserSocket, []byte(raw)); err == nil {
			t.Errorf("%s decoded", raw)
		}
	}
	if _, err := Decode(SessionSocket, []byte(`{"kind":"pause"}`)); err == nil {
		t.Error("a session socket pause decoded")
	}
}

func TestFeaturesFrame(t *testing.T) {
	if got := string(featuresFrame(FeaturePause)); got != `{"kind":"features","features":["pause"]}` {
		t.Fatalf("got %s", got)
	}
	if got := string(featuresFrame()); got != `{"kind":"features","features":[]}` {
		t.Fatalf("got %s", got)
	}
}

func TestStoreRemove(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := randomUUID()
	if err := st.Save(&SessionRecord{SessionID: id, Status: "ended"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Remove(id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(id); !isNotExist(err) {
		t.Fatalf("record kept: %v", err)
	}
	if err := st.Remove(id); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if err := st.Remove("../x"); err == nil {
		t.Fatal("a path outside the store was accepted")
	}
}

// A daemon with OnPause declares the feature after its capabilities and
// calls OnPause on the relay's {kind:"pause"}; one without (the v0.2.34
// shape) declares nothing and ignores a pause.
func TestDaemonDeclaresPauseAndCallsOnPause(t *testing.T) {
	h := newHarness(t, nil)
	var paused atomic.Int32
	h.onPause = func() { paused.Add(1) }
	h.start()
	waitFor(t, 10*time.Second, "features frame", func() bool {
		var n int
		h.relay.set(func(r *fakeRelay) { n = len(r.features) })
		return n == 1
	})
	var frame map[string]any
	h.relay.set(func(r *fakeRelay) { frame = r.features[0] })
	if b, _ := json.Marshal(frame); string(b) != `{"features":["pause"],"kind":"features"}` {
		t.Fatalf("features frame %s", b)
	}
	h.relay.sendRaw("", `{"kind":"pause"}`)
	waitFor(t, 10*time.Second, "OnPause", func() bool { return paused.Load() == 1 })
	if !strings.Contains(h.log.String(), "paused from Mosaic") {
		t.Fatalf("log:\n%s", h.log.String())
	}
}

func TestDaemonWithoutOnPauseDeclaresNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.relay.sendRaw("", `{"kind":"pause"}`)
	waitFor(t, 10*time.Second, "pause ignored", func() bool {
		return strings.Contains(h.log.String(), "pause from the relay ignored")
	})
	var n int
	h.relay.set(func(r *fakeRelay) { n = len(r.features) })
	if n != 0 {
		t.Fatalf("%d features frames from a daemon without Pause", n)
	}
}

// An older relay refuses the features frame INVALID_FRAME and keeps the
// socket; the refusal is not taken for the controls frame's.
func TestDaemonOldRelayRefusesFeatures(t *testing.T) {
	h := newHarness(t, nil)
	h.onPause = func() {}
	h.relay.set(func(r *fakeRelay) { r.oldRelay = true })
	h.start()
	waitFor(t, 10*time.Second, "features refusal", func() bool {
		return strings.Contains(h.log.String(), "the relay refused the features frame")
	})
	waitFor(t, 10*time.Second, "controls refusal", func() bool {
		return strings.Contains(h.log.String(), "the relay refused the controls frame")
	})
	if strings.Count(h.log.String(), "relay error INVALID_FRAME") != 0 {
		t.Fatalf("an unattributed refusal:\n%s", h.log.String())
	}
}

// A start for a session chat-api does not know is refused and acked with no
// record written.
func TestDaemonUnknownSessionWritesNoRecord(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	unknown := randomUUID()
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": "c-unknown", "sessionId": unknown, "agent": "claude", "prompt": "hi"})
	waitFor(t, 10*time.Second, "unknown ack", func() bool { return h.relay.acked("c-unknown") })
	path := filepath.Join(h.home, ".archivist", "connect", "sessions", unknown+".json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record written for an unknown session: %v", err)
	}
	// Redelivered: refused the same way, still no record.
	h.relay.send("", map[string]any{"kind": "start_session", "correlationId": "c-unknown-2", "sessionId": unknown, "agent": "claude", "prompt": "hi"})
	waitFor(t, 10*time.Second, "second ack", func() bool { return h.relay.acked("c-unknown-2") })
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record written on redelivery: %v", err)
	}
}

// At startup, ended and failed records older than 7 days (from the later of
// creation and expiry) are removed; newer ones and unreadable ones stay.
func TestDaemonPrunesOldEndedRecords(t *testing.T) {
	h := newHarness(t, nil)
	st, err := OpenStore(filepath.Join(h.home, ".archivist", "connect"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour).UnixMilli()
	recent := now.Add(-6 * 24 * time.Hour).UnixMilli()
	ids := map[string]*SessionRecord{
		"oldEnded":     {Status: "ended", CreatedAt: old, ExpiresAt: old},
		"oldFailed":    {Status: "failed", CreatedAt: old},
		"oldExpired":   {Status: "active", CreatedAt: old, ExpiresAt: old},
		"recentEnded":  {Status: "ended", CreatedAt: old, ExpiresAt: recent},
		"recentFailed": {Status: "failed", CreatedAt: recent},
	}
	byName := map[string]string{}
	for name, rec := range ids {
		rec.SessionID = randomUUID()
		rec.Agent = "claude"
		byName[name] = rec.SessionID
		if err := st.Save(rec); err != nil {
			t.Fatal(err)
		}
	}
	corrupt := filepath.Join(h.home, ".archivist", "connect", "sessions", randomUUID()+".json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start()
	for name, id := range byName {
		_, err := st.Load(id)
		gone := isNotExist(err)
		if want := strings.HasPrefix(name, "old"); gone != want {
			t.Errorf("%s: removed %v, want %v (%v)", name, gone, want, err)
		}
	}
	if _, err := os.Stat(corrupt); err != nil {
		t.Fatalf("unreadable record removed: %v", err)
	}
}
