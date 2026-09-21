package sip

import (
	"log/slog"
	"sync/atomic"
)

// defLogger is what the package logs through wherever no component logger is at
// hand: a connection's reference counting, the pool's cleanup.
//
// Atomic because of who touches it. It is written once by a program's setup and
// read by every connection goroutine, so as a plain variable SetDefaultLogger is
// a data race against any connection already running. "Must be called before any
// usage of library" is the only thing that made that safe, and nothing enforces
// it — a program that installs a logger after its first call, or a test that
// installs one to observe the package, trips the race detector in code that is
// otherwise correct.
var defLogger atomic.Pointer[slog.Logger]

// SetDefaultLogger sets the default logger that will be used within the sip
// package. Safe to call at any time, including with a connection running.
func SetDefaultLogger(l *slog.Logger) {
	defLogger.Store(l)
}

// DefaultLogger is the logger set by SetDefaultLogger, or slog.Default() if
// none was. Never nil.
func DefaultLogger() *slog.Logger {
	if l := defLogger.Load(); l != nil {
		return l
	}
	return slog.Default()
}
