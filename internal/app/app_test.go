package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/config"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestHealthAndCampusEndpoints(t *testing.T) {
	srv := httptest.NewServer(newTestApp(t).Handler())
	defer srv.Close()

	for _, path := range []string{"/api/health", "/api/campus"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, decode err %v", path, res.StatusCode, err)
		}
	}
}

func TestRunShutsDownOnCancel(t *testing.T) {
	a := newTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
