package sip

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// RFC 3261 S.18.2.2: on a reliable transport "the response MUST be sent using
// the existing connection to the source of the original request that created
// the transaction, if that connection is still open."
//
// The pool files an accepted connection under its remote address alone, so two
// connections from one address to two of our ports share a key. Behind a NAT
// that happens without anyone misbehaving: the NAT gives one UE's cleartext
// REGISTER a source port that another UE behind the same public address already
// uses as its protected client port. The later accept takes the key, and when it
// closes it deletes the key. Looked up by source address, the first UE's
// requests were answered on the other UE's connection, and then by a dial to
// the Via port, which timed out, so they were never answered at all.
func TestServerRequestConnectionAnswersOnTheConnectionTheRequestArrivedOn(t *testing.T) {
	tp := NewTransportLayer(net.DefaultResolver, NewParser(), nil)
	defer tp.Close()

	const source = "198.51.100.7:40000"
	raw := []byte("OPTIONS sip:example.com SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 198.51.100.7:40002;branch=z9hG4bK-arrival\r\n" +
		"From: <sip:alice@example.com>;tag=from1\r\n" +
		"To: <sip:bob@example.com>\r\n" +
		"Call-ID: arrival\r\n" +
		"CSeq: 1 OPTIONS\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n")

	accept := func() *TCPConnection {
		srv, cli := net.Pipe()
		t.Cleanup(func() { cli.Close() })
		c := &TCPConnection{Conn: srv, refcount: 1 + TransportIdleConnection}
		tp.tcp.pool.Add(source, c)
		return c
	}

	arrived := accept()
	var req *Request
	err := tp.tcp.parseStream(tp.tcp.parser.NewSIPStream(), raw, arrived, source, func(msg Message) {
		req = msg.(*Request)
	})
	require.NoError(t, err)
	require.NotNil(t, req)

	serve := func() (Connection, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		return tp.serverRequestConnection(ctx, req)
	}

	other := accept()
	conn, err := serve()
	require.NoError(t, err)
	require.Same(t, arrived, conn, "the response would go to the connection that took the key")
	conn.TryClose()

	require.NoError(t, tp.tcp.pool.CloseAndDelete(other, source))
	conn, err = serve()
	require.NoError(t, err, "the response was not sent on the connection the request arrived on")
	require.Same(t, arrived, conn)
	conn.TryClose()

	// Once that connection is gone, S.18.2.2's fallback applies as before.
	require.NoError(t, arrived.Close())
	conn, _ = serve()
	if conn != nil {
		require.NotSame(t, arrived, conn, "a closed connection was handed out for a response")
		conn.TryClose()
	}
}
