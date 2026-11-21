package sip

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testMakeInviteAndCancel(branch string) (*Request, *Request) {
	p := NewParser()
	invRaw := []byte("INVITE sip:bob@example.com SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 198.51.100.10:5060;branch=" + branch + "\r\n" +
		"From: <sip:alice@example.com>;tag=from1\r\n" +
		"To: <sip:bob@example.com>\r\n" +
		"Call-ID: cancel-test-" + branch + "\r\n" +
		"CSeq: 1 INVITE\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n")
	canRaw := []byte("CANCEL sip:bob@example.com SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 198.51.100.10:5060;branch=" + branch + "\r\n" +
		"From: <sip:alice@example.com>;tag=from1\r\n" +
		"To: <sip:bob@example.com>\r\n" +
		"Call-ID: cancel-test-" + branch + "\r\n" +
		"CSeq: 1 CANCEL\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n")
	inv, err := p.ParseSIP(invRaw)
	if err != nil {
		panic(err)
	}
	can, err := p.ParseSIP(canRaw)
	if err != nil {
		panic(err)
	}
	return inv.(*Request), can.(*Request)
}

func testReadSIPResponse(t *testing.T, r *bufio.Reader, conn net.Conn) *Response {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		b.WriteString(line)
		if line == "\r\n" {
			break
		}
	}
	msg, err := NewParser().ParseSIP([]byte(b.String()))
	require.NoError(t, err)
	res, ok := msg.(*Response)
	require.True(t, ok, "expected SIP response, got %T", msg)
	return res
}

// RFC 3261 section 16.10 and RFC 5407 section 3.1.2: a stateful proxy answers
// CANCEL with 200 OK and forwards CANCEL downstream while keeping the upstream
// INVITE server transaction open so a crossing 200 OK from the callee is still
// relayed upstream instead of being rejected after an automatic 487.
func TestProxyCancelRelaysCrossing200OKInsteadOfSynthesizing487(t *testing.T) {
	tp := NewTransportLayer(net.DefaultResolver, NewParser(), nil)
	defer tp.Close()
	txl := NewTransactionLayer(tp, WithTransactionLayerProxyCancel())
	defer txl.Close()

	srvPipe, cliPipe := net.Pipe()
	defer cliPipe.Close()
	conn := &TCPConnection{Conn: srvPipe, refcount: 1 + TransportIdleConnection}
	defer conn.Close()
	tp.tcp.pool.Add("198.51.100.10:5060", conn)

	inv, can := testMakeInviteAndCancel("z9hG4bK-proxy-cancel-200")
	inv.SetSource("198.51.100.10:5060")
	can.SetSource("198.51.100.10:5060")

	gotTx := make(chan *ServerTx, 1)
	txl.OnRequest(func(_ *Request, tx *ServerTx) {
		gotTx <- tx
	})

	require.NoError(t, txl.handleRequest(inv))
	tx := <-gotTx
	defer tx.Terminate()

	canceled := make(chan *Request, 1)
	require.True(t, tx.OnCancel(func(r *Request) {
		canceled <- r
	}))

	// Read responses concurrently so net.Pipe writes never block.
	reader := bufio.NewReader(cliPipe)
	resCh := make(chan *Response, 4)
	go func() {
		for i := 0; i < 2; i++ {
			resCh <- testReadSIPResponse(t, reader, cliPipe)
		}
	}()

	require.NoError(t, txl.handleRequest(can))

	select {
	case r := <-canceled:
		require.Equal(t, CANCEL, r.Method)
	case <-time.After(time.Second):
		t.Fatal("OnCancel was not invoked")
	}

	// First response on wire must be 200 OK to CANCEL (never 487 to INVITE).
	res1 := <-resCh
	require.Equal(t, StatusOK, res1.StatusCode)
	require.Equal(t, CANCEL, res1.CSeq().MethodName)

	// Transaction must still be open and accept a crossing 200 OK to INVITE.
	select {
	case <-tx.Done():
		t.Fatal("INVITE server transaction terminated on CANCEL in proxy cancel mode")
	default:
	}

	invOK := NewResponseFromRequest(inv, StatusOK, "OK", nil)
	require.NoError(t, tx.Respond(invOK), "relaying a crossing 200 OK after CANCEL must succeed")

	res2 := <-resCh
	require.Equal(t, StatusOK, res2.StatusCode)
	require.Equal(t, INVITE, res2.CSeq().MethodName)
}

func TestProxyCancelRelaysDownstream487(t *testing.T) {
	tp := NewTransportLayer(net.DefaultResolver, NewParser(), nil)
	defer tp.Close()
	txl := NewTransactionLayer(tp, WithTransactionLayerProxyCancel())
	defer txl.Close()

	srvPipe, cliPipe := net.Pipe()
	defer cliPipe.Close()
	conn := &TCPConnection{Conn: srvPipe, refcount: 1 + TransportIdleConnection}
	defer conn.Close()
	tp.tcp.pool.Add("198.51.100.10:5060", conn)

	inv, can := testMakeInviteAndCancel("z9hG4bK-proxy-cancel-487")
	inv.SetSource("198.51.100.10:5060")
	can.SetSource("198.51.100.10:5060")

	gotTx := make(chan *ServerTx, 1)
	txl.OnRequest(func(_ *Request, tx *ServerTx) {
		gotTx <- tx
	})

	require.NoError(t, txl.handleRequest(inv))
	tx := <-gotTx
	defer tx.Terminate()

	canceled := make(chan struct{}, 1)
	require.True(t, tx.OnCancel(func(*Request) { canceled <- struct{}{} }))

	reader := bufio.NewReader(cliPipe)
	resCh := make(chan *Response, 4)
	go func() {
		for i := 0; i < 2; i++ {
			resCh <- testReadSIPResponse(t, reader, cliPipe)
		}
	}()

	require.NoError(t, txl.handleRequest(can))
	<-canceled

	res1 := <-resCh
	require.Equal(t, StatusOK, res1.StatusCode)
	require.Equal(t, CANCEL, res1.CSeq().MethodName)

	inv487 := NewResponseFromRequest(inv, StatusRequestTerminated, "Request Terminated", nil)
	require.NoError(t, tx.Respond(inv487))

	res2 := <-resCh
	require.Equal(t, StatusRequestTerminated, res2.StatusCode)
	require.Equal(t, INVITE, res2.CSeq().MethodName)
}

func TestProxyCancelNotifiesLateOnCancelRegistrationAndFiresOnce(t *testing.T) {
	tp := NewTransportLayer(net.DefaultResolver, NewParser(), nil)
	defer tp.Close()
	txl := NewTransactionLayer(tp, WithTransactionLayerProxyCancel())
	defer txl.Close()

	srvPipe, cliPipe := net.Pipe()
	defer cliPipe.Close()
	conn := &TCPConnection{Conn: srvPipe, refcount: 1 + TransportIdleConnection}
	defer conn.Close()
	tp.tcp.pool.Add("198.51.100.10:5060", conn)

	inv, can := testMakeInviteAndCancel("z9hG4bK-proxy-cancel-late")
	inv.SetSource("198.51.100.10:5060")
	can.SetSource("198.51.100.10:5060")

	gotTx := make(chan *ServerTx, 1)
	txl.OnRequest(func(_ *Request, tx *ServerTx) {
		gotTx <- tx
	})

	require.NoError(t, txl.handleRequest(inv))
	tx := <-gotTx
	defer tx.Terminate()

	// Drain 200 OKs for two CANCELs and one 200 OK for INVITE.
	reader := bufio.NewReader(cliPipe)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3; i++ {
			_ = testReadSIPResponse(t, reader, cliPipe)
		}
	}()

	// CANCEL arrives BEFORE OnCancel is registered.
	require.NoError(t, txl.handleRequest(can))

	var calls atomic.Int32
	var gotReq *Request
	require.True(t, tx.OnCancel(func(r *Request) {
		calls.Add(1)
		gotReq = r
	}))
	require.Equal(t, int32(1), calls.Load(), "late OnCancel registration must be notified immediately")
	require.NotNil(t, gotReq)
	require.Equal(t, CANCEL, gotReq.Method)

	// Retransmitted CANCEL must not invoke OnCancel again.
	require.NoError(t, txl.handleRequest(can))
	require.Equal(t, int32(1), calls.Load())

	// Transaction still relays crossing 200 OK.
	require.NoError(t, tx.Respond(NewResponseFromRequest(inv, StatusOK, "OK", nil)))
	wg.Wait()
}
