package alerts

import (
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/diagnostics"
)

// fakeStore records calls; it satisfies campus.AlertWriter, which is all
// the manager depends on.
type fakeStore struct {
	open     map[string]bool
	raised   int
	resolved int
}

func (f *fakeStore) RaiseAlert(a campus.Alert) (campus.Alert, bool) {
	k := string(a.AccessPoint) + a.Kind.String()
	if f.open[k] {
		return a, false
	}
	f.open[k] = true
	f.raised++
	return a, true
}

func (f *fakeStore) ResolveAlerts(ap campus.AccessPointID, kind campus.DiagnosisKind, _ time.Time) int {
	k := string(ap) + kind.String()
	if !f.open[k] {
		return 0
	}
	delete(f.open, k)
	f.resolved++
	return 1
}

var _ campus.AlertWriter = (*fakeStore)(nil)

func TestRaiseDedupeAndResolveWithHysteresis(t *testing.T) {
	c := campus.NewGWUCampus()
	fs := &fakeStore{open: map[string]bool{}}
	m := NewManager(c, fs, 2)
	t0 := time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)
	failed := []diagnostics.APStatus{{AP: "bell-1-l", LastSeen: t0.Add(-4 * time.Minute),
		All: []campus.Diagnosis{{AccessPoint: "bell-1-l", Kind: campus.DiagnosisAPFailure, Severity: campus.SeverityCritical}}}}
	clean := []diagnostics.APStatus{{AP: "bell-1-l"}}

	ch := m.Process(failed, t0)
	if len(ch.Raised) != 1 {
		t.Fatalf("raised = %d", len(ch.Raised))
	}
	a := ch.Raised[0]
	if a.BuildingID != "bell" || a.Level != 1 || len(a.AffectedRooms) == 0 || a.DetectionDelay != 4*time.Minute {
		t.Errorf("alert = %+v", a)
	}
	if ch := m.Process(failed, t0.Add(2*time.Minute)); len(ch.Raised) != 0 {
		t.Error("duplicate alert raised")
	}
	if ch := m.Process(clean, t0.Add(4*time.Minute)); ch.Resolved != 0 {
		t.Error("resolved after one clean batch; hysteresis should wait for two")
	}
	if ch := m.Process(clean, t0.Add(6*time.Minute)); ch.Resolved != 1 {
		t.Errorf("resolved = %d after two clean batches", ch.Resolved)
	}
}
