package executor

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
func routeToServer(t *testing.T, target string) {
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
}

// The plugin runs on context.Background(), and nothing limited the silence
// between two reads of a 2xx body: an upstream that sent its headers and then
// stopped held Execute, or the stream goroutine, forever. Both paths must give
// up after the idle limit and report a gateway timeout.
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
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != http.StatusGatewayTimeout {
					t.Fatalf("stalled body = %T %v, want a gateway timeout", err, err)
				}
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
