package app

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type retryAfterError struct{ delay time.Duration }

func (e retryAfterError) Error() string             { return "retry later" }
func (e retryAfterError) RetryDelay() time.Duration { return e.delay }

func TestRetryDelayRespectsProviderAndAddsJitter(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{"default backoff", errors.New("offline"), 2, 1, 4 * time.Second},
		{"provider minimum", retryAfterError{12 * time.Second}, 0, 1, 15600 * time.Millisecond},
		{"provider shorter than backoff", retryAfterError{time.Second}, 2, 1, 4300 * time.Millisecond},
		{"wrapped header", fmt.Errorf("wrapped: %w", retryAfterError{time.Second}), 0, .5, 1150 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryDelay(tt.err, tt.attempt, tt.jitter); got != tt.want {
				t.Fatalf("delay = %v, want %v", got, tt.want)
			}
		})
	}
}
