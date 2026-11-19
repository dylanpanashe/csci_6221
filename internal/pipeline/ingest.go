package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/fn"
	"github.com/dylanpanashe/csci_6221/internal/privacy"
)

// Errors returned by the observation steps. Rejected observations are
// counted, not fatal: one malformed reading must not stop the pipeline.
var (
	ErrUnknownAP   = errors.New("unknown access point")
	ErrBadSignal   = errors.New("signal out of range")
	ErrNoDevice    = errors.New("missing device identifier")
	ErrNoTimestamp = errors.New("missing timestamp")
)

// ValidateStep checks an observation against the campus model. It is a
// closure over the (immutable) campus, returned as a fn.Step.
func ValidateStep(c *campus.Campus) fn.Step[campus.WiFiObservation] {
	return func(o campus.WiFiObservation) (campus.WiFiObservation, error) {
		if o.RawDeviceID == "" && o.DeviceHash.IsZero() {
			return o, ErrNoDevice
		}
		if o.At.IsZero() {
			return o, ErrNoTimestamp
		}
		if _, err := c.AccessPoint(o.AccessPoint); err != nil {
			return o, fmt.Errorf("%w: %s", ErrUnknownAP, o.AccessPoint)
		}
		if o.Signal < -110 || o.Signal > -10 {
			return o, fmt.Errorf("%w: %v", ErrBadSignal, o.Signal)
		}
		return o, nil
	}
}

// NormalizeStep tidies values: whole-dBm signal readings, and a location
// fallback to a room the AP actually serves when the location service gave
// an unknown or missing room.
func NormalizeStep(c *campus.Campus) fn.Step[campus.WiFiObservation] {
	return func(o campus.WiFiObservation) (campus.WiFiObservation, error) {
		o.Signal = campus.SignalStrength(math.Round(float64(o.Signal)))
		if _, err := c.Room(o.NearestRoom); err != nil {
			if served := c.RoomsServedBy(o.AccessPoint); len(served) > 0 {
				o.NearestRoom = served[0].ID
			}
		}
		return o, nil
	}
}

// PrivacyStep replaces the raw device ID with its daily keyed hash and
// wipes the raw value. After this step no raw identifier exists anywhere in
// the pipeline.
func PrivacyStep(h *privacy.Hasher) fn.Step[campus.WiFiObservation] {
	return func(o campus.WiFiObservation) (campus.WiFiObservation, error) {
		if o.RawDeviceID != "" {
			o.DeviceHash = h.Hash(o.RawDeviceID, o.At)
			o.RawDeviceID = ""
		}
		return o, nil
	}
}

// APObserved summarises what devices on one AP reported in one tick.
type APObserved struct {
	Devices   campus.DeviceCount    `json:"devices"`
	AvgSignal campus.SignalStrength `json:"avg_signal"`
	WeakShare float64               `json:"weak_share"` // share of devices below -75 dBm
}

// Partial is the result of processing one observation batch.
type Partial struct {
	Seq          uint64
	At           time.Time
	Index, Total int
	Rooms        map[campus.RoomID][]campus.DeviceHash
	APs          map[campus.AccessPointID]*apAcc
	Accepted     int
	Rejected     int
}

type apAcc struct {
	hashes    []campus.DeviceHash
	signalSum float64
	weak      int
}

// ProcessBatch is the worker-pool task: run every observation in the batch
// through the composed step chain and pre-aggregate the survivors. Doing
// the per-batch grouping in the workers keeps the single aggregator
// goroutine from becoming the bottleneck.
func ProcessBatch(chain fn.Step[campus.WiFiObservation]) func(context.Context, campus.ObservationBatch) (Partial, error) {
	return func(ctx context.Context, b campus.ObservationBatch) (Partial, error) {
		p := Partial{Seq: b.Seq, At: b.At, Index: b.Index, Total: b.Total,
			Rooms: make(map[campus.RoomID][]campus.DeviceHash, 16),
			APs:   make(map[campus.AccessPointID]*apAcc, 8)}
		for i := range b.Observations {
			o, err := chain(b.Observations[i])
			// Overwrite the input slot so the raw ID in the producer's
			// backing array is gone too.
			b.Observations[i] = o
			if err != nil {
				p.Rejected++
				continue
			}
			p.Accepted++
			p.Rooms[o.NearestRoom] = append(p.Rooms[o.NearestRoom], o.DeviceHash)
			a := p.APs[o.AccessPoint]
			if a == nil {
				a = &apAcc{}
				p.APs[o.AccessPoint] = a
			}
			a.hashes = append(a.hashes, o.DeviceHash)
			a.signalSum += float64(o.Signal)
			if o.Signal < -75 {
				a.weak++
			}
		}
		if err := ctx.Err(); err != nil {
			return p, err
		}
		return p, nil
	}
}
