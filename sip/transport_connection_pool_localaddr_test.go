//go:build linux

package sip

import (
	"bufio"
	"context"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sharePort lets a second socket bind a local address another socket already
// holds, which is what Linux does for connect() whenever the two connections
// lead to different peers: it autobinds a port that is in use as long as the
// 4-tuple stays unique.
func sharePort(_, _ string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); cerr != nil {
		return cerr
	}
	return err
}

// A peer that shares this element's IP address can connect to it from the
// same local port the element later picks for a connection of its own to
// somebody else. The response to the peer's next request must still go back
// over the connection that request arrived on, not over the one the element
// dialed. (RFC 3261 section 18.2.2)
func TestTCPDialFromAnAcceptedPeersAddressDoesNotTakeItsResponses(t *testing.T) {
	tp := NewTransportLayer(net.DefaultResolver, NewParser(), nil)
	txl := NewTransactionLayer(tp)
	defer func() { require.NoError(t, tp.Close()) }()
	defer txl.Close()
	txl.OnRequest(func(req *Request, tx *ServerTx) {
		_ = tx.Respond(NewResponseFromRequest(req, StatusOK, "OK", nil))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go tp.ServeTCP(ln)

	peerDialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, Control: sharePort}
	peer, err := peerDialer.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer peer.Close()
	shared := peer.LocalAddr().(*net.TCPAddr)
	require.Eventually(t, func() bool { return tp.tcp.pool.getUnref(shared.String()) != nil },
		2*time.Second, 5*time.Millisecond, "the element never accepted the peer's connection")

	elsewhere, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer elsewhere.Close()
	misdelivered := make(chan string, 1)
	go func() {
		conn, err := elsewhere.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		misdelivered <- line
	}()

	// No local address is asked of sipgo, so the kernel chooses one, and it
	// is allowed to choose the port the peer's connection comes from.
	tp.tcp.DialerCreate = func(laddr net.Addr) net.Dialer {
		require.Nil(t, laddr)
		return net.Dialer{LocalAddr: shared, Control: sharePort}
	}
	target := elsewhere.Addr().(*net.TCPAddr)
	dialed, err := tp.tcp.CreateConnection(context.Background(), Addr{}, Addr{IP: target.IP, Port: target.Port}, tp.handleMessage)
	require.NoError(t, err)
	require.Equal(t, shared.String(), dialed.LocalAddr().String())

	_, err = peer.Write([]byte("OPTIONS sip:element@127.0.0.1 SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP " + shared.String() + ";branch=z9hG4bK-shared-port\r\n" +
		"From: <sip:peer@127.0.0.1>;tag=peer\r\n" +
		"To: <sip:element@127.0.0.1>\r\n" +
		"Call-ID: shared-port\r\n" +
		"CSeq: 1 OPTIONS\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n"))
	require.NoError(t, err)

	require.NoError(t, peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	status, err := bufio.NewReader(peer).ReadString('\n')
	if err != nil {
		select {
		case line := <-misdelivered:
			t.Fatalf("the response went to the connection dialed from the peer's address: %q", strings.TrimSpace(line))
		default:
			t.Fatalf("no response reached the peer: %v", err)
		}
	}
	require.Equal(t, "SIP/2.0 200 OK", strings.TrimSpace(status))
}
