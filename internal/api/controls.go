package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// ControlRequest is the body of POST /api/simulation/control.
//
//	{"action": "pause"}
//	{"action": "speed", "value": 120}
//	{"action": "fail_ap", "ap_id": "bell-1-l"}
type ControlRequest struct {
	Action string               `json:"action"`
	Value  float64              `json:"value,omitempty"`
	AP     campus.AccessPointID `json:"ap_id,omitempty"`
}

// controlFunc runs one control action against the simulator.
type controlFunc func(ctx context.Context, sim Simulation, req ControlRequest) error

// controls maps action names to functions. Adding a control is one line
// here, with no switch statement to grow.
var controls = map[string]controlFunc{
	"pause":         func(ctx context.Context, s Simulation, _ ControlRequest) error { return s.Pause(ctx) },
	"resume":        func(ctx context.Context, s Simulation, _ ControlRequest) error { return s.Resume(ctx) },
	"reset":         func(ctx context.Context, s Simulation, _ ControlRequest) error { return s.Reset(ctx) },
	"speed":         func(ctx context.Context, s Simulation, r ControlRequest) error { return s.SetSpeed(ctx, r.Value) },
	"activity_up":   func(ctx context.Context, s Simulation, _ ControlRequest) error { return s.AdjustActivity(ctx, 0.25) },
	"activity_down": func(ctx context.Context, s Simulation, _ ControlRequest) error { return s.AdjustActivity(ctx, -0.25) },
	"changeover":    func(ctx context.Context, s Simulation, _ ControlRequest) error { return s.TriggerChangeover(ctx) },
	"demo":          func(ctx context.Context, s Simulation, _ ControlRequest) error { return s.DemoScenario(ctx) },
	"fail_ap": func(ctx context.Context, s Simulation, r ControlRequest) error {
		if r.AP == "" {
			return &campus.ValidationError{Field: "ap_id", Value: "", Reason: "required for fail_ap"}
		}
		return s.FailAP(ctx, r.AP)
	},
	"restore_ap": func(ctx context.Context, s Simulation, r ControlRequest) error {
		if r.AP == "" {
			return &campus.ValidationError{Field: "ap_id", Value: "", Reason: "required for restore_ap"}
		}
		return s.RestoreAP(ctx, r.AP)
	},
}

// ControlActions lists the supported action names.
func ControlActions() []string {
	names := make([]string, 0, len(controls))
	for n := range controls {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	var req ControlRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			err = fmt.Errorf("%w: body larger than %d bytes", errBadRequest, tooBig.Limit)
		} else {
			err = fmt.Errorf("%w: invalid JSON: %v", errBadRequest, err)
		}
		s.writeError(w, r, err)
		return
	}
	do, ok := controls[req.Action]
	if !ok {
		s.writeError(w, r, &campus.ValidationError{Field: "action", Value: req.Action,
			Reason: fmt.Sprintf("must be one of %v", ControlActions())})
		return
	}
	if req.Action == "speed" && (req.Value < 1 || req.Value > 3600) {
		s.writeError(w, r, &campus.ValidationError{Field: "value", Value: req.Value, Reason: "speed must be between 1 and 3600"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := do(ctx, s.deps.Sim, req); err != nil {
		s.writeError(w, r, fmt.Errorf("control %q: %w", req.Action, err))
		return
	}
	s.log.Info("simulation control", "action", req.Action, "value", req.Value, "ap", req.AP)
	s.writeOK(w, r, map[string]any{"action": req.Action, "applied": true, "status": s.deps.Sim.Status()})
}
