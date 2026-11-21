package sip

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startFakeDNSServer serves A queries over UDP on 127.0.0.1:0, returning replyIP
// for A queries (and NODATA for AAAA) when fail is false, or NXDOMAIN when fail
// is true. Every incoming A query increments aQueries (and optionally waits on
// gate before responding so concurrent callers overlap).
func startFakeDNSServer(t *testing.T, replyIP [4]byte, fail *atomic.Bool, aQueries *atomic.Int32, gate <-chan struct{}) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 12 {
				continue
			}
			pkt := make([]byte, n)
			copy(pkt, buf[:n])

			// Walk the single question name to find QTYPE.
			off := 12
			for off < len(pkt) && pkt[off] != 0 {
				off += int(pkt[off]) + 1
			}
			if off+5 > len(pkt) {
				continue
			}
			qend := off + 5
			qtype := binary.BigEndian.Uint16(pkt[off+1 : off+3])

			go func(req []byte, qend int, qtype uint16, dst net.Addr) {
				if qtype == 1 { // A
					aQueries.Add(1)
					if gate != nil {
						<-gate
					}
				}
				resp := make([]byte, 0, qend+16)
				resp = append(resp, req[:qend]...)
				// QR=1, AA=1, RD=copied
				resp[2] = 0x84 | (req[2] & 0x01)
				if fail != nil && fail.Load() {
					resp[3] = 0x83 // RA=1, RCODE=NXDOMAIN
					binary.BigEndian.PutUint16(resp[6:8], 0)
					binary.BigEndian.PutUint16(resp[8:10], 0)
					binary.BigEndian.PutUint16(resp[10:12], 0)
					_, _ = pc.WriteTo(resp, dst)
					return
				}
				resp[3] = 0x80 // RA=1, NOERROR
				binary.BigEndian.PutUint16(resp[8:10], 0)
				binary.BigEndian.PutUint16(resp[10:12], 0)
				if qtype == 1 {
					binary.BigEndian.PutUint16(resp[6:8], 1) // ANCOUNT=1
					// Pointer to name at offset 12, TYPE A, CLASS IN, TTL 30, RDLENGTH 4, IPv4
					resp = append(resp,
						0xc0, 0x0c,
						0x00, 0x01,
						0x00, 0x01,
						0x00, 0x00, 0x00, 0x1e,
						0x00, 0x04,
						replyIP[0], replyIP[1], replyIP[2], replyIP[3],
					)
				} else {
					binary.BigEndian.PutUint16(resp[6:8], 0) // NODATA for AAAA
				}
				_, _ = pc.WriteTo(resp, dst)
			}(pkt, qend, qtype, from)
		}
	}()

	addr := pc.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp4", addr)
		},
	}
}

func TestDNSCacheCoalescesConcurrentLookupsAndServesWithinTTL(t *testing.T) {
	var queries atomic.Int32
	gate := make(chan struct{})
	resolver := startFakeDNSServer(t, [4]byte{10, 96, 1, 2}, nil, &queries, gate)

	var clockMu sync.Mutex
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)

	tp := NewTransportLayer(resolver, NewParser(), nil, WithTransportLayerDNSCacheTTL(5*time.Second))
	defer tp.Close()
	tp.dnsNow = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}

	const workers = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	ips := make(chan string, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var addr Addr
			err := tp.resolveAddrIP(context.Background(), "scscf.cnaas.svc.cluster.local.", &addr)
			errs <- err
			if err == nil {
				ips <- addr.IP.String()
			}
		}()
	}

	// Wait until the single coalesced query has hit the server, then release it.
	require.Eventually(t, func() bool { return queries.Load() >= 1 }, 2*time.Second, 5*time.Millisecond)
	// Give other goroutines a moment to join the singleflight before unblocking.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	close(ips)

	for err := range errs {
		require.NoError(t, err)
	}
	for ip := range ips {
		require.Equal(t, "10.96.1.2", ip)
	}
	require.Equal(t, int32(1), queries.Load(), "concurrent lookups must coalesce into one DNS query")

	// Subsequent lookups within TTL must hit cache without querying DNS.
	for i := 0; i < 5; i++ {
		var addr Addr
		require.NoError(t, tp.resolveAddrIP(context.Background(), "scscf.cnaas.svc.cluster.local.", &addr))
		require.Equal(t, "10.96.1.2", addr.IP.String())
	}
	require.Equal(t, int32(1), queries.Load(), "cached entry must serve within TTL")

	// Advance past TTL: next lookup must refresh from DNS.
	clockMu.Lock()
	now = now.Add(6 * time.Second)
	clockMu.Unlock()

	var addr Addr
	require.NoError(t, tp.resolveAddrIP(context.Background(), "scscf.cnaas.svc.cluster.local.", &addr))
	require.Equal(t, "10.96.1.2", addr.IP.String())
	require.Equal(t, int32(2), queries.Load(), "expired entry must trigger a fresh DNS query")
}

func TestDNSCacheDoesNotCacheErrors(t *testing.T) {
	var dials atomic.Int32
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("resolver unavailable")
		},
	}

	tp := NewTransportLayer(resolver, NewParser(), nil, WithTransportLayerDNSCacheTTL(30*time.Second))
	defer tp.Close()

	var addr Addr
	require.Error(t, tp.resolveAddrIP(context.Background(), "missing.invalid.", &addr))
	first := dials.Load()
	require.GreaterOrEqual(t, first, int32(1))

	require.Error(t, tp.resolveAddrIP(context.Background(), "missing.invalid.", &addr))
	require.Greater(t, dials.Load(), first, "errors must not be cached")
}

func TestDNSCacheDisabledByDefault(t *testing.T) {
	var queries atomic.Int32
	resolver := startFakeDNSServer(t, [4]byte{10, 96, 2, 3}, nil, &queries, nil)

	tp := NewTransportLayer(resolver, NewParser(), nil)
	defer tp.Close()

	for i := 0; i < 3; i++ {
		var addr Addr
		require.NoError(t, tp.resolveAddrIP(context.Background(), "tas.cnaas.svc.cluster.local.", &addr))
		require.Equal(t, "10.96.2.3", addr.IP.String())
	}
	require.Equal(t, int32(3), queries.Load(), "DNS cache must be disabled unless WithTransportLayerDNSCacheTTL is set")
}
