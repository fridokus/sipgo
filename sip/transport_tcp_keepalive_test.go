package sip

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// readConnectionOverPipe drives the real read loop over a net.Pipe, which is
// unbuffered and synchronous: one Write on the client side is one Read on the
// transport side, so a test can place a TCP read boundary exactly where it
// wants one. Returns the Call-IDs the handler was given, and whatever the
// transport wrote back.
func readConnectionOverPipe(t *testing.T, writes [][]byte) (delivered []Message, pong []byte) {
	t.Helper()

	tcp := &TransportTCP{}
	tcp.init(NewParser())

	closed := make(chan struct{})
	tcp.onConnClose = func(Connection) { close(closed) }

	serverConn, clientConn := net.Pipe()
	conn := &TCPConnection{Conn: serverConn, refcount: 1}

	got := make(chan Message, 8)
	go tcp.readConnection(conn, serverConn.LocalAddr().String(), serverConn.RemoteAddr().String(),
		func(msg Message) { got <- msg })

	// Anything the transport writes back is a pong; read it off so a Write on
	// an unbuffered pipe cannot deadlock.
	pongs := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		var acc []byte
		for {
			n, err := clientConn.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
				select {
				case pongs <- acc:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for _, w := range writes {
		_ = clientConn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, err := clientConn.Write(w)
		require.NoError(t, err)
	}

	// Give the loop a moment to deliver everything these writes can produce.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-got:
			delivered = append(delivered, msg)
			continue
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
		}
		break
	}

	select {
	case pong = <-pongs:
	default:
	}

	_ = clientConn.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
	}
	return delivered, pong
}

// summarise names every part of a message a desynced stream can silently
// mangle: the method, because a stream that resumes two bytes late still parses
// -- "OPTIONS" simply becomes "TIONS" -- the Call-ID, and the body, because
// bytes swallowed out of one message are made up from the next one.
func summarise(t *testing.T, msgs []Message) []string {
	t.Helper()
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		req, ok := m.(*Request)
		require.True(t, ok, "expected a request, got %T", m)
		cid := req.CallID()
		require.NotNil(t, cid)
		out = append(out, fmt.Sprintf("%s %s %q", req.Method, cid.Value(), req.Body()))
	}
	return out
}

// TestReadConnectionKeepAliveMidMessage is the 10,000-UE bed's defect, made
// small. A TCP read that happens to contain nothing but the LF of a header
// line's CRLF used to be swallowed as an RFC 5626 keep-alive, which left the
// parser holding a bare CR. No later byte can complete it, so the *next*
// message -- entirely well formed -- fails to frame with "line has no CRLF",
// and so does every message after it.
//
// Every CRLF in the message is tried, because the read boundary is TCP's
// choice and not ours.
//
// ⚠️ Neutering the !midMessage guard in readConnection turns this red at all
// 22 boundaries with "delivered no messages at all", which is exactly how it
// presented in production: sockets draining, CPU idle, nothing handled.
func TestReadConnectionKeepAliveMidMessage(t *testing.T) {
	first := testRawOptions("mid-message")
	second := testRawOptions("after-the-split")

	var boundaries int
	for i := 0; i+1 < len(first); i++ {
		if first[i] != '\r' || first[i+1] != '\n' {
			continue
		}
		boundaries++

		// Three reads: up to and including the CR, then the lone LF, then the
		// rest of the first message followed by a whole second one.
		writes := [][]byte{
			first[:i+1],
			first[i+1 : i+2],
			append(append([]byte{}, first[i+2:]...), second...),
		}
		delivered, _ := readConnectionOverPipe(t, writes)
		require.Equal(t, []string{`OPTIONS mid-message ""`, `OPTIONS after-the-split ""`}, summarise(t, delivered),
			"a read boundary between the CR and the LF at byte %d must not lose a message", i)
	}
	require.Greater(t, boundaries, 1, "the fixture must contain several CRLFs to be worth splitting")
}

// TestReadConnectionKeepAliveBetweenMessages is the positive control for the
// guard above: it fails if the guard is written so tightly that a real
// keep-alive stops being one. A double CRLF arriving between two messages is
// still a ping, and RFC 5626 S.3.5.1 has it answered with a single CRLF.
func TestReadConnectionKeepAliveBetweenMessages(t *testing.T) {
	writes := [][]byte{
		testRawOptions("before-the-ping"),
		[]byte("\r\n\r\n"),
		testRawOptions("after-the-ping"),
	}
	delivered, pong := readConnectionOverPipe(t, writes)

	require.Equal(t, []string{`OPTIONS before-the-ping ""`, `OPTIONS after-the-ping ""`}, summarise(t, delivered))
	require.Equal(t, []byte("\r\n"), pong, "a double CRLF between messages is a ping and must be ponged")
}

// TestReadConnectionSingleCRLFBetweenMessagesIsNotPonged keeps the other half
// of clause 3.5.1 honest: a single CRLF is a keep-alive that asks for nothing
// back, so the guard must not turn it into a pong now that it also has to
// decide what a CRLF-only read means.
func TestReadConnectionSingleCRLFBetweenMessagesIsNotPonged(t *testing.T) {
	writes := [][]byte{
		testRawOptions("before-the-crlf"),
		[]byte("\r\n"),
		testRawOptions("after-the-crlf"),
	}
	delivered, pong := readConnectionOverPipe(t, writes)

	require.Equal(t, []string{`OPTIONS before-the-crlf ""`, `OPTIONS after-the-crlf ""`}, summarise(t, delivered))
	require.Empty(t, pong, "a single CRLF is not a ping")
}

// TestReadConnectionKeepAliveInsideABody covers the case the obvious fix gets
// wrong. Guarding on "the parser's buffer is empty" looks equivalent to
// "between messages" and is not: ParserStream copies body bytes out of its
// buffer as they arrive, so a message waiting for the last two bytes of its
// body has an empty buffer and is very much part way through.
//
// ⚠️ Swapping the guard for par.Buffer().Len() == 0 turns this red as
//
//	OPTIONS has-a-body "v=0OP"   TIONS after-the-body ""
//
// -- the swallowed CRLF is made up out of the next message, which then resumes
// two bytes late and still parses, method and all.
func TestReadConnectionKeepAliveInsideABody(t *testing.T) {
	body := "v=0\r\n"
	msg := []byte("OPTIONS sip:example.com SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 127.0.0.1:5060;branch=z9hG4bK-body\r\n" +
		"From: <sip:alice@example.com>;tag=from1\r\n" +
		"To: <sip:bob@example.com>\r\n" +
		"Call-ID: has-a-body\r\n" +
		"CSeq: 1 OPTIONS\r\n" +
		"Content-Length: 5\r\n" +
		"\r\n" + body)

	cut := bytes.LastIndex(msg, []byte("\r\n"))
	require.Greater(t, cut, 0)

	// The body's own trailing CRLF arrives alone, with the parser's buffer
	// already drained.
	writes := [][]byte{msg[:cut], msg[cut:], testRawOptions("after-the-body")}
	delivered, _ := readConnectionOverPipe(t, writes)

	require.Equal(t, []string{`OPTIONS has-a-body "v=0\r\n"`, `OPTIONS after-the-body ""`}, summarise(t, delivered))
}
