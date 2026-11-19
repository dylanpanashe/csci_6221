package websocket

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// A client whose queue is full gets messages dropped, then is disconnected,
// and the broadcast never blocks.
func TestSlowClientIsDropped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SendBuffer, cfg.MaxDrops = 1, 3
	h := NewHub(cfg, quiet, nil)
	slow := &client{remote: "test", send: make(chan []byte, 1)}
	fast := &client{remote: "fast", send: make(chan []byte, 100)}
	slow.flows.Store(true)
	fast.flows.Store(true)
	h.clients[slow] = struct{}{}
	h.clients[fast] = struct{}{}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			h.broadcast(campus.Envelope{Type: "occupancy", Data: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcast blocked on a slow client")
	}
	if _, still := h.clients[slow]; still {
		t.Fatal("slow client was not disconnected")
	}
	<-slow.send // the one message that fitted in its buffer
	if _, open := <-slow.send; open {
		t.Fatal("slow client's channel should be closed")
	}
	if len(fast.send) != 10 {
		t.Fatalf("fast client got %d of 10", len(fast.send))
	}
	if s := h.Stats(); s.Kicked != 1 || s.Dropped < 3 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestTopicsAndFlows(t *testing.T) {
	h := NewHub(DefaultConfig(), quiet, nil)
	c := &client{remote: "x", send: make(chan []byte, 10)}
	topics := map[string]bool{"frame": true}
	c.topics.Store(&topics)
	c.flows.Store(false)
	h.clients[c] = struct{}{}
	h.broadcast(campus.Envelope{Type: "wifi"})
	h.broadcast(campus.Envelope{Type: "frame", Data: campus.SimFrame{Seq: 1, Flows: []campus.MovementFlow{{Count: 50}}}})
	if len(c.send) != 1 {
		t.Fatalf("got %d messages, want only the frame", len(c.send))
	}
	if msg := string(<-c.send); strings.Contains(msg, `"count"`) {
		t.Fatalf("flows should be stripped: %s", msg)
	}
}

func TestServeSnapshotAndShutdown(t *testing.T) {
	h := NewHub(DefaultConfig(), quiet, func() []campus.Envelope {
		return []campus.Envelope{{Type: "snapshot-test"}}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Not running yet: refuse.
	if _, res, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil); err == nil || res.StatusCode != 503 {
		t.Fatalf("expected 503 before Run, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan campus.Envelope)
	stopped := make(chan struct{})
	go func() { h.Run(ctx, updates); close(stopped) }()
	for !h.running.Load() {
		time.Sleep(time.Millisecond)
	}

	conn, _, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var m campus.Envelope
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&m); err != nil || m.Type != "snapshot-test" {
		t.Fatalf("first message = %+v, %v", m, err)
	}
	updates <- campus.Envelope{Type: "occupancy"}
	if err := conn.ReadJSON(&m); err != nil || m.Type != "occupancy" {
		t.Fatalf("update = %+v, %v", m, err)
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("hub did not stop")
	}
	if _, _, err := conn.ReadMessage(); !gws.IsCloseError(err, gws.CloseGoingAway) {
		t.Fatalf("expected a going-away close frame, got %v", err)
	}
}

func TestCheckOrigin(t *testing.T) {
	h := NewHub(Config{AllowedOrigins: []string{"http://dev.local:3000"}}, quiet, nil)
	tests := []struct {
		origin string
		ok     bool
	}{
		{"", true},
		{"http://campus.test", true}, // same host as the request
		{"http://dev.local:3000", true},
		{"http://evil.example", false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "http://campus.test/ws", nil)
		if tt.origin != "" {
			r.Header.Set("Origin", tt.origin)
		}
		if got := h.checkOrigin(r); got != tt.ok {
			t.Errorf("origin %q: %v, want %v", tt.origin, got, tt.ok)
		}
	}
}
