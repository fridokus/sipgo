package sip

import (
	"net"
	"testing"

	"github.com/emiago/sipgo/fakes"
)

func TestConnectionPool(t *testing.T) {
	pool := newConnectionPool()

	fakeConn := &fakes.TCPConn{
		LAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060},
		RAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 5060},
		Reader: nil,
		Writer: nil,
	}
	conn := &TCPConnection{Conn: fakeConn}

	pool.Add(fakeConn.RAddr.String(), conn)

	c := pool.Get(fakeConn.RAddr.String())
	if c != conn {
		t.Fatal("Not found connection")
	}
}

func TestConnectionPoolFilesADialedConnectionUnderALocalAddressOnlyWhenChosen(t *testing.T) {
	for _, tc := range []struct {
		name      string
		laddr     Addr
		reuse     bool
		wantLocal bool
	}{
		{"kernel picked the port", Addr{}, true, false},
		{"kernel picked the port without reuse", Addr{}, false, false},
		{"kernel picked the port of a chosen IP", Addr{IP: net.ParseIP("127.0.0.1")}, true, false},
		{"port without an IP", Addr{Port: 5060}, true, false},
		{"port chosen", Addr{IP: net.ParseIP("127.0.0.1"), Port: 5060}, true, true},
		{"port chosen without reuse", Addr{IP: net.ParseIP("127.0.0.1"), Port: 5060}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newConnectionPool()
			conn := &TCPConnection{Conn: &fakes.TCPConn{
				LAddr: net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060},
				RAddr: net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 5060},
			}}
			raddr := Addr{IP: net.ParseIP("127.0.0.2"), Port: 5060}

			c, err := pool.addSingleflight(raddr, tc.laddr, tc.reuse,
				func() (Connection, error) { return conn, nil })
			if err != nil {
				t.Fatalf("create pooled connection: %v", err)
			}
			if c != conn {
				t.Fatal("did not return the created connection")
			}
			if got := pool.Get(raddr.String()); got != conn {
				t.Fatal("not filed under its remote address")
			}
			local := pool.Get(conn.LocalAddr().String())
			if tc.wantLocal && local != conn {
				t.Fatal("not filed under the local address it was asked to use")
			}
			if !tc.wantLocal && local != nil {
				t.Fatal("filed under a local address the kernel picked")
			}
		})
	}
}

func BenchmarkConnectionPool(b *testing.B) {
	pool := newConnectionPool()

	for i := 0; i < b.N; i++ {
		conn := &TCPConnection{Conn: &fakes.TCPConn{
			LAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060},
			RAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 5060},
			Reader: nil,
			Writer: nil,
		}}
		a := &net.TCPAddr{
			IP:   net.IPv4('1', '2', '3', byte(i)),
			Port: 1000,
		}
		pool.Add(a.String(), conn)
		c := pool.Get(a.String())
		if c != conn {
			b.Fatal("mismatched function")
		}
	}
}
