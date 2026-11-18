package config

import (
	"errors"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}

func TestPrecedenceFlagsOverEnv(t *testing.T) {
	cfg, err := Load([]string{"-speed", "120"}, env(map[string]string{
		"CAMPUSPULSE_SPEED": "30",
		"CAMPUSPULSE_TICK":  "1s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SimSpeed != 120 {
		t.Errorf("speed = %v, want flag value 120", cfg.SimSpeed)
	}
	if cfg.TickInterval != time.Second {
		t.Errorf("tick = %v, want env value 1s", cfg.TickInterval)
	}
}

func TestInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"bad env number", nil, map[string]string{"CAMPUSPULSE_POPULATION": "lots"}},
		{"speed out of range", []string{"-speed", "0"}, nil},
		{"start hour", []string{"-start-hour", "25"}, nil},
		{"unknown flag", []string{"-nope"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(tt.args, env(tt.env)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Default()
	cfg.Workers = 0
	cfg.NoiseStdDev = -1
	err := cfg.Validate()
	var errs campus.ValidationErrors
	if !errors.As(err, &errs) || len(errs) != 2 {
		t.Fatalf("want 2 validation errors, got %v", err)
	}
	if !errors.Is(err, campus.ErrInvalidInput) {
		t.Fatal("validation error should match ErrInvalidInput")
	}
}
