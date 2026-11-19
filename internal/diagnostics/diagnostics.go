// Package diagnostics decides what is wrong with an access point, if
// anything. The point is to tell IT which fix applies:
//
//   - congestion: plenty of signal, too many devices. Add capacity.
//   - dead zone:  weak signal while the AP is lightly loaded. Move or add
//     an AP. Adding bandwidth will not help.
//   - failure:    the AP has stopped reporting. Send someone to it.
//
// Rules are registered as function values rather than written as one big
// switch statement. Each rule is a small, testable function that gets a
// summary of recent readings and says whether it applies.
package diagnostics

import (
	"fmt"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// Signals summarises recent readings for one AP. Rules only see this.
type Signals struct {
	AP         *campus.AccessPoint
	Now        time.Time
	Samples    int     // telemetry samples in the window
	Util       float64 // mean utilisation %
	Latency    float64 // mean ms
	Loss       float64 // mean packet loss %
	Clients    float64 // mean associated clients
	Signal     float64 // mean client signal, dBm (from observations)
	WeakShare  float64 // share of observed devices below -75 dBm
	Devices    int     // observed devices in the latest tick
	SilentFor  time.Duration
	LastSeenAt time.Time
}

// Finding is what a rule reports when it applies.
type Finding struct {
	Severity campus.Severity
	Detail   string
}

// CheckFunc is the signature every rule implements.
type CheckFunc func(Signals) (Finding, bool)

// Rule is a named check producing one kind of diagnosis.
type Rule struct {
	Name  string
	Kind  campus.DiagnosisKind
	Check CheckFunc
}

// Thresholds configures the default rules.
type Thresholds struct {
	FailureTimeout  time.Duration // silence before an AP counts as failed
	CongestionUtil  float64
	CongestionLat   float64
	CongestionLoss  float64
	DeadZoneWeak    float64 // weak-signal share
	DeadZoneMaxUtil float64
	DeadZoneMinDevs int
	DegradedLatency float64
	DegradedLoss    float64
}

// DefaultThresholds are documented in docs/DATA_PIPELINE.md.
var DefaultThresholds = Thresholds{
	FailureTimeout:  3 * time.Minute,
	CongestionUtil:  75,
	CongestionLat:   35,
	CongestionLoss:  1.5,
	DeadZoneWeak:    0.5,
	DeadZoneMaxUtil: 60,
	DeadZoneMinDevs: 4,
	DegradedLatency: 60,
	DegradedLoss:    3,
}

// The rule constructors below are closures over their thresholds.

// FailureRule fires when an AP has been silent longer than timeout.
func FailureRule(timeout time.Duration) Rule {
	return Rule{Name: "ap_silent", Kind: campus.DiagnosisAPFailure, Check: func(s Signals) (Finding, bool) {
		if s.SilentFor < timeout {
			return Finding{}, false
		}
		return Finding{campus.SeverityCritical, fmt.Sprintf("No telemetry for %s (last seen %s)",
			s.SilentFor.Round(time.Minute), s.LastSeenAt.Format("3:04 PM"))}, true
	}}
}

// CongestionRule fires on high utilisation that is hurting performance.
func CongestionRule(util, lat, loss float64) Rule {
	return Rule{Name: "high_utilisation", Kind: campus.DiagnosisCongestion, Check: func(s Signals) (Finding, bool) {
		if s.SilentFor > 0 || s.Util < util || (s.Latency < lat && s.Loss < loss) {
			return Finding{}, false
		}
		sev := campus.SeverityWarning
		if s.Util >= 92 {
			sev = campus.SeverityCritical
		}
		return Finding{sev, fmt.Sprintf("%.0f%% utilisation, %.0f ms latency, %.1f%% loss with %.0f clients",
			s.Util, s.Latency, s.Loss, s.Clients)}, true
	}}
}

// DeadZoneRule fires when most devices have weak signal although the AP
// has spare capacity: a coverage problem, not a load problem.
func DeadZoneRule(weak, maxUtil float64, minDevices int) Rule {
	return Rule{Name: "weak_coverage", Kind: campus.DiagnosisDeadZone, Check: func(s Signals) (Finding, bool) {
		if s.SilentFor > 0 || s.Devices < minDevices || s.WeakShare < weak || s.Util >= maxUtil {
			return Finding{}, false
		}
		return Finding{campus.SeverityWarning, fmt.Sprintf("%.0f%% of devices below -75 dBm (mean %.0f dBm) at only %.0f%% utilisation",
			s.WeakShare*100, s.Signal, s.Util)}, true
	}}
}

// DegradedRule catches poor performance that is neither load nor coverage,
// which usually means interference or a faulty radio.
func DegradedRule(lat, loss, maxUtil, weak float64) Rule {
	return Rule{Name: "poor_performance", Kind: campus.DiagnosisDegraded, Check: func(s Signals) (Finding, bool) {
		if s.SilentFor > 0 || s.Util >= maxUtil || s.WeakShare >= weak || (s.Latency < lat && s.Loss < loss) {
			return Finding{}, false
		}
		return Finding{campus.SeverityInfo, fmt.Sprintf("%.0f ms latency and %.1f%% loss without high load or weak signal", s.Latency, s.Loss)}, true
	}}
}

// DefaultRules builds the standard rule set from thresholds.
func DefaultRules(t Thresholds) []Rule {
	return []Rule{
		FailureRule(t.FailureTimeout),
		CongestionRule(t.CongestionUtil, t.CongestionLat, t.CongestionLoss),
		DeadZoneRule(t.DeadZoneWeak, t.DeadZoneMaxUtil, t.DeadZoneMinDevs),
		DegradedRule(t.DegradedLatency, t.DegradedLoss, t.DeadZoneMaxUtil, t.DeadZoneWeak),
	}
}

// Evaluate runs every rule against the signals. Pure function.
func Evaluate(rules []Rule, s Signals) []campus.Diagnosis {
	var out []campus.Diagnosis
	for _, r := range rules {
		if f, ok := r.Check(s); ok {
			out = append(out, campus.Diagnosis{AccessPoint: s.AP.ID, Kind: r.Kind, Severity: f.Severity, Rule: r.Name, Detail: f.Detail, At: s.Now})
		}
	}
	return out
}

// Worst returns the most important diagnosis, or a healthy one.
func Worst(ap campus.AccessPointID, ds []campus.Diagnosis, at time.Time) campus.Diagnosis {
	best := campus.Diagnosis{AccessPoint: ap, Kind: campus.DiagnosisHealthy, Severity: campus.SeverityInfo, At: at}
	for _, d := range ds {
		if best.Kind == campus.DiagnosisHealthy || d.Severity > best.Severity {
			best = d
		}
	}
	return best
}

// HealthFor maps a diagnosis to the AP health shown in the UI.
func HealthFor(kind campus.DiagnosisKind) campus.APHealth {
	switch kind {
	case campus.DiagnosisAPFailure:
		return campus.APOffline
	case campus.DiagnosisCongestion:
		return campus.APCongested
	case campus.DiagnosisDeadZone, campus.DiagnosisDegraded:
		return campus.APDegraded
	default:
		return campus.APHealthy
	}
}

// QualityFor turns an AP's state into the student-facing Wi-Fi rating for
// the rooms it serves.
func QualityFor(kind campus.DiagnosisKind, signal, util float64) campus.WiFiQuality {
	switch {
	case kind == campus.DiagnosisAPFailure:
		return campus.WiFiNone
	case kind == campus.DiagnosisDeadZone:
		return campus.WiFiPoor
	case kind == campus.DiagnosisCongestion, kind == campus.DiagnosisDegraded:
		return campus.WiFiFair
	case signal >= -62 && util < 60:
		return campus.WiFiExcellent
	case signal >= -72:
		return campus.WiFiGood
	default:
		return campus.WiFiFair
	}
}
