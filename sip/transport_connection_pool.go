package sip

import (
	"bytes"
	"errors"
	"net"
	"sync"

	"golang.org/x/sync/singleflight"
)

type Connection interface {
	// LocalAddr used for connection
	LocalAddr() net.Addr
	// WriteMsg marshals message and sends to socket
	WriteMsg(msg Message) error
	// Reference of connection can be increased/decreased to prevent closing to earlyss
	Ref(i int) int
	// Close decreases reference and if ref = 0 closes connection. Returns last ref. If 0 then it is closed
	TryClose() (int, error)

	Close() error
}

var bufPool = sync.Pool{
	New: func() interface{} {
		// The Pool's New function should generally only return pointer
		// types, since a pointer can be put into the return interface
		// value without an allocation:
		b := new(bytes.Buffer)
		// b.Grow(2048)
		return b
	},
}

type connectionPool struct {
	// TODO consider sync.Map way with atomic checks to reduce mutex contention
	sync.RWMutex
	m  map[string]Connection
	sf singleflight.Group
}

func newConnectionPool() *connectionPool {
	p := &connectionPool{}
	p.init()
	return p
}

func (p *connectionPool) init() {
	p.m = make(map[string]Connection)
}

func (p *connectionPool) addSingleflight(raddr Addr, laddr Addr, reuse bool, do func() (Connection, error)) (Connection, error) {
	a := raddr.String()

	if laddr.Port > 0 || reuse {
		// TODO: implement singleflight without  type conversion
		laddrStr := laddr.String()
		// We create or return existing
		conn, err, _ := p.sf.Do(laddrStr+a, func() (any, error) {
			if laddr.Port > 0 {
				if c := p.getUnref(laddrStr); c != nil {
					return c, nil
				}
			} else {
				if c := p.getUnref(a); c != nil {
					return c, nil
				}
			}

			c, err := do()
			if err != nil {
				return nil, err
			}
			// Decrease reference as it will be increased after
			// Singleflight will return cached so we need todo this
			c.Ref(-1)

			p.Lock()
			defer p.Unlock()

			p.m[a] = c
			p.m[c.LocalAddr().String()] = c
			return c, nil
		})
		if err != nil {
			return nil, err
		}
		c := conn.(Connection)
		c.Ref(1)
		return c, nil
	}

	// There is nothing here to block
	c, err := do()
	if err != nil {
		return nil, err
	}

	if c.Ref(0) < 1 {
		c.Ref(1) // Make 1 reference count by default
	}
	p.m[a] = c
	p.m[c.LocalAddr().String()] = c
	return c, nil
}

func (p *connectionPool) Add(a string, c Connection) {
	// TODO how about multi connection support for same remote address
	// We can then check ref count

	if c.Ref(0) < 1 {
		c.Ref(1) // Make 1 reference count by default
	}
	p.Lock()
	p.m[a] = c
	p.Unlock()
}

// Getting connection pool increases reference
// Make sure you TryClose after finish
func (p *connectionPool) Get(a string) (c Connection) {
	// p.RLock()
	// c, exists := p.m[a]
	// p.RUnlock()
	// if !exists {
	// 	return nil
	// }
	c = p.getUnref(a)
	if c == nil {
		return nil
	}
	c.Ref(1)
	return c
}

func (p *connectionPool) getUnref(a string) (c Connection) {
	p.RLock()
	c, exists := p.m[a]
	p.RUnlock()
	if !exists {
		return nil
	}
	return c
}

// CloseAndDelete closes connection and deletes from pool.
//
// An address is only a name for a connection, and names get reused. A peer that
// reconnects from the same source port — a SIP UE re-registering, say — produces
// a successor filed under a key its predecessor already holds, and Add lets the
// successor win it, which is what we want: the newer connection is the reachable
// one. Deleting by name alone would then let the predecessor's reader, exiting
// afterwards as it must, evict a connection it has nothing to do with. The
// successor stays open and stays read, and only the pool forgets it, so nothing
// re-adds it; every message routed to that address dials a second connection or
// fails outright. Remove the entry only while it still names c.
func (p *connectionPool) CloseAndDelete(c Connection, addr string) error {
	p.Lock()
	defer p.Unlock()
	p.deleteIfCurrent(addr, c)
	ref, _ := c.TryClose() // Be nice. Saves from double closing
	if ref > 0 {
		return c.Close()
	}
	return nil
}

// Delete removes addr from the pool if it still names c. See [connectionPool.CloseAndDelete]
// for why the connection and not just the address has to be matched.
func (p *connectionPool) Delete(addr string, c Connection) {
	p.Lock()
	defer p.Unlock()
	p.deleteIfCurrent(addr, c)
}

// DeleteMultiple removes each of addrs that still names c. See
// [connectionPool.CloseAndDelete] for why c has to be matched.
func (p *connectionPool) DeleteMultiple(addrs []string, c Connection) {
	p.Lock()
	defer p.Unlock()
	for _, a := range addrs {
		p.deleteIfCurrent(a, c)
	}
}

// deleteIfCurrent drops addr only while it still resolves to c. Callers hold the
// lock.
func (p *connectionPool) deleteIfCurrent(addr string, c Connection) {
	if existing, ok := p.m[addr]; ok && existing != c {
		return
	}
	delete(p.m, addr)
}

// Clear will clear all connection from pool and close them
func (p *connectionPool) Clear() error {
	p.Lock()
	defer p.Unlock()

	defer func() {
		// Remove all
		p.m = make(map[string]Connection)
	}()

	var werr error
	for _, c := range p.m {
		if c.Ref(0) <= 0 {
			continue
		}
		werr = errors.Join(werr, c.Close())
	}
	return werr
}

func (p *connectionPool) Size() int {
	p.RLock()
	l := len(p.m)
	p.RUnlock()
	return l
}
