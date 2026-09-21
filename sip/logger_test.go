package sip

import (
	"io"
	"log/slog"
	"sync"
	"testing"
)

// TestTheDefaultLoggerCanBeSetWhileTheStackIsRunning is a race-detector test and
// nothing else: it asserts no value, only that the two things that really happen
// to this variable may happen at once.
//
// The doc on SetDefaultLogger says it must be called before any use of the
// library, and as a plain variable that was load-bearing rather than advice —
// every connection goroutine reads the variable, so a program that installed a
// logger after its first call raced, and so did a test that installed one to
// observe the package. Neither is a misuse worth a crash.
func TestTheDefaultLoggerCanBeSetWhileTheStackIsRunning(t *testing.T) {
	prev := DefaultLogger()
	t.Cleanup(func() { SetDefaultLogger(prev) })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					DefaultLogger().Debug("as a connection would")
				}
			}
		}()
	}

	for range 100 {
		SetDefaultLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	SetDefaultLogger(nil) // and back to none, which must also be readable

	close(stop)
	wg.Wait()
}
