package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Middleware wraps a handler with extra behaviour. It is just a function
// from handler to handler: a higher-order function.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware so the first one listed is the outermost:
// Chain(a, b, c)(h) == a(b(c(h))).
func Chain(ms ...Middleware) Middleware {
	return func(h http.Handler) http.Handler {
		for i := len(ms) - 1; i >= 0; i-- {
			h = ms[i](h)
		}
		return h
	}
}

type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDFrom returns the request ID stored in ctx.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// RequestID gives every request an ID (or keeps the caller's X-Request-ID)
// and puts it in the context and the response headers.
func RequestID() Middleware {
	var counter atomic.Uint64
	prefix := make([]byte, 4)
	_, _ = rand.Read(prefix)
	p := hex.EncodeToString(prefix)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" || len(id) > 64 {
				id = fmt.Sprintf("%s-%06d", p, counter.Add(1))
			}
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
		})
	}
}

// statusRecorder captures the status code and size for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, which the
// WebSocket upgrade needs (it hijacks the connection).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Logging writes one structured log line per request. skip is a predicate,
// so noisy paths (static files, the WebSocket) can be left out.
func Logging(log *slog.Logger, skip func(*http.Request) bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip != nil && skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			level := slog.LevelDebug
			if rec.status >= 500 {
				level = slog.LevelError
			} else if rec.status >= 400 {
				level = slog.LevelWarn
			}
			log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
				"bytes", rec.bytes, "ms", float64(time.Since(start).Microseconds())/1000, "request_id", RequestIDFrom(r.Context()))
		})
	}
}

// Timing adds a Server-Timing header, which browser dev tools display.
// It wraps the writer so the header is set just before the body starts.
func Timing() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&timingWriter{ResponseWriter: w, start: time.Now()}, r)
		})
	}
}

type timingWriter struct {
	http.ResponseWriter
	start   time.Time
	written bool
}

func (t *timingWriter) WriteHeader(code int) {
	if !t.written {
		t.written = true
		t.Header().Set("Server-Timing", fmt.Sprintf("app;dur=%.2f", float64(time.Since(t.start).Microseconds())/1000))
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *timingWriter) Write(b []byte) (int, error) {
	if !t.written {
		t.WriteHeader(http.StatusOK)
	}
	return t.ResponseWriter.Write(b)
}

func (t *timingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// Recover turns a panic in a handler into a 500 response and a log entry.
// This is one of the few places recover is the right tool: one buggy
// request must not take the server down.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler { // the server's own signal; let it through
						panic(v)
					}
					log.Error("handler panicked", "panic", v, "path", r.URL.Path,
						"request_id", RequestIDFrom(r.Context()), "stack", string(debug.Stack()))
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprintf(w, `{"ok":false,"error":{"code":"internal","message":"internal server error"},"meta":{"request_id":%q}}`+"\n",
						RequestIDFrom(r.Context()))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// CORS allows the listed origins. The frontend is served by the same Go
// server, so in normal use no cross-origin request happens at all; this is
// for running a separate dev frontend.
func CORS(origins []string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (slices.Contains(origins, "*") || slices.Contains(origins, origin)) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Request-ID")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ValidateJSON rejects POST bodies that are not JSON or are too large.
func ValidateJSON(maxBytes int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/") {
				mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil || mt != "application/json" {
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusUnsupportedMediaType)
					fmt.Fprintf(w, `{"ok":false,"error":{"code":"unsupported_media_type","message":"Content-Type must be application/json"},"meta":{"request_id":%q}}`+"\n",
						RequestIDFrom(r.Context()))
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets conservative browser security headers.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("X-Frame-Options", "DENY")
			next.ServeHTTP(w, r)
		})
	}
}
