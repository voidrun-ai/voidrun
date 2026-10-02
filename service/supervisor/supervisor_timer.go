package supervisor

import (
	"log"
	"time"
)

func (a *Supervisor) IdleArmed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.idleTimer != nil
}

// SetIdleAfter arms the idle snapshot timer. d < 0 clears it; d == 0 fires now.
func (a *Supervisor) SetIdleAfter(d time.Duration, fn func()) {
	a.replaceTimer(&a.idleTimer, &a.idleGen, d, fn)
}

func (a *Supervisor) StopIdleTimer() {
	a.replaceTimer(&a.idleTimer, &a.idleGen, -1, nil)
}

func (a *Supervisor) replaceTimer(slot **time.Timer, gen *uint64, d time.Duration, fn func()) {
	a.mu.Lock()
	*gen++
	cur := *gen
	if *slot != nil {
		(*slot).Stop()
		*slot = nil
	}
	if fn == nil || d < 0 {
		a.mu.Unlock()
		return
	}
	*slot = time.AfterFunc(d, func() {
		a.mu.Lock()
		ok := cur == *gen
		if ok {
			*slot = nil
		}
		a.mu.Unlock()
		if !ok {
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[supervisor] %s timer panic: %v", a.id, rec)
			}
		}()
		fn()
	})
	a.mu.Unlock()
}
