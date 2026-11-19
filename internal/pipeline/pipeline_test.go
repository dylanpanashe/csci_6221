package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/fn"
	"github.com/dylanpanashe/csci_6221/internal/privacy"
)

var (
	testCampus = campus.NewGWUCampus()
	quiet      = slog.New(slog.NewTextHandler(io.Discard, nil))
	t0         = time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)
)

func obs(dev string, ap campus.AccessPointID, room campus.RoomID, sig campus.SignalStrength) campus.WiFiObservation {
	return campus.WiFiObservation{RawDeviceID: dev, AccessPoint: ap, NearestRoom: room, Signal: sig, Connected: true, At: t0}
}

func TestValidateStep(t *testing.T) {
	v := ValidateStep(testCampus)
	tests := []struct {
		name string
		in   campus.WiFiObservation
		want error
	}{
		{"valid", obs("d1", "bell-1-w", "bell-102", -60), nil},
		{"unknown AP", obs("d1", "mars-1-w", "bell-102", -60), ErrUnknownAP},
		{"signal too strong", obs("d1", "bell-1-w", "bell-102", 5), ErrBadSignal},
		{"signal too weak", obs("d1", "bell-1-w", "bell-102", -130), ErrBadSignal},
		{"no device", obs("", "bell-1-w", "bell-102", -60), ErrNoDevice},
		{"no timestamp", func() campus.WiFiObservation {
			o := obs("d1", "bell-1-w", "bell-102", -60)
			o.At = time.Time{}
			return o
		}(), ErrNoTimestamp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v(tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestPrivacyStepRemovesRawID(t *testing.T) {
	h, _ := privacy.NewHasher("s", time.UTC)
	out, err := PrivacyStep(h)(obs("02:aa", "bell-1-w", "bell-102", -60))
	if err != nil || out.RawDeviceID != "" || out.DeviceHash.IsZero() {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestNormalizeFallsBackToServedRoom(t *testing.T) {
	out, _ := NormalizeStep(testCampus)(obs("d", "bell-1-w", "nowhere", -60.6))
	if out.NearestRoom == "nowhere" || out.Signal != -61 {
		t.Fatalf("out = %+v", out)
	}
}

// scriptSource is a test CampusSource that sends fixed batches then stops.
type scriptSource struct {
	batches []campus.ObservationBatch
	tel     []campus.TelemetryBatch
	block   bool // keep running until cancelled
}

func (s scriptSource) Run(ctx context.Context, out campus.Feeds) error {
	for _, b := range s.batches {
		select {
		case out.Observations <- b:
		case <-ctx.Done():
			return nil
		}
	}
	for _, b := range s.tel {
		select {
		case out.Telemetry <- b:
		case <-ctx.Done():
			return nil
		}
	}
	if s.block {
		<-ctx.Done()
	}
	return nil
}

var _ campus.CampusSource = scriptSource{}

func newTestPipeline(t testing.TB, workers int) *Pipeline {
	t.Helper()
	h, _ := privacy.NewHasher("test", time.UTC)
	p, err := New(Config{Workers: workers, Buffer: 16}, testCampus, h, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func drain(o Outputs) (ticks []AggregatedTick) {
	tel, frames, errs := o.Telemetry, o.Frames, o.SourceErr
	aggs := o.Aggregates
	for aggs != nil || tel != nil || frames != nil || errs != nil {
		select {
		case t, ok := <-aggs:
			if !ok {
				aggs = nil
				continue
			}
			ticks = append(ticks, t)
		case _, ok := <-tel:
			if !ok {
				tel = nil
			}
		case _, ok := <-frames:
			if !ok {
				frames = nil
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
			}
		}
	}
	return ticks
}

func TestPipelineAggregatesAndDeduplicates(t *testing.T) {
	// One tick split over three batches, delivered to a 3-worker pool.
	// "phone" appears in two batches and must be counted once.
	b := func(i int, os ...campus.WiFiObservation) campus.ObservationBatch {
		return campus.ObservationBatch{Seq: 1, At: t0, Index: i, Total: 3, Observations: os}
	}
	src := scriptSource{batches: []campus.ObservationBatch{
		b(0, obs("phone", "bell-1-w", "bell-102", -58), obs("laptop", "bell-1-w", "bell-102", -61)),
		b(1, obs("phone", "bell-1-w", "bell-102", -59), obs("bad", "nope", "bell-102", -50)),
		b(2, obs("tablet", "bell-2-e", "bell-203", -84)),
	}}
	ticks := drain(newTestPipeline(t, 3).Start(context.Background(), src))
	if len(ticks) != 1 {
		t.Fatalf("ticks = %d, want 1", len(ticks))
	}
	tk := ticks[0]
	if tk.Rooms["bell-102"] != 2 {
		t.Errorf("bell-102 devices = %d, want 2 (phone de-duplicated)", tk.Rooms["bell-102"])
	}
	if tk.Rejected != 1 || tk.Accepted != 4 {
		t.Errorf("accepted=%d rejected=%d", tk.Accepted, tk.Rejected)
	}
	if ap := tk.APs["bell-2-e"]; ap.WeakShare != 1 || ap.AvgSignal != -84 {
		t.Errorf("dead-zone AP stats = %+v", ap)
	}
}

func TestPipelineShutsDownOnCancel(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	outs := newTestPipeline(t, 4).Start(ctx, scriptSource{block: true})
	done := make(chan struct{})
	go func() { drain(outs); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("outputs did not close after cancel")
	}
	time.Sleep(30 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines before=%d after=%d: leak", before, after)
	}
}

func TestChainRejectsAndContinues(t *testing.T) {
	h, _ := privacy.NewHasher("x", time.UTC)
	chain := fn.Chain(ValidateStep(testCampus), NormalizeStep(testCampus), PrivacyStep(h))
	p, _ := ProcessBatch(chain)(context.Background(), campus.ObservationBatch{Seq: 9, Total: 1,
		Observations: []campus.WiFiObservation{obs("", "bell-1-w", "bell-102", -60), obs("ok", "bell-1-w", "bell-102", -60)}})
	if p.Accepted != 1 || p.Rejected != 1 {
		t.Fatalf("%+v", p)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func benchBatch(n int, seq uint64) campus.ObservationBatch {
	rooms := testCampus.AllRooms()
	b := campus.ObservationBatch{Seq: seq, At: t0, Total: 1, Observations: make([]campus.WiFiObservation, n)}
	for i := range b.Observations {
		r := rooms[i%len(rooms)]
		b.Observations[i] = obs(fmt.Sprintf("02:%06x", i), r.PrimaryAP, r.ID, -60)
	}
	return b
}

// BenchmarkProcessBatch measures one worker: validate, normalise, hash and
// pre-aggregate a 512-observation batch.
func BenchmarkProcessBatch(b *testing.B) {
	h, _ := privacy.NewHasher("bench", time.UTC)
	task := ProcessBatch(fn.Chain(ValidateStep(testCampus), NormalizeStep(testCampus), PrivacyStep(h)))
	tmpl := benchBatch(512, 1)
	batch := tmpl
	batch.Observations = make([]campus.WiFiObservation, len(tmpl.Observations))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(batch.Observations, tmpl.Observations)
		if _, err := task(context.Background(), batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(512*b.N)/b.Elapsed().Seconds(), "obs/s")
}

// BenchmarkPipelineThroughput runs the whole ingest path (source, pool,
// aggregator) with one tick of 50 batches x 512 observations per iteration.
func BenchmarkPipelineThroughput(b *testing.B) {
	const perBatch, batches = 512, 50
	tmpl := make([]campus.ObservationBatch, batches)
	for i := range tmpl {
		tmpl[i] = benchBatch(perBatch, 0)
		tmpl[i].Index, tmpl[i].Total = i, batches
	}
	p := newTestPipeline(b, runtime.NumCPU())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		src := scriptSource{batches: make([]campus.ObservationBatch, batches)}
		for j := range tmpl {
			src.batches[j] = tmpl[j]
			src.batches[j].Seq = uint64(i + 1)
			src.batches[j].Observations = append([]campus.WiFiObservation(nil), tmpl[j].Observations...)
		}
		b.StartTimer()
		if ticks := drain(p.Start(context.Background(), src)); len(ticks) != 1 {
			b.Fatalf("ticks = %d", len(ticks))
		}
	}
	b.ReportMetric(float64(perBatch*batches*b.N)/b.Elapsed().Seconds(), "obs/s")
}
