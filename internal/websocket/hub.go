// Package websocket pushes live updates to browsers.
//
// Design:
//   - One Hub goroutine reads updates from the engine's event bus, encodes
//     each message once, and hands the bytes to every interested client.
//   - Each client has a buffered send channel and exactly one writer
//     goroutine (writePump). Gorilla connections do not allow concurrent
//     writes; giving each connection a single writer makes that impossible.
//   - The hub never blocks on a client. If a client's buffer is full the
//     message is dropped for that client only. A client that misses
//     MaxDrops messages in a row is disconnected; the browser reconnects
//     and gets a fresh snapshot.
//   - Clients can subscribe to topics and switch off movement flows, which
//     cuts the data the server sends them (used by the UI toggles).
package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// Config tunes the hub.
type Config struct {
	SendBuffer     int           // per-client queued messages
	MaxDrops       int           // consecutive drops before disconnecting
	PingInterval   time.Duration // keep-alive pings
	WriteTimeout   time.Duration
	AllowedOrigins []string // in addition to same-origin
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{SendBuffer: 64, MaxDrops: 20, PingInterval: 25 * time.Second, WriteTimeout: 5 * time.Second}
}

// Stats are hub counters for /api/metrics.
type Stats struct {
	Clients   int   `json:"clients"`
	Connected int64 `json:"connected_total"`
	Sent      int64 `json:"messages_sent"`
	Dropped   int64 `json:"messages_dropped"`
	Kicked    int64 `json:"slow_clients_disconnected"`
}

// Hub manages WebSocket clients.
type Hub struct {
	cfg      Config
	log      *slog.Logger
	snapshot func() []campus.Envelope
	upgrader gws.Upgrader

	mu      sync.Mutex // guards clients and each client's drop counter
	clients map[*client]struct{}
	running atomic.Bool
	writers sync.WaitGroup

	connected, sent, dropped, kicked atomic.Int64
}

type client struct {
	conn   *gws.Conn
	remote string
	send   chan []byte // closed exactly once, by whoever removes the client
	drops  int         // guarded by Hub.mu
	topics atomic.Pointer[map[string]bool]
	flows  atomic.Bool
}

// wants reports whether the client subscribed to a topic (nil = all).
func (c *client) wants(topic string) bool {
	t := c.topics.Load()
	return t == nil || (*t)[topic]
}

// NewHub creates a hub. snapshot is called for each new client so the page
// can draw immediately instead of waiting for the next tick.
func NewHub(cfg Config, log *slog.Logger, snapshot func() []campus.Envelope) *Hub {
	h := &Hub{cfg: cfg, log: log, snapshot: snapshot, clients: map[*client]struct{}{}}
	h.upgrader = gws.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 8192, CheckOrigin: h.checkOrigin}
	return h
}

func (h *Hub) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host || slices.Contains(h.cfg.AllowedOrigins, origin) || slices.Contains(h.cfg.AllowedOrigins, "*")
}

// Run broadcasts updates until the channel closes or ctx is cancelled, then
// disconnects every client and waits for their writers to finish.
func (h *Hub) Run(ctx context.Context, updates <-chan campus.Envelope) {
	h.running.Store(true)
	defer h.shutdown()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-updates:
			if !ok {
				return
			}
			h.broadcast(msg)
		}
	}
}

// broadcast encodes msg once (twice for frames: with and without flows)
// and queues the bytes for every interested client.
func (h *Hub) broadcast(msg campus.Envelope) {
	data, err := json.Marshal(msg)
	if err != nil {
		h.log.Error("encode update", "type", msg.Type, "err", err)
		return
	}
	var lite []byte // frame without flows, encoded only if someone needs it
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if !c.wants(msg.Type) {
			continue
		}
		payload := data
		if msg.Type == "frame" && !c.flows.Load() {
			if lite == nil {
				lite = withoutFlows(msg)
			}
			payload = lite
		}
		select {
		case c.send <- payload:
			c.drops = 0
			h.sent.Add(1)
		default:
			c.drops++
			h.dropped.Add(1)
			if c.drops >= h.cfg.MaxDrops {
				h.kicked.Add(1)
				h.log.Warn("disconnecting slow websocket client", "remote", c.remote)
				h.removeLocked(c)
			}
		}
	}
}

func withoutFlows(msg campus.Envelope) []byte {
	if f, ok := msg.Data.(campus.SimFrame); ok {
		f.Flows = nil
		msg.Data = f
	}
	b, _ := json.Marshal(msg)
	return b
}

// removeLocked unregisters c and closes its send channel, which tells its
// writer to send a close frame and exit. Caller holds h.mu. Because removal
// and closing happen together under the lock, the channel is closed once.
func (h *Hub) removeLocked(c *client) {
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
}

func (h *Hub) shutdown() {
	h.running.Store(false)
	h.mu.Lock()
	for c := range h.clients {
		h.removeLocked(c)
	}
	h.mu.Unlock()
	h.writers.Wait()
}

// ServeHTTP upgrades the request and runs the client until it disconnects.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.running.Load() {
		http.Error(w, "live updates are not running", http.StatusServiceUnavailable)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader has already replied
	}
	c := &client{conn: conn, remote: conn.RemoteAddr().String(), send: make(chan []byte, h.cfg.SendBuffer)}
	c.flows.Store(true)

	// Queue the snapshot before registering, so it arrives before updates.
	if h.snapshot != nil {
		for _, env := range h.snapshot() {
			if b, err := json.Marshal(env); err == nil {
				select {
				case c.send <- b:
				default:
				}
			}
		}
	}
	h.mu.Lock()
	if !h.running.Load() { // shut down while we were upgrading
		h.mu.Unlock()
		conn.Close()
		return
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.connected.Add(1)

	h.writers.Add(1)
	go func() {
		defer h.writers.Done()
		h.writePump(c)
	}()
	h.readPump(c) // blocks until the client goes away

	h.mu.Lock()
	h.removeLocked(c)
	h.mu.Unlock()
}

// writePump is the only goroutine that writes to c.conn.
func (h *Hub) writePump(c *client) {
	ping := time.NewTicker(h.cfg.PingInterval)
	defer func() {
		ping.Stop()
		c.conn.Close() // also unblocks readPump
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout))
			if !ok {
				_ = c.conn.WriteMessage(gws.CloseMessage,
					gws.FormatCloseMessage(gws.CloseGoingAway, "server shutting down or client too slow"))
				return
			}
			if err := c.conn.WriteMessage(gws.TextMessage, msg); err != nil {
				return
			}
		case <-ping.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout))
			if err := c.conn.WriteMessage(gws.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ClientMessage is what a browser may send.
//
//	{"type": "subscribe", "topics": ["occupancy", "wifi"]}
//	{"type": "prefs", "flows": false}
type ClientMessage struct {
	Type   string   `json:"type"`
	Topics []string `json:"topics,omitempty"`
	Flows  *bool    `json:"flows,omitempty"`
}

// readPump handles client messages and notices disconnects.
func (h *Hub) readPump(c *client) {
	c.conn.SetReadLimit(4096)
	deadline := func() { _ = c.conn.SetReadDeadline(time.Now().Add(2*h.cfg.PingInterval + 10*time.Second)) }
	deadline()
	c.conn.SetPongHandler(func(string) error { deadline(); return nil })
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if !gws.IsCloseError(err, gws.CloseNormalClosure, gws.CloseGoingAway) && !errors.Is(err, net.ErrClosed) {
				h.log.Debug("websocket read ended", "err", err)
			}
			return
		}
		deadline()
		var m ClientMessage
		if json.Unmarshal(data, &m) != nil {
			continue // ignore junk rather than drop the connection
		}
		switch m.Type {
		case "subscribe":
			set := make(map[string]bool, len(m.Topics))
			for _, t := range m.Topics {
				set[t] = true
			}
			if len(m.Topics) == 0 {
				set = nil
			}
			if set == nil {
				c.topics.Store(nil)
			} else {
				c.topics.Store(&set)
			}
		case "prefs":
			if m.Flows != nil {
				c.flows.Store(*m.Flows)
			}
		}
	}
}

// Stats returns hub counters.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	n := len(h.clients)
	h.mu.Unlock()
	return Stats{Clients: n, Connected: h.connected.Load(), Sent: h.sent.Load(), Dropped: h.dropped.Load(), Kicked: h.kicked.Load()}
}
