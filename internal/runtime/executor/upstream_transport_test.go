package executor

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kiroauth "github.com/nguyenha935/kiro-cpa-plugin/internal/auth/kiro"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// routeToServer sends executor requests to target through a clone of the
// production pooled transport, so real connection and body behaviour applies.
// The clone is returned for tests that shorten its timings.
func routeToServer(t *testing.T, target string) *http.Transport {
	t.Helper()
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	transport := getKiroPooledHTTPClient().Transport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	original := kiroHTTPClientFor
	kiroauth.ConfigureGlobalRateLimiter(kiroauth.RateLimiterConfig{MinTokenInterval: time.Millisecond, MaxTokenInterval: 2 * time.Millisecond})
	t.Cleanup(func() {
		kiroHTTPClientFor = original
		kiroauth.ConfigureGlobalRateLimiter(kiroauth.RateLimiterConfig{})
	})
	kiroHTTPClientFor = func(context.Context, *config.Config, *cliproxyauth.Auth, time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.URL.Scheme, r.URL.Host = parsed.Scheme, parsed.Host
			return transport.RoundTrip(r)
		})}
	}
	return transport
}

// The plugin runs on context.Background(), and nothing limited the silence
// between two reads of a 2xx body: an upstream that sent its headers and then
// stopped held Execute, or the stream goroutine, forever. Both paths must give
// up after the idle limit and report it as the transport timeout it is.
func TestStalledUpstreamBodyTimesOut(t *testing.T) {
	originalIdle := upstreamIdleTimeout
	upstreamIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { upstreamIdleTimeout = originalIdle })

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	routeToServer(t, server.URL)

	for _, test := range []struct {
		name    string
		execute func() error
	}{
		{"non-stream", func() error {
			request, options := executorTestRequest()
			_, err := NewKiroExecutor(nil).Execute(context.Background(), builderIDAuth("stall-non-stream"), request, options)
			return err
		}},
		{"stream", func() error {
			request, options := executorTestRequest()
			result, err := NewKiroExecutor(nil).ExecuteStream(context.Background(), builderIDAuth("stall-stream"), request, options)
			if err != nil {
				return err
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					return chunk.Err
				}
			}
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := make(chan error, 1)
			go func() { result <- test.execute() }()
			select {
			case err := <-result:
				assertTransportFailure(t, err, "i/o timeout")
			case <-time.After(3 * time.Second):
				t.Fatal("still blocked after 3s of upstream silence")
			}
		})
	}
}

// A transport error after the request was written (the peer reset or closed
// the connection, or never answered) does not mean Kiro did not receive it:
// retrying made Kiro generate, and bill, the same request again. Only a
// request that never left the client (dial, DNS, TLS, refused) is retried.
func TestTransportErrorIsRetriedOnlyWhenTheRequestWasNotSent(t *testing.T) {
	originalDelay := calculateRetryDelay
	var retries atomic.Int32
	calculateRetryDelay = func(int) time.Duration { retries.Add(1); return time.Millisecond }
	t.Cleanup(func() { calculateRetryDelay = originalDelay })

	var received atomic.Int32
	closing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		received.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(closing.Close)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + listener.Addr().String()
	_ = listener.Close()

	paths := map[string]func(*cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) error{
		"non-stream": func(auth *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options) error {
			_, err := NewKiroExecutor(nil).Execute(context.Background(), auth, request, options)
			return err
		},
		"stream": func(auth *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options) error {
			_, err := NewKiroExecutor(nil).ExecuteStream(context.Background(), auth, request, options)
			return err
		},
	}
	for name, execute := range paths {
		t.Run(name+"/sent", func(t *testing.T) {
			received.Store(0)
			retries.Store(0)
			routeToServer(t, closing.URL)
			request, options := executorTestRequest()
			err := execute(builderIDAuth("sent-"+name), request, options)
			if err == nil || received.Load() != 1 || retries.Load() != 0 {
				t.Fatalf("upstream received the request %d times after %d retries (err=%v), want once with an error", received.Load(), retries.Load(), err)
			}
		})
		t.Run(name+"/refused", func(t *testing.T) {
			retries.Store(0)
			routeToServer(t, refused)
			request, options := executorTestRequest()
			err := execute(builderIDAuth("refused-"+name), request, options)
			if err == nil || retries.Load() != 2 {
				t.Fatalf("refused connection retried %d times (err=%v), want 2", retries.Load(), err)
			}
		})
	}
}

// assertTransportFailure checks that err carries no HTTP status and keeps the
// transport cause in its text, which is all the host sees across the plugin
// ABI.
func assertTransportFailure(t *testing.T, err error, cause string) {
	t.Helper()
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != 0 || !strings.Contains(err.Error(), cause) {
		t.Fatalf("transport failure = %T %v, want status 0 with %q in the message", err, err, cause)
	}
}

// A transport failure is a property of the connection, which both Kiro
// credentials share, not of the credential. Reported as 502 or 504, it made CPA
// cool the credential for its transient-error window; with no status, CPA
// decides from the message, so the cause text must survive.
func TestTransportFailureCarriesNoStatus(t *testing.T) {
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(silent.Close)
	t.Cleanup(func() { close(release) })
	reset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	}))
	t.Cleanup(reset.Close)

	paths := map[string]func(*cliproxyauth.Auth) error{
		"non-stream": func(auth *cliproxyauth.Auth) error {
			request, options := executorTestRequest()
			_, err := NewKiroExecutor(nil).Execute(context.Background(), auth, request, options)
			return err
		},
		"stream": func(auth *cliproxyauth.Auth) error {
			request, options := executorTestRequest()
			_, err := NewKiroExecutor(nil).ExecuteStream(context.Background(), auth, request, options)
			return err
		},
	}
	for name, execute := range paths {
		t.Run(name+"/reset", func(t *testing.T) {
			routeToServer(t, reset.URL)
			assertTransportFailure(t, execute(builderIDAuth("reset-"+name)), "connection reset")
		})
		t.Run(name+"/no response headers", func(t *testing.T) {
			routeToServer(t, silent.URL).ResponseHeaderTimeout = 100 * time.Millisecond
			assertTransportFailure(t, execute(builderIDAuth("headers-"+name)), "timeout awaiting response headers")
		})
	}
}
