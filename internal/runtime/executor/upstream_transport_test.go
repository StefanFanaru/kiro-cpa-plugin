package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	kiroauth "github.com/nguyenha935/kiro-cpa-plugin/internal/auth/kiro"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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
