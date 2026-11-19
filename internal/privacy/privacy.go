// Package privacy keeps CampusPulse from turning into a tracking system.
//
//   - Device identifiers are replaced with a keyed hash (HMAC-SHA256) as the
//     first thing the pipeline does. The key rotates every day, so the same
//     phone gets an unrelated hash tomorrow and cannot be followed across days.
//   - Only group counts leave the backend. Any count under the threshold
//     (10 by default) is suppressed and published as "<10".
//   - Small random noise is added to published counts.
package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"hash"
	"math"
	mrand "math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// Hasher turns raw device IDs into daily-rotating keyed hashes. It is safe
// for concurrent use by every observation worker.
type Hasher struct {
	secret []byte
	loc    *time.Location
	// current holds the key for the day most recently seen. Swapping a
	// pointer atomically lets workers read the key without a mutex.
	current atomic.Pointer[dailyKey]
	mu      sync.Mutex // serialises key rotation only
}

type dailyKey struct {
	day  string
	pool sync.Pool // reusable HMAC states for this key; avoids an allocation per hash
}

// NewHasher creates a hasher. An empty secret gets a random one, which means
// hashes are not even stable across restarts.
func NewHasher(secret string, loc *time.Location) (*Hasher, error) {
	key := []byte(secret)
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("privacy: generate secret: %w", err)
		}
	}
	if loc == nil {
		loc = time.UTC
	}
	return &Hasher{secret: key, loc: loc}, nil
}

// keyFor returns the HMAC key material for a calendar day.
func (h *Hasher) keyFor(day string) []byte {
	m := hmac.New(sha256.New, h.secret)
	m.Write([]byte("campuspulse/device-key/" + day))
	return m.Sum(nil)
}

func (h *Hasher) dayKey(at time.Time) *dailyKey {
	day := at.In(h.loc).Format(time.DateOnly)
	if k := h.current.Load(); k != nil && k.day == day {
		return k // fast path: no lock
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if k := h.current.Load(); k != nil && k.day == day {
		return k
	}
	material := h.keyFor(day)
	k := &dailyKey{day: day}
	k.pool.New = func() any { return hmac.New(sha256.New, material) }
	h.current.Store(k)
	return k
}

// Hash returns the device hash for raw on the day of at.
func (h *Hasher) Hash(raw string, at time.Time) campus.DeviceHash {
	k := h.dayKey(at)
	m := k.pool.Get().(hash.Hash)
	defer k.pool.Put(m)
	m.Reset()
	m.Write([]byte(raw))
	var sum [sha256.Size]byte
	var out campus.DeviceHash
	copy(out[:], m.Sum(sum[:0]))
	return out
}

// ---------------------------------------------------------------------------
// Publication rules (pure functions)
// ---------------------------------------------------------------------------

// Suppress applies the small-group rule. Counts below threshold are hidden.
func Suppress(n campus.OccupancyCount, threshold int) campus.PublicCount {
	if int(n) < threshold {
		return campus.PublicCount{Suppressed: true, Display: "<" + strconv.Itoa(threshold)}
	}
	return campus.PublicCount{Value: n, Display: strconv.Itoa(int(n))}
}

// AddNoise adds Gaussian noise with the given standard deviation and rounds.
// The random source is passed in so callers control seeding and so the
// function has no hidden shared state. Results never go below zero.
func AddNoise(n float64, stddev float64, rng *mrand.Rand) campus.OccupancyCount {
	if stddev > 0 && rng != nil {
		n += rng.NormFloat64() * stddev
	}
	return campus.OccupancyCount(math.Max(0, math.Round(n)))
}

// Publisher bundles the publication rules with their settings. Each
// goroutine that publishes should own its own Publisher (it holds an RNG).
type Publisher struct {
	Threshold int
	Noise     float64
	rng       *mrand.Rand
}

// NewPublisher creates a publisher with its own random source.
func NewPublisher(threshold int, noise float64, seed uint64) *Publisher {
	return &Publisher{Threshold: threshold, Noise: noise, rng: mrand.New(mrand.NewPCG(seed, 0x5eed))}
}

// Publish noises then suppresses a raw estimate. The order matters: noise is
// applied first so the suppression decision is made on the published value.
func (p *Publisher) Publish(estimate float64) campus.PublicCount {
	return Suppress(AddNoise(estimate, p.Noise, p.rng), p.Threshold)
}
