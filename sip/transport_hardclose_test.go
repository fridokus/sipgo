package sip

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"

	"github.com/emiago/sipgo/fakes"
)

// Connection is the shape all three transports share for this purpose.
type refCounted interface {
	Ref(int) int
	Close() error
	TryClose() (int, error)
}

// warnRecorder collects the messages logged at Warn or above.
type warnRecorder struct {
	mu  sync.Mutex
	got []string
}

func (w *warnRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnRecorder) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w *warnRecorder) WithGroup(string) slog.Handler            { return w }

func (w *warnRecorder) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.mu.Lock()
		w.got = append(w.got, r.Message)
		w.mu.Unlock()
	}
	return nil
}

func (w *warnRecorder) messages() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.got...)
}

// recordWarnings installs a logger for the duration of the test and hands back
// what it caught. SetDefaultLogger is process-wide, so these tests do not run in
// parallel with anything.
func recordWarnings(t *testing.T) *warnRecorder {
	t.Helper()
	w := &warnRecorder{}
	prev := DefaultLogger()
	SetDefaultLogger(slog.New(w))
	t.Cleanup(func() { SetDefaultLogger(prev) })
	return w
}

func connections(t *testing.T) map[string]refCounted {
	t.Helper()
	ip1, ip2 := net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2")
	laddr := net.TCPAddr{IP: ip1, Port: 5060}
	raddr := net.TCPAddr{IP: ip2, Port: 5060}
	return map[string]refCounted{
		"tcp": &TCPConnection{Conn: &fakes.TCPConn{LAddr: laddr, RAddr: raddr}},
		"ws":  &WSConnection{Conn: &fakes.TCPConn{LAddr: laddr, RAddr: raddr}},
		"udp": &UDPConnection{PacketConn: &fakes.UDPConn{LAddr: net.UDPAddr{IP: ip1, Port: 5060}, RAddr: net.UDPAddr{IP: ip2, Port: 5060}}},
	}
}

// A hard close is how a connection is taken away from readers that are still
// holding references to it: Close sets the count to zero whatever it was, and
// the socket goes with it. Each holder then calls TryClose on its way out — a
// reader goroutine's deferred release, a lookup that closed what it found — and
// every one of those drives the count below zero. Reporting each as a fault
// turns one deliberate close into a warning per holder.
//
// The P-CSCF closes one pooled connection per registration, and this was 3,700
// warnings an hour on a 25,000-subscriber bed, against a handful of warnings
// about anything else.
func TestAHardCloseIsNotAMiscountedReference(t *testing.T) {
	for name, c := range connections(t) {
		t.Run(name, func(t *testing.T) {
			w := recordWarnings(t)

			// Two holders: the reader goroutine and a pool lookup, say.
			c.Ref(1)
			c.Ref(1)
			// The error is the fake's, not the transport's: these connections
			// have no descriptor behind them, so closing one is free to fail.
			// What is under test is the count and what gets logged about it.
			_ = c.Close()

			// Both let go afterwards, as they must.
			for i := 1; i <= 2; i++ {
				ref, _ := c.TryClose()
				if ref != 0 {
					t.Errorf("release %d reported ref %d, want 0: a count already at zero has nothing left to give", i, ref)
				}
			}

			if got := w.messages(); len(got) != 0 {
				t.Errorf("a hard close warned %d time(s): %v", len(got), got)
			}
		})
	}
}

// The warning still has a job: a release of a reference that was never taken
// leaves the count one short, and the next holder to let go closes a connection
// somebody else is still writing to. Nothing hard-closed this one, so there is
// nothing to excuse the negative count.
func TestAReleaseOfAReferenceNobodyTookStillWarns(t *testing.T) {
	for name, c := range connections(t) {
		t.Run(name, func(t *testing.T) {
			w := recordWarnings(t)

			// One holder, letting go properly: the count reaches zero and the
			// connection closes. Any error is the fake's; see above.
			c.Ref(1)
			if ref, _ := c.TryClose(); ref != 0 {
				t.Fatalf("the proper release left ref %d, want 0", ref)
			}
			// This second one is the reference nobody held.
			if _, err := c.TryClose(); err != nil {
				t.Fatalf("the release too many: %v", err)
			}

			if got := w.messages(); len(got) != 1 {
				t.Errorf("a release too many warned %d time(s), want 1: %v", len(got), got)
			}
		})
	}
}
