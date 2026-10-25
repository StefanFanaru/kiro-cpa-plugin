package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	kiroauth "github.com/nguyenha935/kiro-cpa-plugin/internal/auth/kiro"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// A credential is valid on exactly one runtime. The previous behaviour returned
// all three surfaces ordered by a guess, which meant a rejection on the first
// surface was retried against services the credential was never valid on.
func TestKiroResolvesExactlyOneEndpointPerAuthSurface(t *testing.T) {
	tests := map[string]string{
		"builder-id":   "https://runtime.us-east-1.kiro.dev/generateAssistantResponse",
		"social":       "https://runtime.us-east-1.kiro.dev/generateAssistantResponse",
		"idc":          "https://runtime.us-east-1.kiro.dev/generateAssistantResponse",
		"external_idp": "https://runtime.us-east-1.kiro.dev/generateAssistantResponse",
		"api_key":      "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
	}
	for method, want := range tests {
		auth := &cliproxyauth.Auth{Metadata: map[string]any{"auth_method": method, "region": "us-east-1"}}
		got, err := kiroEndpointFor(auth)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if got.URL != want {
			t.Fatalf("%s endpoint = %s, want %s", method, got.URL, want)
		}
	}
}

// An enterprise profile provisioned outside the login region must drive the
// runtime region. Leading with the login region sent every request to a host
// that does not serve the profile.
func TestKiroRuntimeFollowsProfileRegionNotLoginRegion(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"auth_method": "idc",
		"region":      "us-east-1",
		"profile_arn": "arn:aws:codewhisperer:eu-central-1:111122223333:profile/EXAMPLEPROFILE",
	}}
	got, err := kiroEndpointFor(auth)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://runtime.eu-central-1.kiro.dev/generateAssistantResponse"; got.URL != want {
		t.Fatalf("endpoint = %s, want %s", got.URL, want)
	}
}

// codewhisperer.<region>.amazonaws.com has no DNS record outside us-east-1 and is
// not the surface Kiro uses. No credential may resolve to it.
func TestKiroNeverResolvesToTheCodeWhispererHost(t *testing.T) {
	for _, method := range []string{"builder-id", "social", "idc", "external_idp", "imported", "api_key"} {
		for _, region := range []string{"us-east-1", "eu-central-1"} {
			auth := &cliproxyauth.Auth{Metadata: map[string]any{"auth_method": method, "region": region}}
			endpoint, err := kiroEndpointFor(auth)
			if err != nil {
				t.Fatalf("%s in %s: %v", method, region, err)
			}
			if strings.Contains(endpoint.URL, "codewhisperer.") {
				t.Fatalf("%s in %s resolved to %s", method, region, endpoint.URL)
			}
		}
	}
}

func TestIsCoolingDisabledUsesCredentialOverride(t *testing.T) {
	if !isCoolingDisabled(&cliproxyauth.Auth{Metadata: map[string]any{"disable_cooling": true}}) {
		t.Fatal("credential disable_cooling=true was not honored")
	}
	if isCoolingDisabled(&cliproxyauth.Auth{Metadata: map[string]any{"disable_cooling": false}}) {
		t.Fatal("credential disable_cooling=false incorrectly disabled plugin limiter")
	}
	if !isCoolingDisabled(&cliproxyauth.Auth{Attributes: map[string]string{"disable-cooling": "true"}}) {
		t.Fatal("legacy disable-cooling attribute was not honored")
	}
}

func TestAPIKeyUsesConfiguredRegion(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"auth_method": "api_key", "region": "eu-west-1"}}
	if got := resolveKiroAccount(auth).Region; got != "eu-west-1" {
		t.Fatalf("API-key region = %q, want eu-west-1", got)
	}
	endpoint, err := kiroEndpointFor(auth)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(endpoint.URL, "q.eu-west-1.amazonaws.com") {
		t.Fatalf("API-key endpoint ignored configured region: %s", endpoint.URL)
	}
}

func TestBuilderIDGenerateIntegration(t *testing.T) {
	path := os.Getenv("KIRO_BUILDER_ID_INTEGRATION_TOKEN_PATH")
	if path == "" {
		t.Skip("KIRO_BUILDER_ID_INTEGRATION_TOKEN_PATH is not set")
	}
	token, err := kiroauth.LoadKiroTokenFromPath(path)
	if err != nil {
		t.Fatalf("load Builder ID credential: %v", err)
	}
	auth := &cliproxyauth.Auth{
		ID: "kiro-builder-id-integration", Provider: "kiro", FileName: path,
		Metadata: map[string]any{
			"access_token": token.AccessToken, "refresh_token": token.RefreshToken,
			"auth_method": token.AuthMethod, "client_id": token.ClientID,
			"client_secret": token.ClientSecret, "expires_at": token.ExpiresAt,
			"region": token.Region,
		},
		Attributes: map[string]string{"auth_method": token.AuthMethod, "region": token.Region, "path": path},
	}
	body := []byte("{\"model\":\"claude-haiku-4.5\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly OK.\"}],\"max_tokens\":16}")
	response, err := NewKiroExecutor(nil).Execute(t.Context(), auth, cliproxyexecutor.Request{
		Model: "claude-haiku-4.5", Payload: body, Format: sdktranslator.FromString("openai"),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: body,
	})
	if err != nil {
		t.Fatalf("execute Builder ID request: %v", err)
	}
	var payload map[string]any
	if json.Unmarshal(response.Payload, &payload) != nil || payload["choices"] == nil {
		t.Fatalf("Builder ID returned an invalid OpenAI response")
	}
}

func TestQuotaErrorsStopAtCredentialBoundary(t *testing.T) {
	originalClient := kiroHTTPClientFor
	t.Cleanup(func() { kiroHTTPClientFor = originalClient })
	body := []byte("{\"model\":\"claude-haiku-4.5\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply OK.\"}],\"max_tokens\":16}")

	for _, test := range []struct {
		name   string
		status int
		stream bool
		want   int
	}{
		{name: "non-stream 429", status: http.StatusTooManyRequests, want: http.StatusTooManyRequests},
		{name: "stream 429", status: http.StatusTooManyRequests, stream: true, want: http.StatusTooManyRequests},
		{name: "non-stream 402", status: http.StatusPaymentRequired, want: http.StatusPaymentRequired},
		{name: "stream 402", status: http.StatusPaymentRequired, stream: true, want: http.StatusPaymentRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			kiroHTTPClientFor = func(context.Context, *config.Config, *cliproxyauth.Auth, time.Duration) *http.Client {
				return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					requests++
					return &http.Response{
						StatusCode: test.status, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader("{\"message\":\"quota\"}")),
					}, nil
				})}
			}
			authID := strings.ReplaceAll(test.name, " ", "-")
			auth := &cliproxyauth.Auth{ID: authID, Metadata: map[string]any{
				"access_token": "token", "auth_method": "builder-id", "region": "us-east-1",
			}}
			request := cliproxyexecutor.Request{Model: "claude-haiku-4.5", Payload: body, Format: sdktranslator.FromString("openai")}
			options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: body}
			var err error
			if test.stream {
				_, err = NewKiroExecutor(nil).ExecuteStream(t.Context(), auth, request, options)
			} else {
				_, err = NewKiroExecutor(nil).Execute(t.Context(), auth, request, options)
			}
			statusErr, ok := err.(interface{ StatusCode() int })
			if !ok || statusErr.StatusCode() != test.want {
				t.Fatalf("quota error = %T %v, want HTTP %d", err, err, test.want)
			}
			if requests != 1 {
				t.Fatalf("quota error made %d upstream requests, want 1", requests)
			}
		})
	}
}

// stubUpstream routes every executor request to handler for the test's duration
// and shrinks the per-credential pacing so repeated calls do not sleep.
func stubUpstream(t *testing.T, handler func(*http.Request) (*http.Response, error)) {
	t.Helper()
	original := kiroHTTPClientFor
	kiroauth.ConfigureGlobalRateLimiter(kiroauth.RateLimiterConfig{MinTokenInterval: time.Millisecond, MaxTokenInterval: 2 * time.Millisecond})
	t.Cleanup(func() {
		kiroHTTPClientFor = original
		kiroauth.ConfigureGlobalRateLimiter(kiroauth.RateLimiterConfig{})
	})
	kiroHTTPClientFor = func(context.Context, *config.Config, *cliproxyauth.Auth, time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(handler)}
	}
}

func upstreamStatus(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func builderIDAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Metadata: map[string]any{
		"access_token": "token", "auth_method": "builder-id", "region": "us-east-1",
	}}
}

var executorTestRequest = func() (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	body := []byte("{\"model\":\"claude-haiku-4.5\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply OK.\"}],\"max_tokens\":16}")
	return cliproxyexecutor.Request{Model: "claude-haiku-4.5", Payload: body, Format: sdktranslator.FromString("openai")},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: body}
}

// A 429 belongs to CPA's backoff ladder. The plugin forwards the upstream
// Retry-After window when Kiro sends one and otherwise leaves it unset, and it
// never parks the credential itself, so the next request still goes upstream.
func TestUpstream429IsForwardedWithoutPluginCooldown(t *testing.T) {
	requests := 0
	stubUpstream(t, func(*http.Request) (*http.Response, error) {
		requests++
		header := http.Header{}
		if requests == 1 {
			header.Set("Retry-After", "7")
		}
		return upstreamStatus(http.StatusTooManyRequests, header, "{\"message\":\"slow down\"}"), nil
	})
	auth := builderIDAuth("upstream-429")
	request, options := executorTestRequest()

	_, err := NewKiroExecutor(nil).Execute(t.Context(), auth, request, options)
	retryAfter, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryAfter.RetryAfter() == nil || *retryAfter.RetryAfter() != 7*time.Second {
		t.Fatalf("first 429 = %T %v, want Retry-After 7s forwarded", err, err)
	}

	_, err = NewKiroExecutor(nil).Execute(t.Context(), auth, request, options)
	retryAfter, ok = err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryAfter.RetryAfter() != nil {
		t.Fatalf("second 429 = %T %v, want no retry-after when upstream sends none", err, err)
	}
	if requests != 2 {
		t.Fatalf("plugin made %d upstream requests, want 2 (no local 429 cooldown)", requests)
	}
}

// A 402 monthly limit and a 403 suspension are the two conditions the plugin
// must remember itself: the host's ladder would retry within seconds and the
// retry-after does not cross the RPC boundary. The credential is refused
// locally until the window ends, with the status CPA cools longest for: 402
// for the monthly limit, 403 for a suspension.
func TestMonthlyLimitAndSuspensionParkTheCredentialLocally(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		reason string
		local  int
	}{
		{name: "402 monthly limit", status: http.StatusPaymentRequired, body: "{\"message\":\"MONTHLY_REQUEST_COUNT\"}", reason: "monthly limit reached", local: http.StatusPaymentRequired},
		{name: "403 suspended", status: http.StatusForbidden, body: "{\"reason\":\"TEMPORARILY_SUSPENDED\"}", reason: "account suspended", local: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			stubUpstream(t, func(*http.Request) (*http.Response, error) {
				requests++
				return upstreamStatus(test.status, nil, test.body), nil
			})
			auth := builderIDAuth(strings.ReplaceAll(test.name, " ", "-"))
			request, options := executorTestRequest()

			if _, err := NewKiroExecutor(nil).Execute(t.Context(), auth, request, options); err == nil {
				t.Fatal("upstream rejection returned no error")
			}
			_, err := NewKiroExecutor(nil).ExecuteStream(t.Context(), auth, request, options)
			statusErr, ok := err.(interface{ StatusCode() int })
			if !ok || statusErr.StatusCode() != test.local || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("parked credential = %T %v, want local %d mentioning %q", err, err, test.local, test.reason)
			}
			retryAfter, ok := err.(interface{ RetryAfter() *time.Duration })
			if !ok || retryAfter.RetryAfter() == nil || *retryAfter.RetryAfter() <= 0 {
				t.Fatalf("parked credential carries no retry-after: %v", err)
			}
			if requests != 1 {
				t.Fatalf("parked credential reached upstream %d times, want 1", requests)
			}

			disabled := builderIDAuth(auth.ID + "-cooling-off")
			disabled.Attributes = map[string]string{"disable_cooling": "true"}
			NewKiroExecutor(nil).Execute(t.Context(), disabled, request, options)
			NewKiroExecutor(nil).Execute(t.Context(), disabled, request, options)
			if requests != 3 {
				t.Fatalf("disable_cooling credential reached upstream %d times, want 3", requests)
			}
		})
	}
}

// A request the plugin refuses on its own (expired token, unsupported
// request) never reaches Kiro, so it must not take the credential's pacing
// slot: reserving the slot first made the next real request on that credential
// wait out a 1-2s gap that protected nothing.
func TestLocallyRefusedRequestDoesNotTakeAPacingSlot(t *testing.T) {
	expiredAuth := func(id string) *cliproxyauth.Auth {
		auth := builderIDAuth(id)
		auth.Metadata["expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		return auth
	}
	unsupported := func() (cliproxyexecutor.Request, cliproxyexecutor.Options) {
		request, options := executorTestRequest()
		body := []byte("{\"model\":\"claude-haiku-4.5\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply OK.\"}],\"response_format\":{\"type\":\"json_object\"}}")
		request.Payload, options.OriginalRequest = body, body
		return request, options
	}
	for _, test := range []struct {
		name    string
		auth    func(string) *cliproxyauth.Auth
		request func() (cliproxyexecutor.Request, cliproxyexecutor.Options)
		want    int
	}{
		{name: "expired token", auth: expiredAuth, request: executorTestRequest, want: http.StatusUnauthorized},
		{name: "unsupported request", auth: builderIDAuth, request: unsupported, want: http.StatusBadRequest},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s stream=%v", test.name, stream), func(t *testing.T) {
				stubUpstream(t, func(*http.Request) (*http.Response, error) {
					return upstreamStatus(http.StatusOK, nil, ""), nil
				})
				kiroauth.ConfigureGlobalRateLimiter(kiroauth.RateLimiterConfig{MinTokenInterval: 2 * time.Second, MaxTokenInterval: 2 * time.Second})
				id := fmt.Sprintf("refused-%s-%v", strings.ReplaceAll(test.name, " ", "-"), stream)
				execute := func(auth *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options) error {
					if stream {
						_, err := NewKiroExecutor(nil).ExecuteStream(t.Context(), auth, request, options)
						return err
					}
					_, err := NewKiroExecutor(nil).Execute(t.Context(), auth, request, options)
					return err
				}

				request, options := test.request()
				err := execute(test.auth(id), request, options)
				if status, ok := err.(interface{ StatusCode() int }); !ok || status.StatusCode() != test.want {
					t.Fatalf("refused request = %T %v, want %d", err, err, test.want)
				}
				request, options = executorTestRequest()
				start := time.Now()
				_ = execute(builderIDAuth(id), request, options)
				if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
					t.Fatalf("next request on the credential waited %v for a slot the refused one took", elapsed)
				}
			})
		}
	}
}

// Retry sleeps end with the caller: a client that disconnects while the plugin
// waits out a 503 gets 499 immediately instead of holding the credential.
func TestRetryBackoffStopsWhenTheClientDisconnects(t *testing.T) {
	stubUpstream(t, func(*http.Request) (*http.Response, error) {
		return upstreamStatus(http.StatusServiceUnavailable, nil, "{\"message\":\"busy\"}"), nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	request, options := executorTestRequest()
	start := time.Now()
	_, err := NewKiroExecutor(nil).Execute(ctx, builderIDAuth("backoff-cancel"), request, options)
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != 499 {
		t.Fatalf("canceled retry = %T %v, want 499", err, err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("retry kept sleeping for %v after the client left", elapsed)
	}
}

// The retry budget is the loop bound: three retryable 5xx in a row use every
// attempt and the caller receives the last upstream status and body, not a
// generic 503. Both request paths share the rule.
func TestRetryExhaustionReturnsTheLastUpstreamStatus(t *testing.T) {
	originalDelay := calculateRetryDelay
	calculateRetryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { calculateRetryDelay = originalDelay })

	statuses := []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	for _, test := range []struct {
		name    string
		execute func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) error
	}{
		{name: "non-stream", execute: func(ctx context.Context, auth *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options) error {
			_, err := NewKiroExecutor(nil).Execute(ctx, auth, request, options)
			return err
		}},
		{name: "stream", execute: func(ctx context.Context, auth *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options) error {
			_, err := NewKiroExecutor(nil).ExecuteStream(ctx, auth, request, options)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			stubUpstream(t, func(*http.Request) (*http.Response, error) {
				status := statuses[requests]
				requests++
				return upstreamStatus(status, nil, fmt.Sprintf("{\"message\":\"upstream %d\"}", status)), nil
			})
			request, options := executorTestRequest()

			err := test.execute(t.Context(), builderIDAuth("retry-exhausted-"+test.name), request, options)
			statusErr, ok := err.(interface{ StatusCode() int })
			if !ok || statusErr.StatusCode() != http.StatusGatewayTimeout || err.Error() != "upstream 504" {
				t.Fatalf("exhausted retries = %T %v, want the final 504 with its body", err, err)
			}
			if requests != len(statuses) {
				t.Fatalf("plugin made %d upstream requests, want %d", requests, len(statuses))
			}
		})
	}
}

// A 5xx with Retry-After is Kiro naming when the credential may come back.
// Retrying it locally on the same credential spent the window on requests Kiro
// had already refused and then dropped the Retry-After; it goes straight back to
// CPA with the window, like a 429.
func TestServerErrorWithRetryAfterIsForwardedWithoutLocalRetry(t *testing.T) {
	originalDelay := calculateRetryDelay
	delays := 0
	calculateRetryDelay = func(int) time.Duration { delays++; return time.Millisecond }
	t.Cleanup(func() { calculateRetryDelay = originalDelay })

	for _, test := range []struct {
		name    string
		execute func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) error
	}{
		{name: "non-stream", execute: func(ctx context.Context, auth *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options) error {
			_, err := NewKiroExecutor(nil).Execute(ctx, auth, request, options)
			return err
		}},
		{name: "stream", execute: func(ctx context.Context, auth *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options) error {
			_, err := NewKiroExecutor(nil).ExecuteStream(ctx, auth, request, options)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			delays = 0
			requests := 0
			stubUpstream(t, func(*http.Request) (*http.Response, error) {
				requests++
				return upstreamStatus(http.StatusServiceUnavailable, http.Header{"Retry-After": []string{"120"}}, "{\"message\":\"busy\"}"), nil
			})
			request, options := executorTestRequest()

			err := test.execute(t.Context(), builderIDAuth("retry-after-5xx-"+test.name), request, options)
			var retryAfter *time.Duration
			if carrier, ok := err.(interface{ RetryAfter() *time.Duration }); ok {
				retryAfter = carrier.RetryAfter()
			}
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusServiceUnavailable || retryAfter == nil || *retryAfter != 2*time.Minute {
				t.Fatalf("5xx with Retry-After = %T %v (retry-after %v), want 503 with 2m", err, err, retryAfter)
			}
			if requests != 1 || delays != 0 {
				t.Fatalf("plugin made %d upstream requests and %d local backoffs, want 1 and 0", requests, delays)
			}
		})
	}
}

// With a proxy configured, the cache must share the transport and nothing
// else: a stream client requested after a 120s non-stream client carries no
// timeout, and both reuse the same connection pool.
func TestProxyClientCacheSharesTransportNotTimeout(t *testing.T) {
	cfg := &config.Config{}
	cfg.ProxyURL = "http://127.0.0.1:9"
	ctx := context.Background()

	nonStream := newKiroHTTPClientWithPooling(ctx, cfg, nil, 120*time.Second)
	stream := newKiroHTTPClientWithPooling(ctx, cfg, nil, 0)
	if nonStream.Timeout != 120*time.Second || stream.Timeout != 0 {
		t.Fatalf("timeouts = %v then %v, want 120s then none", nonStream.Timeout, stream.Timeout)
	}
	if nonStream.Transport == nil || nonStream.Transport != stream.Transport {
		t.Fatalf("proxy transport not shared: %p vs %p", nonStream.Transport, stream.Transport)
	}
	if nonStream == stream {
		t.Fatal("cache handed out the same client to both paths")
	}
}

func TestRetryAfterHeaderParsesSecondsAndDates(t *testing.T) {
	header := http.Header{}
	if got := retryAfterHeader(header); got != nil {
		t.Fatalf("absent header = %v, want nil", *got)
	}
	header.Set("Retry-After", "120")
	if got := retryAfterHeader(header); got == nil || *got != 2*time.Minute {
		t.Fatalf("delay-seconds = %v, want 2m", got)
	}
	header.Set("Retry-After", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	if got := retryAfterHeader(header); got == nil || *got < 59*time.Minute || *got > time.Hour {
		t.Fatalf("HTTP-date = %v, want about 1h", got)
	}
	for _, bad := range []string{"soon", "-5", "0"} {
		header.Set("Retry-After", bad)
		if got := retryAfterHeader(header); got != nil {
			t.Fatalf("Retry-After %q = %v, want nil", bad, *got)
		}
	}
}

func TestKiroAuthorizationTokenTypeMatchesCredential(t *testing.T) {
	tests := map[string]string{"builder-id": "", "idc": "", "api_key": "API_KEY", "external_idp": "EXTERNAL_IDP"}
	for method, want := range tests {
		req, _ := http.NewRequest(http.MethodPost, "https://example.test", strings.NewReader(""))
		auth := &cliproxyauth.Auth{Metadata: map[string]any{"auth_method": method}}
		setKiroAuthorization(req, auth, "secret")
		if got := req.Header.Get("TokenType"); got != want {
			t.Fatalf("%s TokenType = %q, want %q", method, got, want)
		}
	}
}

func TestAPIKeyAuthErrorsDoNotEnterOAuthRefresh(t *testing.T) {
	originalClient := kiroHTTPClientFor
	t.Cleanup(func() { kiroHTTPClientFor = originalClient })
	body := []byte("{\"model\":\"claude-haiku-4.5\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply OK.\"}],\"max_tokens\":16}")

	for _, test := range []struct {
		name   string
		status int
		stream bool
	}{
		{name: "non-stream 401", status: http.StatusUnauthorized},
		{name: "stream 401", status: http.StatusUnauthorized, stream: true},
		{name: "non-stream 403", status: http.StatusForbidden},
		{name: "stream 403", status: http.StatusForbidden, stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			kiroHTTPClientFor = func(context.Context, *config.Config, *cliproxyauth.Auth, time.Duration) *http.Client {
				return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					requests++
					if request.Header.Get("TokenType") != "API_KEY" {
						t.Fatalf("API-key request TokenType = %q", request.Header.Get("TokenType"))
					}
					return &http.Response{
						StatusCode: test.status, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader("{\"message\":\"invalid API key\"}")),
					}, nil
				})}
			}
			auth := &cliproxyauth.Auth{ID: strings.ReplaceAll(test.name, " ", "-"), Metadata: map[string]any{
				"access_token": "ksk-test", "auth_method": "api_key", "auth_kind": "apikey", "region": "us-east-1",
			}}
			request := cliproxyexecutor.Request{Model: "claude-haiku-4.5", Payload: body, Format: sdktranslator.FromString("openai")}
			options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: body}
			var err error
			if test.stream {
				_, err = NewKiroExecutor(nil).ExecuteStream(t.Context(), auth, request, options)
			} else {
				_, err = NewKiroExecutor(nil).Execute(t.Context(), auth, request, options)
			}
			statusError, ok := err.(interface{ StatusCode() int })
			if !ok || statusError.StatusCode() != test.status {
				t.Fatalf("API-key auth error = %T %v, want HTTP %d", err, err, test.status)
			}
			if requests != 1 {
				t.Fatalf("API-key auth error made %d upstream requests, want 1", requests)
			}
		})
	}
}

// The local refusal status decides CPA's cooldown, because CPA cannot read the
// plugin's retry-after. Unknown reasons keep the 429 ladder.
func TestUnavailableStatusByReason(t *testing.T) {
	for reason, want := range map[string]int{
		kiroauth.ReasonMonthlyLimit: http.StatusPaymentRequired,
		kiroauth.ReasonSuspended:    http.StatusForbidden,
		"something else":            http.StatusTooManyRequests,
	} {
		if got := unavailableStatus(reason); got != want {
			t.Fatalf("unavailableStatus(%q) = %d, want %d", reason, got, want)
		}
	}
}

// A non-stream request reads the whole generation before it returns, so a
// client timeout caps generation length: long Opus runs failed at 120s and
// CPA retried them to 480s. Only the caller's context may bound the request.
func TestNonStreamClientHasNoTimeout(t *testing.T) {
	original := kiroHTTPClientFor
	t.Cleanup(func() { kiroHTTPClientFor = original })
	var timeouts []time.Duration
	kiroHTTPClientFor = func(_ context.Context, _ *config.Config, _ *cliproxyauth.Auth, timeout time.Duration) *http.Client {
		timeouts = append(timeouts, timeout)
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return upstreamStatus(http.StatusTooManyRequests, nil, "{\"message\":\"stop\"}"), nil
		})}
	}
	request, options := executorTestRequest()

	_, _ = NewKiroExecutor(nil).Execute(t.Context(), builderIDAuth("non-stream-timeout"), request, options)
	if len(timeouts) != 1 || timeouts[0] != 0 {
		t.Fatalf("non-stream client timeouts = %v, want [0]", timeouts)
	}
}
