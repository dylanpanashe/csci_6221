package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/dylanpanashe/csci_6221/internal/config"
)

// startApp runs the full application on a random port at high speed.
func startApp(t *testing.T) (base string, stop func()) {
	t.Helper()
	cfg := config.Default()
	cfg.TickInterval = 100 * time.Millisecond
	cfg.SimSpeed = 1200
	cfg.SimStartHour = 13
	cfg.Seed = 5
	cfg.WebDir = t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Addr = ln.Addr().String()
	ln.Close()
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	base = "http://" + cfg.Addr
	// Wait until estimates exist.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var env struct {
			Data struct {
				Summary struct {
					Occupancy struct {
						RoomID string `json:"room_id"`
					} `json:"occupancy"`
				} `json:"summary"`
			} `json:"data"`
		}
		if res, err := http.Get(base + "/api/campus"); err == nil {
			_ = json.NewDecoder(res.Body).Decode(&env)
			res.Body.Close()
			if env.Data.Summary.Occupancy.RoomID == "campus" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("app did not produce estimates")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return base, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("app did not shut down")
		}
	}
}

type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
	Meta struct {
		RequestID string `json:"request_id"`
	} `json:"meta"`
}

func get(t *testing.T, url string) (int, envelope) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var env envelope
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatalf("%s: decode: %v", url, err)
	}
	return res.StatusCode, env
}

func post(t *testing.T, url, body string) (int, envelope) {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var env envelope
	_ = json.NewDecoder(res.Body).Decode(&env)
	return res.StatusCode, env
}

func TestAPIEndpoints(t *testing.T) {
	base, stop := startApp(t)
	defer stop()

	ok := []string{
		"/api/health", "/api/campus", "/api/buildings", "/api/buildings/bell", "/api/buildings/seh/floors",
		"/api/rooms/bell-101", "/api/rooms?building=gelman&wifi=reliable", "/api/recommendations/quiet?minutes=60",
		"/api/wifi/access-points", "/api/wifi/access-points/bell-2-e", "/api/wifi/dead-zones", "/api/alerts?active=false",
		"/api/forecast", "/api/forecast?kind=building&subject=bell", "/api/changeovers", "/api/history", "/api/metrics",
		"/api/accuracy", "/api/simulation",
	}
	for _, path := range ok {
		code, env := get(t, base+path)
		if code != http.StatusOK || !env.OK || len(env.Data) == 0 || env.Meta.RequestID == "" {
			t.Errorf("%s: status %d ok=%v", path, code, env.OK)
		}
	}

	bad := map[string]int{
		"/api/buildings/nope":                  404,
		"/api/rooms/nope":                      404,
		"/api/nope":                            404,
		"/api/rooms?wifi=amazing":              400,
		"/api/rooms?level=two":                 400,
		"/api/forecast?kind=planet":            400,
		"/api/recommendations/quiet?minutes=5": 400,
	}
	for path, want := range bad {
		code, env := get(t, base+path)
		if code != want || env.OK || env.Error == nil {
			t.Errorf("%s: status %d, want %d (%+v)", path, code, want, env.Error)
		}
	}
}

func TestControls(t *testing.T) {
	base, stop := startApp(t)
	defer stop()
	url := base + "/api/simulation/control"

	if code, _ := post(t, url, `{"action":"pause"}`); code != 200 {
		t.Fatalf("pause: %d", code)
	}
	var st struct {
		Status struct {
			Paused bool    `json:"paused"`
			Speed  float64 `json:"speed"`
		} `json:"status"`
	}
	_, env := get(t, base+"/api/simulation")
	_ = json.Unmarshal(env.Data, &st)
	if !st.Status.Paused {
		t.Error("simulation not paused in the backend")
	}
	for _, body := range []string{`{"action":"resume"}`, `{"action":"speed","value":600}`, `{"action":"activity_up"}`,
		`{"action":"fail_ap","ap_id":"seh-2-w"}`, `{"action":"restore_ap","ap_id":"seh-2-w"}`, `{"action":"demo"}`} {
		if code, env := post(t, url, body); code != 200 {
			t.Errorf("%s: %d %+v", body, code, env.Error)
		}
	}
	_, env = get(t, base+"/api/simulation")
	_ = json.Unmarshal(env.Data, &st)
	if st.Status.Speed != 600 {
		t.Errorf("speed = %v", st.Status.Speed)
	}

	bad := map[string]int{
		`{"action":"explode"}`:                400,
		`{"action":"fail_ap"}`:                400,
		`{"action":"fail_ap","ap_id":"nope"}`: 404,
		`{"action":"speed","value":99999}`:    400,
		`not json`:                            400,
		`{"action":"pause","extra":1}`:        400,
	}
	for body, want := range bad {
		if code, _ := post(t, url, body); code != want {
			t.Errorf("%s: status %d, want %d", body, code, want)
		}
	}
	// Wrong content type is rejected by middleware.
	res, err := http.Post(url, "text/plain", bytes.NewBufferString(`{"action":"pause"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", res.StatusCode)
	}
}

func TestWebSocketClients(t *testing.T) {
	base, stop := startApp(t)
	defer stop()
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws"

	dial := func() *gws.Conn {
		c, _, err := gws.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := dial(), dial()
	defer a.Close()
	defer b.Close()

	// b only wants occupancy updates.
	if err := b.WriteJSON(map[string]any{"type": "subscribe", "topics": []string{"occupancy"}}); err != nil {
		t.Fatal(err)
	}
	read := func(c *gws.Conn, n int) map[string]int {
		seen := map[string]int{}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		for i := 0; i < n; i++ {
			var m struct {
				Type string `json:"type"`
			}
			if err := c.ReadJSON(&m); err != nil {
				t.Fatalf("read: %v", err)
			}
			seen[m.Type]++
		}
		return seen
	}
	got := read(a, 15)
	for _, k := range []string{"occupancy", "wifi", "frame"} {
		if got[k] == 0 {
			t.Errorf("client a never got %q (got %v)", k, got)
		}
	}
	got = read(b, 25)
	// b first receives its snapshot (all types), then only occupancy.
	if got["occupancy"] < 5 {
		t.Errorf("subscribed client got %v", got)
	}
}

func TestShutdownClosesWebSockets(t *testing.T) {
	base, stop := startApp(t)
	c, _, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	stop()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			if !gws.IsCloseError(err, gws.CloseGoingAway) {
				t.Logf("closed with %v", err)
			}
			return
		}
	}
}
