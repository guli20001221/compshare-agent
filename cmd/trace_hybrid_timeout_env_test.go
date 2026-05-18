package main

import (
	"testing"
	"time"
)

// hybridTimeoutFromEnv must return zero (meaning "use retriever default")
// for missing / invalid input, and the parsed duration for valid input.
// Returning 0 (not a panic / not a non-zero default) is load-bearing:
// knowledge.NewRetriever then substitutes its own 5s baseline, so the
// env var is purely an override knob and baseline behavior is unchanged
// when the env var is absent.
func TestHybridTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"unset", "", 0},
		{"whitespace", "   ", 0},
		{"5000_ms", "5000", 5 * time.Second},
		{"8000_ms", "8000", 8 * time.Second},
		{"valid_with_whitespace", "  10000  ", 10 * time.Second},
		{"non_numeric", "abc", 0},
		{"negative", "-100", 0},
		{"zero", "0", 0},
		{"float_invalid", "5.5", 0},
		{"trailing_unit_rejected", "5s", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == "RAG_HYBRID_TIMEOUT_MS" {
					return tc.raw
				}
				return ""
			}
			got := hybridTimeoutFromEnv(getenv)
			if got != tc.want {
				t.Fatalf("hybridTimeoutFromEnv(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
