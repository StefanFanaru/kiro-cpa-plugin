package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A zero-value http.Transport dials, handshakes and waits for response headers
// with no deadline at all. The streaming path runs with no client timeout by
// design, so an unreachable or half-open proxy hung the request until the client
// gave up. The unproxied client already carries these values; the proxied one
// must not be the only path without them.
func TestProxyTransportBoundsConnectionEstablishment(t *testing.T) {
	t.Parallel()

	for _, proxyURL := range []string{"http://proxy.invalid:3128", "https://proxy.invalid:3128", "socks5://proxy.invalid:1080"} {
		transport := buildProxyTransport(proxyURL)
		if transport == nil {
			t.Fatalf("%s: no transport built", proxyURL)
		}
		if transport.TLSHandshakeTimeout == 0 {
			t.Errorf("%s: TLS handshake has no deadline", proxyURL)
		}
		if transport.ResponseHeaderTimeout == 0 {
			t.Errorf("%s: waiting for response headers has no deadline", proxyURL)
		}
		if transport.ExpectContinueTimeout == 0 {
			t.Errorf("%s: 100-continue has no deadline", proxyURL)
		}
		if transport.IdleConnTimeout == 0 {
			t.Errorf("%s: idle connections are never reclaimed", proxyURL)
		}
		if transport.DialContext == nil {
			t.Errorf("%s: dialing does not go through a context", proxyURL)
		}
		if !transport.ForceAttemptHTTP2 {
			t.Errorf("%s: HTTP/2 is off, unlike the unproxied transport", proxyURL)
		}
	}
}

// Those deadlines must match the unproxied client's, or routing a credential
// through a proxy silently changes how long a dead peer is tolerated.
func TestProxyTransportMatchesThePooledClientTimings(t *testing.T) {
	t.Parallel()

	pooled, ok := getKiroPooledHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("pooled transport is %T", getKiroPooledHTTPClient().Transport)
	}
	proxied := buildProxyTransport("http://proxy.invalid:3128")

	for _, field := range []struct {
		name            string
		pooled, proxied any
	}{
		{"TLSHandshakeTimeout", pooled.TLSHandshakeTimeout, proxied.TLSHandshakeTimeout},
		{"ResponseHeaderTimeout", pooled.ResponseHeaderTimeout, proxied.ResponseHeaderTimeout},
		{"ExpectContinueTimeout", pooled.ExpectContinueTimeout, proxied.ExpectContinueTimeout},
		{"IdleConnTimeout", pooled.IdleConnTimeout, proxied.IdleConnTimeout},
		{"MaxIdleConns", pooled.MaxIdleConns, proxied.MaxIdleConns},
		{"MaxIdleConnsPerHost", pooled.MaxIdleConnsPerHost, proxied.MaxIdleConnsPerHost},
		{"MaxConnsPerHost", pooled.MaxConnsPerHost, proxied.MaxConnsPerHost},
	} {
		if field.pooled != field.proxied {
			t.Errorf("%s: proxied %v, pooled %v", field.name, field.proxied, field.pooled)
		}
	}
}

// A SOCKS5 proxy must be dialled through the request's context. proxy.SOCKS5
// returns a *socks.Dialer whose deprecated Dial runs the whole handshake against
// context.Background(), so the previous wrapper discarded both the dial deadline
// and the client's cancellation: a request to an unresponsive proxy could not be
// abandoned.
func TestSOCKS5ProxyDialHonoursTheContext(t *testing.T) {
	t.Parallel()

	transport := buildProxyTransport("socks5://198.51.100.1:1080")
	if transport == nil {
		t.Fatal("no transport built for a SOCKS5 proxy")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	conn, err := transport.DialContext(ctx, "tcp", "codewhisperer.us-east-1.amazonaws.com:443")
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("dialling with a cancelled context succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error = %v, want the cancellation", err)
	}
	// 198.51.100.0/24 is TEST-NET-2 and does not answer, so a dial that ignored
	// the context would sit in the TCP connect until the 30s dial deadline.
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("dial took %v to notice a cancelled context", elapsed)
	}
}

// freezer relays TCP connections to a target and, once frozen, stops relaying
// on every connection opened so far without closing it: a peer that went
// silent, as a NAT or load balancer that dropped the flow leaves it.
type freezer struct {
	mu     sync.Mutex
	frozen []chan struct{}
}

func (f *freezer) serve(t *testing.T, target string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()
		close(release)
	})
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			upstream, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			stop := make(chan struct{})
			f.mu.Lock()
			f.frozen = append(f.frozen, stop)
			f.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					select {
					case <-stop:
						<-release
						_ = dst.Close()
						_ = src.Close()
						return
					default:
					}
					if n > 0 {
						_, _ = dst.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}
			go pipe(upstream, client)
			go pipe(client, upstream)
		}
	}()
	return listener.Addr().String()
}

func (f *freezer) freezeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, stop := range f.frozen {
		close(stop)
	}
	f.frozen = nil
}

// Both credentials share one HTTP/2 connection per transport. When that
// connection goes silent without a FIN or RST, nothing told the transport it
// was dead: every later request for either credential was multiplexed onto it
// and failed only at the response header timeout. The transports must probe a
// read-idle connection and drop it when the probe goes unanswered, so the next
// request dials afresh. The production probe timings are shortened here only to
// keep the test fast.
func TestDeadHTTP2ConnectionIsNotReused(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		transport func() *http.Transport
	}{
		{"pooled", func() *http.Transport {
			return getKiroPooledHTTPClient().Transport.(*http.Transport).Clone()
		}},
		{"proxied", func() *http.Transport {
			transport := proxyTransportTimings()
			transport.ForceAttemptHTTP2 = true
			return transport
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			t.Cleanup(server.Close)
			var relay freezer
			addr := relay.serve(t, server.Listener.Addr().String())

			transport := test.transport()
			transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			transport.ResponseHeaderTimeout = time.Second
			if transport.HTTP2 != nil {
				health := *transport.HTTP2
				health.SendPingTimeout = 300 * time.Millisecond
				health.PingTimeout = 300 * time.Millisecond
				transport.HTTP2 = &health
			}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport}
			post := func() (int, error) {
				request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+addr+"/", nil)
				response, err := client.Do(request)
				if err != nil {
					return 0, err
				}
				defer response.Body.Close()
				_, _ = io.ReadAll(response.Body)
				return response.ProtoMajor, nil
			}

			if proto, err := post(); err != nil || proto != 2 {
				t.Fatalf("first request proto=%d err=%v, want HTTP/2", proto, err)
			}
			relay.freezeAll()
			_, _ = post()
			time.Sleep(time.Second)
			if _, err := post(); err != nil {
				t.Fatalf("request after the connection went silent failed: %v", err)
			}
		})
	}
}
