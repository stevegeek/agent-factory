package testkit

import (
	"sync"
	"time"
)

// Fixed is a clock that always says T. It satisfies clock.Clock
// (Now() time.Time) without importing it.
type Fixed struct{ T time.Time }

// Now returns T.
func (f Fixed) Now() time.Time { return f.T }

// Step is a clock that starts at a time and moves on by a fixed step after
// every call. It is safe for concurrent use.
type Step struct {
	mu   sync.Mutex
	next time.Time
	step time.Duration
}

// NewStep returns a clock whose first Now is start.
func NewStep(start time.Time, step time.Duration) *Step {
	return &Step{next: start, step: step}
}

// Now returns the current time and advances the clock by one step.
func (s *Step) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.next
	s.next = s.next.Add(s.step)
	return t
}

// Advance moves the clock on by d without a call to Now.
func (s *Step) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = s.next.Add(d)
}

// Stamp is a fixed time used across tests: 2026-01-02T03:04:05+00:00.
func Stamp() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}
