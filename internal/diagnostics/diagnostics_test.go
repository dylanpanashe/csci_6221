package diagnostics

import (
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

var (
	c  = campus.NewGWUCampus()
	ap = c.AllAccessPoints()[0]
	t0 = time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)
)

// The core requirement: weak Wi-Fi is not automatically congestion.
func TestClassification(t *testing.T) {
	rules := DefaultRules(DefaultThresholds)
	tests := []struct {
		name string
		s    Signals
		want campus.DiagnosisKind
	}{
		{"healthy", Signals{Util: 35, Latency: 9, Loss: 0.1, Signal: -55, WeakShare: 0.02, Devices: 40}, campus.DiagnosisHealthy},
		{"congestion: busy AP, good signal", Signals{Util: 94, Latency: 120, Loss: 4, Signal: -57, WeakShare: 0.05, Devices: 380}, campus.DiagnosisCongestion},
		{"dead zone: weak signal, idle AP", Signals{Util: 12, Latency: 10, Loss: 3.8, Signal: -84, WeakShare: 0.95, Devices: 9}, campus.DiagnosisDeadZone},
		{"weak signal but too few devices to tell", Signals{Util: 8, Signal: -84, WeakShare: 1, Devices: 2}, campus.DiagnosisHealthy},
		{"busy AP with some weak clients is congestion, not dead zone", Signals{Util: 90, Latency: 80, Loss: 3, WeakShare: 0.6, Devices: 200}, campus.DiagnosisCongestion},
		{"silent AP is a failure", Signals{SilentFor: 4 * time.Minute, LastSeenAt: t0}, campus.DiagnosisAPFailure},
		{"brief silence is not yet a failure", Signals{SilentFor: 2 * time.Minute, LastSeenAt: t0}, campus.DiagnosisHealthy},
		{"slow without load or weak signal: degraded", Signals{Util: 30, Latency: 90, Loss: 0.5, WeakShare: 0.1, Devices: 20}, campus.DiagnosisDegraded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.s.AP, tt.s.Now = ap, t0
			got := Worst(ap.ID, Evaluate(rules, tt.s), t0)
			if got.Kind != tt.want {
				t.Fatalf("got %v (%s), want %v", got.Kind, got.Detail, tt.want)
			}
		})
	}
}

// Rules are function values, so adding one needs no change to the engine.
func TestCustomRule(t *testing.T) {
	r := Rule{Name: "always", Kind: campus.DiagnosisDegraded, Check: func(Signals) (Finding, bool) {
		return Finding{campus.SeverityInfo, "custom"}, true
	}}
	ds := Evaluate([]Rule{r}, Signals{AP: ap, Now: t0})
	if len(ds) != 1 || ds[0].Rule != "always" {
		t.Fatalf("%+v", ds)
	}
}

func TestEngineDetectsSilentAP(t *testing.T) {
	e := NewEngine(c, DefaultRules(DefaultThresholds), 3)
	all := c.AllAccessPoints()
	batch := func(at time.Time, skip campus.AccessPointID) campus.TelemetryBatch {
		b := campus.TelemetryBatch{At: at}
		for _, a := range all {
			if a.ID != skip {
				b.Samples = append(b.Samples, campus.APTelemetry{AccessPoint: a.ID, Utilization: 30, Latency: 8, Online: true, At: at})
			}
		}
		return b
	}
	e.Process(batch(t0, ""))
	victim := all[3].ID
	var found time.Time
	for i := 1; i <= 5; i++ {
		at := t0.Add(time.Duration(i*2) * time.Minute)
		for _, st := range e.Process(batch(at, victim)) {
			if st.AP == victim && st.Health == campus.APOffline && found.IsZero() {
				found = at
			}
			if st.AP != victim && st.Health != campus.APHealthy {
				t.Fatalf("healthy AP %s flagged %v", st.AP, st.Health)
			}
		}
	}
	if found.IsZero() {
		t.Fatal("silent AP never flagged")
	}
	if d := found.Sub(t0); d > 5*time.Minute {
		t.Fatalf("detected after %v; target is under 5 minutes", d)
	}
}

func TestQualityFor(t *testing.T) {
	tests := []struct {
		kind campus.DiagnosisKind
		sig  float64
		util float64
		want campus.WiFiQuality
	}{
		{campus.DiagnosisAPFailure, -55, 0, campus.WiFiNone},
		{campus.DiagnosisDeadZone, -84, 10, campus.WiFiPoor},
		{campus.DiagnosisCongestion, -55, 95, campus.WiFiFair},
		{campus.DiagnosisHealthy, -55, 30, campus.WiFiExcellent},
		{campus.DiagnosisHealthy, -68, 30, campus.WiFiGood},
	}
	for _, tt := range tests {
		if got := QualityFor(tt.kind, tt.sig, tt.util); got != tt.want {
			t.Errorf("QualityFor(%v, %v, %v) = %v, want %v", tt.kind, tt.sig, tt.util, got, tt.want)
		}
	}
}
