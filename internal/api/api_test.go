package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(mk("a"), mk("b"), mk("c"))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "h") }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if strings.Join(order, "") != "abch" {
		t.Fatalf("order = %v", order)
	}
}

func TestRequestIDAndRecover(t *testing.T) {
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("bug") })
	h := Chain(RequestID(), Recover(quiet))(boom)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/x", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Fatalf("panic not converted: %d %s", rec.Code, rec.Body)
	}
	id := rec.Header().Get("X-Request-ID")
	if id == "" || !strings.Contains(rec.Body.String(), id) {
		t.Fatal("request id missing from header or body")
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	rec = httptest.NewRecorder()
	Chain(RequestID())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestIDFrom(r.Context()) != "abc-123" {
			t.Error("incoming request id not kept")
		}
	})).ServeHTTP(rec, req)
}

func TestCORS(t *testing.T) {
	h := CORS([]string{"http://ok.test"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	tests := []struct {
		method, origin string
		allow          bool
		code           int
	}{
		{"GET", "http://ok.test", true, 200},
		{"GET", "http://bad.test", false, 200},
		{"OPTIONS", "http://ok.test", true, 204},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(tt.method, "/api/x", nil)
		r.Header.Set("Origin", tt.origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		got := rec.Header().Get("Access-Control-Allow-Origin") != ""
		if got != tt.allow || rec.Code != tt.code {
			t.Errorf("%s %s: allow=%v code=%d", tt.method, tt.origin, got, rec.Code)
		}
	}
}

func TestTimingHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	Timing()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("x")) })).
		ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.HasPrefix(rec.Header().Get("Server-Timing"), "app;dur=") {
		t.Fatalf("header = %q", rec.Header().Get("Server-Timing"))
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		err  error
		code int
	}{
		{&campus.NotFoundError{Kind: "room", ID: "x"}, 404},
		{fmt.Errorf("wrapped: %w", &campus.ValidationError{Field: "f"}), 400},
		{fmt.Errorf("%w: junk", errBadRequest), 400},
		{fmt.Errorf("control: %w", simulator.ErrNotRunning), 503},
		{context.DeadlineExceeded, 504},
		{errors.New("disk on fire"), 500},
	}
	for _, tt := range tests {
		if got, _ := classify(tt.err); got != tt.code {
			t.Errorf("classify(%v) = %d, want %d", tt.err, got, tt.code)
		}
	}
}

// fakeSim records which control was called. It satisfies Simulation, which
// is all the control handler depends on.
type fakeSim struct {
	calls []string
	err   error
}

func (f *fakeSim) rec(name string) error        { f.calls = append(f.calls, name); return f.err }
func (f *fakeSim) Pause(context.Context) error  { return f.rec("pause") }
func (f *fakeSim) Resume(context.Context) error { return f.rec("resume") }
func (f *fakeSim) Reset(context.Context) error  { return f.rec("reset") }
func (f *fakeSim) SetSpeed(_ context.Context, x float64) error {
	return f.rec(fmt.Sprintf("speed:%v", x))
}
func (f *fakeSim) AdjustActivity(_ context.Context, d float64) error {
	return f.rec(fmt.Sprintf("activity:%v", d))
}
func (f *fakeSim) TriggerChangeover(context.Context) error { return f.rec("changeover") }
func (f *fakeSim) FailAP(_ context.Context, id campus.AccessPointID) error {
	return f.rec("fail:" + string(id))
}
func (f *fakeSim) RestoreAP(_ context.Context, id campus.AccessPointID) error {
	return f.rec("restore:" + string(id))
}
func (f *fakeSim) DemoScenario(context.Context) error { return f.rec("demo") }
func (f *fakeSim) Status() simulator.Status           { return simulator.Status{Running: true} }

var _ Simulation = (*fakeSim)(nil)

func TestControlDispatch(t *testing.T) {
	sim := &fakeSim{}
	s := New(Deps{Sim: sim, Log: quiet, Clock: fixedClock{}})
	h := s.Handler()
	send := func(body string) int {
		r := httptest.NewRequest("POST", "/api/simulation/control", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for _, body := range []string{`{"action":"pause"}`, `{"action":"speed","value":120}`, `{"action":"activity_down"}`,
		`{"action":"fail_ap","ap_id":"bell-1-l"}`, `{"action":"changeover"}`} {
		if code := send(body); code != 200 {
			t.Errorf("%s: %d", body, code)
		}
	}
	want := "pause speed:120 activity:-0.25 fail:bell-1-l changeover"
	if got := strings.Join(sim.calls, " "); got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	sim.err = simulator.ErrNotRunning
	if code := send(`{"action":"pause"}`); code != 503 {
		t.Fatalf("simulator down: %d, want 503", code)
	}
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 11, 18, 13, 40, 0, 0, time.UTC) }
