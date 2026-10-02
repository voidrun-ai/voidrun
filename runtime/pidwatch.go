package runtime

import "errors"

// ErrWatchStopped is returned when WatchPID is cancelled because the supervisor stopped.
var ErrWatchStopped = errors.New("pid watch stopped")
