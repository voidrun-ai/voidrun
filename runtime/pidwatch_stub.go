//go:build !linux

package runtime

import "errors"

var errWatchUnsupported = errors.New("pid watch requires linux")

func LiveCHPID(string) (int, bool) { return 0, false }

func WatchPID(int, <-chan struct{}) error { return errWatchUnsupported }
