// Package alerts turns diagnoses into alerts a person should act on, and
// closes them again when the problem clears.
package alerts

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/diagnostics"
)

// alertKinds are the diagnosis kinds that produce alerts.
var alertKinds = []campus.DiagnosisKind{
	campus.DiagnosisAPFailure, campus.DiagnosisCongestion, campus.DiagnosisDeadZone, campus.DiagnosisDegraded,
}

// Change reports what one Process call did, for broadcasting.
type Change struct {
	Raised   []campus.Alert
	Resolved int
}

// Manager raises, de-duplicates and resolves alerts. It writes through the
// narrow campus.AlertWriter interface, so in tests it can be pointed at a
// fake store.
type Manager struct {
	campus  *campus.Campus
	store   campus.AlertWriter
	next    atomic.Uint64
	absent  map[key]int // consecutive batches a condition has been gone
	clearAt int         // batches of absence before resolving (hysteresis)
}

type key struct {
	ap   campus.AccessPointID
	kind campus.DiagnosisKind
}

// NewManager creates a manager. clearAfter is how many consecutive clean
// batches are needed before an alert resolves, which stops alerts flapping
// on and off around a threshold.
func NewManager(c *campus.Campus, store campus.AlertWriter, clearAfter int) *Manager {
	if clearAfter < 1 {
		clearAfter = 2
	}
	return &Manager{campus: c, store: store, absent: map[key]int{}, clearAt: clearAfter}
}

// Process updates alerts from one batch of AP statuses.
func (m *Manager) Process(statuses []diagnostics.APStatus, at time.Time) Change {
	var ch Change
	for _, st := range statuses {
		present := map[campus.DiagnosisKind]campus.Diagnosis{}
		for _, d := range st.All {
			present[d.Kind] = d
		}
		for _, kind := range alertKinds {
			k := key{st.AP, kind}
			d, ok := present[kind]
			if !ok {
				m.absent[k]++
				if m.absent[k] == m.clearAt {
					ch.Resolved += m.store.ResolveAlerts(st.AP, kind, at)
				}
				continue
			}
			m.absent[k] = 0
			if a, raised := m.store.RaiseAlert(m.build(st, d, at)); raised {
				ch.Raised = append(ch.Raised, a)
			}
		}
	}
	return ch
}

// Reset forgets hysteresis state and resolves everything open.
func (m *Manager) Reset(at time.Time) {
	clear(m.absent)
	for _, ap := range m.campus.AllAccessPoints() {
		for _, kind := range alertKinds {
			m.store.ResolveAlerts(ap.ID, kind, at)
		}
	}
}

func (m *Manager) build(st diagnostics.APStatus, d campus.Diagnosis, at time.Time) campus.Alert {
	ap, _ := m.campus.AccessPoint(st.AP)
	b, _ := m.campus.Building(ap.BuildingID)
	a := campus.Alert{
		ID:          campus.AlertID(fmt.Sprintf("A-%05d", m.next.Add(1))),
		Kind:        d.Kind,
		Severity:    d.Severity,
		Title:       title(d.Kind, ap, b),
		Detail:      d.Detail,
		BuildingID:  ap.BuildingID,
		Level:       ap.Level,
		AccessPoint: ap.ID,
		RaisedAt:    at,
	}
	for _, r := range m.campus.RoomsServedBy(ap.ID) {
		a.AffectedRooms = append(a.AffectedRooms, r.ID)
	}
	if d.Kind == campus.DiagnosisAPFailure {
		// How long after the AP's last heartbeat the failure was flagged.
		a.DetectionDelay = at.Sub(st.LastSeen)
		a.DetectionMinutes = a.DetectionDelay.Minutes()
	}
	return a
}

func title(kind campus.DiagnosisKind, ap *campus.AccessPoint, b *campus.Building) string {
	where := fmt.Sprintf("%s floor %d (%s)", b.Name, ap.Level, ap.Label)
	switch kind {
	case campus.DiagnosisAPFailure:
		return "Access point offline: " + where
	case campus.DiagnosisCongestion:
		return "Wi-Fi congestion: " + where
	case campus.DiagnosisDeadZone:
		return "Weak coverage (dead zone): " + where
	default:
		return "Degraded Wi-Fi performance: " + where
	}
}
