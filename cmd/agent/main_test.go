package main

import (
	"strings"
	"testing"
	"time"
)

func TestValidateRuntimeDurations(t *testing.T) {
	tests := []struct {
		name         string
		interval     time.Duration
		probeTimeout time.Duration
		wantError    string
	}{
		{name: "valid", interval: time.Second, probeTimeout: time.Second},
		{name: "zero interval", interval: 0, probeTimeout: time.Second, wantError: "-interval"},
		{name: "negative interval", interval: -time.Second, probeTimeout: time.Second, wantError: "-interval"},
		{name: "zero timeout", interval: time.Second, probeTimeout: 0, wantError: "-probe-timeout"},
		{name: "negative timeout", interval: time.Second, probeTimeout: -time.Second, wantError: "-probe-timeout"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRuntimeDurations(tt.interval, tt.probeTimeout)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantError)
			}
		})
	}
}
