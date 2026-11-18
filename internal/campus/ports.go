package campus

import (
	"context"
	"time"
)

// This file defines the behaviour-based interfaces the rest of the system is
// wired through. They are small on purpose (one to three methods), which is
// the Go convention: a type satisfies an interface just by having the methods,
// with no "implements" keyword, so the simulator, the real-time pipeline and
// the test fakes can all plug into the same slots.

// Clock tells the time. The simulator uses an accelerated clock; tests use a
// fixed one. Nothing in the pipeline calls time.Now directly.
type Clock interface {
	Now() time.Time
}

// ObservationSource produces Wi-Fi association records. Stream writes into a
// send-only channel and returns when ctx is cancelled. The caller owns the
// channel and closes it after Stream returns, so a source can never cause a
// send-on-closed-channel panic.
type ObservationSource interface {
	Stream(ctx context.Context, out chan<- WiFiObservation) error
}

// TelemetrySource produces periodic AP health samples, with the same
// ownership contract as ObservationSource.
type TelemetrySource interface {
	StreamTelemetry(ctx context.Context, out chan<- APTelemetry) error
}

// Timetable answers questions about scheduled events.
type Timetable interface {
	// EventsAt returns events running in a room at t.
	EventsAt(room RoomID, t time.Time) []TimetableEvent
	// Between returns all events overlapping [from, to), sorted by start.
	Between(from, to time.Time) []TimetableEvent
}

// EstimateInput is everything the estimator needs for one room at one moment.
// It is passed by value: the estimator cannot reach back into shared state.
type EstimateInput struct {
	Room           *Room
	Devices        DeviceCount      // de-duplicated devices on the room's AP(s)
	ActiveEvents   []TimetableEvent // what the timetable says is happening
	DevicesPerUser float64          // calibration factor
	At             time.Time
}

// OccupancyEstimator fuses timetable and Wi-Fi data into an estimate.
type OccupancyEstimator interface {
	Estimate(in EstimateInput) OccupancyEstimate
}

// ForecastRequest asks for a forecast for one subject.
type ForecastRequest struct {
	Subject  string // room, floor or building ID
	Kind     string // "room", "floor" or "building"
	Horizons []time.Duration
}

// ForecastEngine produces forecasts. It takes a context because a forecast
// may need to wait for fresh state and should give up if the caller does.
type ForecastEngine interface {
	Forecast(ctx context.Context, req ForecastRequest) (Forecast, error)
}

// DiagnosticEngine turns AP telemetry into diagnoses.
type DiagnosticEngine interface {
	Diagnose(t APTelemetry) []Diagnosis
}

// Envelope is the JSON message shape sent to browsers over WebSocket.
// Every message has the same outer structure so the frontend can switch on Type.
type Envelope struct {
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	Data any       `json:"data"`
}

// EventPublisher fans messages out to subscribers (the WebSocket hub, tests).
type EventPublisher interface {
	Publish(msg Envelope) error
}

// Repository interfaces are split into readers and writers and then composed.
// HTTP handlers only get the reader half, so they cannot write state by
// accident; the pipeline gets the full Repository.

type OccupancyReader interface {
	RoomEstimate(id RoomID) (OccupancyEstimate, error)
	RoomEstimates() []OccupancyEstimate
}

type OccupancyWriter interface {
	PutEstimates(estimates []OccupancyEstimate)
}

type AlertReader interface {
	Alerts(activeOnly bool) []Alert
}

type AlertWriter interface {
	RaiseAlert(a Alert) (Alert, bool) // false if an identical alert is already open
	ResolveAlerts(ap AccessPointID, kind DiagnosisKind, at time.Time) int
}

type TelemetryReader interface {
	Telemetry(id AccessPointID) (APTelemetry, error)
	AllTelemetry() []APTelemetry
}

type TelemetryWriter interface {
	PutTelemetry(t APTelemetry)
}

// ReadRepository is everything the API layer may read.
type ReadRepository interface {
	OccupancyReader
	AlertReader
	TelemetryReader
}

// Repository is the full read/write store used by the pipeline.
type Repository interface {
	ReadRepository
	OccupancyWriter
	AlertWriter
	TelemetryWriter
}
