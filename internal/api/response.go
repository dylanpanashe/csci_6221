// Package api is the CampusPulse REST API. Handlers parse and validate the
// request, call a service or read the store, and write a response. Business
// logic lives in the domain packages, never here.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
)

// Envelope is the shape of every API response:
//
//	{"ok": true,  "data": {...}, "meta": {...}}
//	{"ok": false, "error": {"code": "not_found", "message": "..."}, "meta": {...}}
type Envelope struct {
	OK    bool       `json:"ok"`
	Data  any        `json:"data,omitempty"`
	Error *ErrorBody `json:"error,omitempty"`
	Meta  Meta       `json:"meta"`
}

// ErrorBody describes a failure.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Meta is attached to every response.
type Meta struct {
	RequestID string     `json:"request_id,omitempty"`
	SimTime   *time.Time `json:"sim_time,omitempty"`
}

// writeOK sends a 200 response with data.
func (s *Server) writeOK(w http.ResponseWriter, r *http.Request, data any) {
	s.write(w, r, http.StatusOK, Envelope{OK: true, Data: data})
}

// writeError maps an error to a status code with errors.Is and errors.As,
// so handlers just return whatever the domain layer gave them.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	if status >= 500 {
		s.log.Error("request failed", "path", r.URL.Path, "err", err, "request_id", RequestIDFrom(r.Context()))
	}
	s.write(w, r, status, Envelope{Error: &ErrorBody{Code: code, Message: err.Error()}})
}

func classify(err error) (int, string) {
	var ve *campus.ValidationError
	switch {
	case errors.Is(err, campus.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.As(err, &ve), errors.Is(err, campus.ErrInvalidInput), errors.Is(err, errBadRequest):
		return http.StatusBadRequest, "invalid_input"
	case errors.Is(err, simulator.ErrNotRunning):
		return http.StatusServiceUnavailable, "simulator_not_running"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout"
	case errors.Is(err, context.Canceled):
		return 499, "client_closed_request"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// errBadRequest marks malformed requests (bad JSON, bad query values).
var errBadRequest = errors.New("bad request")

func (s *Server) write(w http.ResponseWriter, r *http.Request, status int, env Envelope) {
	env.Meta.RequestID = RequestIDFrom(r.Context())
	if s.deps.Clock != nil {
		t := s.deps.Clock.Now()
		env.Meta.SimTime = &t
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(env); err != nil {
		// Headers are already sent, so all we can do is record it.
		s.log.Warn("encode response", "err", err, slog.String("path", r.URL.Path))
	}
}
