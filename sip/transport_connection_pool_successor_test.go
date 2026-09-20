package sip

import (
	"net"
	"testing"

	"github.com/emiago/sipgo/fakes"
)

// Two connections can legitimately share a pool key. A connection is filed under
// both its local and its remote address, so a peer that reconnects from the same
// source port — a SIP UE re-registering, say — produces a successor whose remote
// key is identical to its predecessor's. Add lets the successor win that key,
// which is the intended outcome: the newer connection is the reachable one.
//
// The predecessor's reader goroutine then exits, as it must, and deletes the key
// by name. The name now belongs to the successor, so the delete evicts a live,
// serving connection that the dying reader has nothing to do with. Nothing
// re-adds it, because nothing knows it is gone: the socket is open, its reader
// is running, and only the pool has forgotten it. Every subsequent request routed
// by that address fails to find a connection and dials a second one, or gives up.
func TestAPredecessorDoesNotEvictItsSuccessor(t *testing.T) {
	remote := net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 5060}

	conn := func(localPort int) *TCPConnection {
		return &TCPConnection{Conn: &fakes.TCPConn{
			LAddr: net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: localPort},
			RAddr: remote,
		}}
	}

	predecessor, successor := conn(5060), conn(5061)

	pool := newConnectionPool()
	pool.Add(remote.String(), predecessor)
	pool.Add(remote.String(), successor)

	// The predecessor's reader exits, exactly as readConnection does it.
	if err := pool.CloseAndDelete(predecessor, remote.String()); err != nil {
		t.Fatalf("closing the predecessor: %v", err)
	}

	switch got := pool.Get(remote.String()); got {
	case successor:
	case nil:
		t.Fatal("the successor was evicted from the pool by the predecessor's reader exiting; " +
			"it is still open and still being read, but nothing can reach it by address any more")
	default:
		t.Fatalf("the pool holds neither the successor nor nothing: %v", got)
	}
}

// TestASoleConnectionIsStillDeleted is the positive control for the test above.
// A fix that made CloseAndDelete compare before deleting could pass that test by
// never deleting at all, which would turn a pool into a leak; this one fails if
// the ordinary case — the only connection under a key, closing — stops clearing
// the key.
func TestASoleConnectionIsStillDeleted(t *testing.T) {
	remote := net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 5060}
	sole := &TCPConnection{Conn: &fakes.TCPConn{
		LAddr: net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060},
		RAddr: remote,
	}}

	pool := newConnectionPool()
	pool.Add(remote.String(), sole)

	if err := pool.CloseAndDelete(sole, remote.String()); err != nil {
		t.Fatalf("closing the sole connection: %v", err)
	}

	if got := pool.Get(remote.String()); got != nil {
		t.Fatalf("a closed connection is still reachable in the pool: %v", got)
	}
}
