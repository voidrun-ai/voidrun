package timer

import (
	"log"
	"time"
)

// ---------------------------------------------------------
// Mode 1: Function Level (The "Defer" pattern)
// ---------------------------------------------------------

// Track returns a function that, when executed, logs the duration.
// Usage: defer timer.Track("FunctionName")()
func Track(name string) func() {
	start := time.Now()
	return func() {
		log.Printf("[TIME] %s took %v", name, time.Since(start))
	}
}

// ---------------------------------------------------------
// Mode 2: Block Level (The "Stopwatch" pattern)
// ---------------------------------------------------------

// Stopwatch is useful for measuring multiple steps within one function.
type Stopwatch struct {
	start time.Time
	last  time.Time
}

// NewStopwatch starts the clock.
func NewStopwatch() *Stopwatch {
	now := time.Now()
	return &Stopwatch{start: now, last: now}
}

// Lap logs the time taken since the last Lap call.
func (s *Stopwatch) Lap(stepName string) {
	now := time.Now()
	elapsed := now.Sub(s.last)
	s.last = now
	log.Printf("[TIME] Step [%s] took %v (Total: %v)", stepName, elapsed, now.Sub(s.start))
}

// Total logs the total time since the stopwatch started.
func (s *Stopwatch) Total(name string) {
	log.Printf("[TIME] Total [%s] finished in %v", name, time.Since(s.start))
}
