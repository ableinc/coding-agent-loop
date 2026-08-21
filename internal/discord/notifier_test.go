package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// stubWebhook captures POSTed bodies so tests can assert on the JSON a
// Notifier method actually sent, without hitting real Discord.
func stubWebhook(t *testing.T) (url string, bodies func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var captured [][]byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, buf)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	return srv.URL, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]byte, len(captured))
		copy(out, captured)
		return out
	}
}

func waitForCount(t *testing.T, bodies func() [][]byte, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := bodies(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d posted notification(s), got %d", n, len(bodies()))
	return nil
}

func TestRunClaimedPostsEmbed(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.RunClaimed("acme/widgets", 42, "run-1", 1)
	n.Close(2 * time.Second)

	got := waitForCount(t, bodies, 1)
	var payload webhookPayload
	if err := json.Unmarshal(got[0], &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(payload.Embeds))
	}
	e := payload.Embeds[0]
	if e.Title != "Run claimed: acme/widgets#42" {
		t.Errorf("unexpected title: %q", e.Title)
	}
	if e.Timestamp == "" {
		t.Error("timestamp should be set")
	}
	foundRunID := false
	for _, f := range e.Fields {
		if f.Name == "Run ID" && f.Value == "run-1" {
			foundRunID = true
		}
	}
	if !foundRunID {
		t.Errorf("expected a Run ID field, got %+v", e.Fields)
	}
}

func TestDisabledNotifierMakesNoRequests(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(false, url, nil)

	n.RunClaimed("acme/widgets", 1, "run-1", 1)
	n.Paused("testing")
	n.Close(200 * time.Millisecond)

	if got := bodies(); len(got) != 0 {
		t.Fatalf("disabled notifier made %d request(s)", len(got))
	}
}

func TestEmptyWebhookURLIsNoop(t *testing.T) {
	n := New(true, "", nil)
	// Must not panic and must not hang.
	done := make(chan struct{})
	go func() {
		n.RunClaimed("acme/widgets", 1, "run-1", 1)
		n.DaemonStarted("worker-1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("calls with an empty webhook URL should return immediately")
	}
}

func TestNilNotifierIsSafe(t *testing.T) {
	var n *Notifier
	// Every exported method must tolerate a nil receiver, since Options{}
	// literals in other packages' tests may not set Discord.
	n.RunClaimed("acme/widgets", 1, "run-1", 1)
	n.Paused("")
	n.Close(time.Millisecond)
}

func TestPostDoesNotBlockOnSlowWebhook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(true, srv.URL, nil)
	done := make(chan struct{})
	go func() {
		n.RunClaimed("acme/widgets", 1, "run-1", 1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RunClaimed should return immediately regardless of webhook latency")
	}
}

func TestCloseDrainsInFlight(t *testing.T) {
	url, bodies := stubWebhook(t)
	n := New(true, url, nil)

	n.Resumed()
	n.Close(2 * time.Second)

	if got := bodies(); len(got) != 1 {
		t.Fatalf("expected the in-flight post to land before Close returned, got %d", len(got))
	}
}
