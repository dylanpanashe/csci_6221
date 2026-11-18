// Package config loads CampusPulse settings from command-line flags and
// CAMPUSPULSE_* environment variables. Flags win over environment variables,
// which win over defaults.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// Config holds every tunable setting. It is a plain value: once loaded it is
// passed around by value or as a read-only pointer and never changed.
type Config struct {
	Addr   string // HTTP listen address
	WebDir string // static frontend directory

	// Simulation
	TickInterval time.Duration // real time between simulation ticks / broadcasts
	SimSpeed     float64       // simulated seconds per real second
	SimStartHour int           // simulated day starts at this local hour
	TimeZone     string        // campus time zone
	Population   int           // simulated students on campus at peak
	Seed         int64         // RNG seed; 0 means time-based
	DemoScenario bool          // load the Bell Hall 1:40 PM changeover scenario

	// Estimation
	DevicesPerUser float64 // calibration: devices observed per person

	// Privacy
	SuppressBelow int     // counts under this are published as "<N"
	NoiseStdDev   float64 // std-dev of noise added to published counts
	HashSecret    string  // base secret for the daily rotating device-hash key

	// Pipeline
	Workers       int // observation workers (bounded pool size)
	ChannelBuffer int // buffer size of pipeline channels

	// Operations
	EnablePprof     bool
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
}

// Default returns the settings used when nothing is overridden.
func Default() Config {
	return Config{
		Addr:            ":8080",
		WebDir:          "web",
		TickInterval:    2 * time.Second,
		SimSpeed:        60, // one simulated minute per real second
		SimStartHour:    8,
		TimeZone:        "America/New_York",
		Population:      2600,
		DevicesPerUser:  1.8,
		SuppressBelow:   10,
		NoiseStdDev:     1.0,
		Workers:         runtime.NumCPU(),
		ChannelBuffer:   4096,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 10 * time.Second,
	}
}

// Load builds a Config from defaults, then the environment, then flags.
// getenv is passed in (usually os.Getenv) so tests can supply a fake
// environment without touching the real one.
func Load(args []string, getenv func(string) string) (Config, error) {
	cfg := Default()
	if err := cfg.applyEnv(getenv); err != nil {
		return Config{}, fmt.Errorf("config: environment: %w", err)
	}

	fs := flag.NewFlagSet("campuspulse", flag.ContinueOnError)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address")
	fs.StringVar(&cfg.WebDir, "web", cfg.WebDir, "directory holding the frontend")
	fs.DurationVar(&cfg.TickInterval, "tick", cfg.TickInterval, "real time between simulation ticks")
	fs.Float64Var(&cfg.SimSpeed, "speed", cfg.SimSpeed, "simulation speed multiplier")
	fs.IntVar(&cfg.SimStartHour, "start-hour", cfg.SimStartHour, "simulated day start hour (0-23)")
	fs.IntVar(&cfg.Population, "population", cfg.Population, "simulated students at peak")
	fs.Int64Var(&cfg.Seed, "seed", cfg.Seed, "random seed (0 = time based)")
	fs.BoolVar(&cfg.DemoScenario, "demo", cfg.DemoScenario, "start at the Bell Hall 1:40 PM demo scenario")
	fs.Float64Var(&cfg.DevicesPerUser, "devices-per-user", cfg.DevicesPerUser, "Wi-Fi devices per person (calibration)")
	fs.IntVar(&cfg.SuppressBelow, "suppress-below", cfg.SuppressBelow, "hide published counts below this value")
	fs.Float64Var(&cfg.NoiseStdDev, "noise", cfg.NoiseStdDev, "std-dev of privacy noise on published counts")
	fs.IntVar(&cfg.Workers, "workers", cfg.Workers, "observation worker pool size")
	fs.BoolVar(&cfg.EnablePprof, "pprof", cfg.EnablePprof, "expose /debug/pprof (development only)")
	logLevel := fs.String("log-level", cfg.LogLevel.String(), "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("config: flags: %w", err)
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(*logLevel)); err != nil {
		return Config{}, fmt.Errorf("config: log level: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// envBinding pairs an environment variable with a function that parses and
// stores it. Using a table of closures keeps applyEnv short and means adding
// a setting is one line.
type envBinding struct {
	key string
	set func(string) error
}

func (c *Config) applyEnv(getenv func(string) string) error {
	bindings := []envBinding{
		{"ADDR", func(v string) error { c.Addr = v; return nil }},
		{"WEB_DIR", func(v string) error { c.WebDir = v; return nil }},
		{"TICK", durationInto(&c.TickInterval)},
		{"SPEED", floatInto(&c.SimSpeed)},
		{"START_HOUR", intInto(&c.SimStartHour)},
		{"TZ", func(v string) error { c.TimeZone = v; return nil }},
		{"POPULATION", intInto(&c.Population)},
		{"DEVICES_PER_USER", floatInto(&c.DevicesPerUser)},
		{"SUPPRESS_BELOW", intInto(&c.SuppressBelow)},
		{"NOISE", floatInto(&c.NoiseStdDev)},
		{"HASH_SECRET", func(v string) error { c.HashSecret = v; return nil }},
		{"WORKERS", intInto(&c.Workers)},
		{"PPROF", boolInto(&c.EnablePprof)},
		{"DEMO", boolInto(&c.DemoScenario)},
	}
	var errs []error
	for _, b := range bindings {
		v := strings.TrimSpace(getenv("CAMPUSPULSE_" + b.key))
		if v == "" {
			continue
		}
		if err := b.set(v); err != nil {
			errs = append(errs, fmt.Errorf("CAMPUSPULSE_%s=%q: %w", b.key, v, err))
		}
	}
	return errors.Join(errs...) // nil when errs is empty
}

// The helpers below are higher-order functions: each takes a pointer to a
// field and returns a setter closure that captures it.

func intInto(p *int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err == nil {
			*p = n
		}
		return err
	}
}

func floatInto(p *float64) func(string) error {
	return func(v string) error {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			*p = f
		}
		return err
	}
}

func boolInto(p *bool) func(string) error {
	return func(v string) error {
		b, err := strconv.ParseBool(v)
		if err == nil {
			*p = b
		}
		return err
	}
}

func durationInto(p *time.Duration) func(string) error {
	return func(v string) error {
		d, err := time.ParseDuration(v)
		if err == nil {
			*p = d
		}
		return err
	}
}

// Validate checks every field and reports all problems at once.
func (c Config) Validate() error {
	var errs campus.ValidationErrors
	check := func(ok bool, field string, value any, reason string) {
		if !ok {
			errs = append(errs, &campus.ValidationError{Field: field, Value: value, Reason: reason})
		}
	}
	check(c.Addr != "", "addr", c.Addr, "must not be empty")
	check(c.TickInterval >= 100*time.Millisecond, "tick", c.TickInterval, "must be at least 100ms")
	check(c.SimSpeed > 0 && c.SimSpeed <= 3600, "speed", c.SimSpeed, "must be between 0 and 3600")
	check(c.SimStartHour >= 0 && c.SimStartHour <= 23, "start-hour", c.SimStartHour, "must be 0-23")
	check(c.Population > 0 && c.Population <= 50000, "population", c.Population, "must be 1-50000")
	check(c.DevicesPerUser >= 0.5 && c.DevicesPerUser <= 5, "devices-per-user", c.DevicesPerUser, "must be 0.5-5")
	check(c.SuppressBelow >= 1, "suppress-below", c.SuppressBelow, "must be at least 1")
	check(c.NoiseStdDev >= 0, "noise", c.NoiseStdDev, "must not be negative")
	check(c.Workers >= 1 && c.Workers <= 256, "workers", c.Workers, "must be 1-256")
	check(c.ChannelBuffer >= 1, "channel-buffer", c.ChannelBuffer, "must be at least 1")
	if _, err := time.LoadLocation(c.TimeZone); err != nil {
		check(false, "tz", c.TimeZone, err.Error())
	}
	return errs.OrNil()
}

// Location returns the campus time zone, falling back to UTC.
// Validate has already confirmed the zone loads.
func (c Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.TimeZone)
	if err != nil {
		return time.UTC
	}
	return loc
}
