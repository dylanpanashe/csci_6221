package diagnostics

import (
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// Compile-time interface check.
var _ campus.DiagnosticEngine = (*Engine)(nil)

// Observed is what the Wi-Fi observations said about an AP in one tick.
type Observed struct {
	Devices   int
	AvgSignal float64
	WeakShare float64
}

// APStatus is the diagnostic result for one AP after a telemetry batch.
type APStatus struct {
	AP        campus.AccessPointID `json:"ap_id"`
	Health    campus.APHealth      `json:"health"`
	Quality   campus.WiFiQuality   `json:"quality"`
	Primary   campus.Diagnosis     `json:"diagnosis"`
	All       []campus.Diagnosis   `json:"diagnoses"`
	Telemetry campus.APTelemetry   `json:"telemetry"`
	Devices   int                  `json:"devices"`
	WeakShare float64              `json:"weak_share"`
	LastSeen  time.Time            `json:"last_seen"`
}

// sample pairs one telemetry reading with that tick's observations.
type sample struct {
	t   campus.APTelemetry
	obs Observed
}

// Engine keeps a short rolling window per AP so a single noisy reading
// does not raise an alert. One goroutine owns it.
type Engine struct {
	campus   *campus.Campus
	rules    []Rule
	window   int
	hist     map[campus.AccessPointID][]sample
	lastSeen map[campus.AccessPointID]time.Time
	observed map[campus.AccessPointID]Observed
	started  bool
}

// NewEngine creates a diagnostic engine with the given rules.
func NewEngine(c *campus.Campus, rules []Rule, window int) *Engine {
	if window < 1 {
		window = 3
	}
	return &Engine{campus: c, rules: rules, window: window,
		hist: map[campus.AccessPointID][]sample{}, lastSeen: map[campus.AccessPointID]time.Time{},
		observed: map[campus.AccessPointID]Observed{}}
}

// Observe records the latest per-AP observation summary.
func (e *Engine) Observe(o map[campus.AccessPointID]Observed) { e.observed = o }

// Diagnose implements campus.DiagnosticEngine for a single sample.
func (e *Engine) Diagnose(t campus.APTelemetry) []campus.Diagnosis {
	ap, err := e.campus.AccessPoint(t.AccessPoint)
	if err != nil {
		return nil
	}
	e.push(t)
	return Evaluate(e.rules, e.signals(ap, t.At))
}

func (e *Engine) push(t campus.APTelemetry) {
	h := append(e.hist[t.AccessPoint], sample{t: t, obs: e.observed[t.AccessPoint]})
	if len(h) > e.window {
		h = h[len(h)-e.window:]
	}
	e.hist[t.AccessPoint] = h
	e.lastSeen[t.AccessPoint] = t.At
}

// Reset forgets all history (used when the simulation resets).
func (e *Engine) Reset() {
	clear(e.hist)
	clear(e.lastSeen)
	e.started = false
}

// Process diagnoses every AP after a telemetry batch, including APs that
// sent nothing, which is how failures are noticed.
func (e *Engine) Process(b campus.TelemetryBatch) []APStatus {
	if !e.started {
		// Treat start-up as "last seen" for every AP, so an AP that is
		// already dead is still detected after the timeout.
		for _, ap := range e.campus.AllAccessPoints() {
			e.lastSeen[ap.ID] = b.At
		}
		e.started = true
	}
	fresh := map[campus.AccessPointID]campus.APTelemetry{}
	for _, t := range b.Samples {
		fresh[t.AccessPoint] = t
	}
	out := make([]APStatus, 0, len(e.lastSeen))
	for _, ap := range e.campus.AllAccessPoints() {
		var ds []campus.Diagnosis
		if t, ok := fresh[ap.ID]; ok {
			ds = e.Diagnose(t)
		} else {
			ds = Evaluate(e.rules, e.signals(ap, b.At))
		}
		worst := Worst(ap.ID, ds, b.At)
		sig := e.signals(ap, b.At)
		st := APStatus{AP: ap.ID, Primary: worst, All: ds, Health: HealthFor(worst.Kind),
			Quality: QualityFor(worst.Kind, sig.Signal, sig.Util), Devices: sig.Devices,
			WeakShare: sig.WeakShare, LastSeen: e.lastSeen[ap.ID]}
		if h := e.hist[ap.ID]; len(h) > 0 {
			st.Telemetry = h[len(h)-1].t
		}
		if sig.SilentFor > 0 {
			st.Telemetry.Online = false
		}
		out = append(out, st)
	}
	return out
}

// signals summarises the window for one AP. Silence is measured in
// simulated time against the batch time.
func (e *Engine) signals(ap *campus.AccessPoint, now time.Time) Signals {
	s := Signals{AP: ap, Now: now, LastSeenAt: e.lastSeen[ap.ID], Signal: float64(ap.BaseSignal)}
	if last, ok := e.lastSeen[ap.ID]; ok && now.After(last) {
		s.SilentFor = now.Sub(last)
	}
	h := e.hist[ap.ID]
	if len(h) == 0 {
		return s
	}
	var devW, sigW float64
	for _, x := range h {
		s.Util += float64(x.t.Utilization)
		s.Latency += float64(x.t.Latency)
		s.Loss += float64(x.t.PacketLoss)
		s.Clients += float64(x.t.Clients)
		w := float64(x.obs.Devices)
		devW += w
		s.WeakShare += x.obs.WeakShare * w
		sigW += x.obs.AvgSignal * w
	}
	n := float64(len(h))
	s.Samples = len(h)
	s.Util /= n
	s.Latency /= n
	s.Loss /= n
	s.Clients /= n
	if devW > 0 {
		s.WeakShare /= devW
		s.Signal = sigW / devW
	}
	s.Devices = h[len(h)-1].obs.Devices
	return s
}
