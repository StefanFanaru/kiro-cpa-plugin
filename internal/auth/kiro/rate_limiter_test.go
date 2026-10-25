package kiro

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func newTestLimiter(min, max, suspend time.Duration) *RateLimiter {
	limiter := NewRateLimiterWithConfig(RateLimiterConfig{MinTokenInterval: min, MaxTokenInterval: max, SuspendCooldown: suspend})
	limiter.jitterPercent = 0
	limiter.rng = rand.New(rand.NewSource(1))
	return limiter
}

func TestRateLimiterAllowsEqualTokenIntervals(t *testing.T) {
	limiter := NewRateLimiterWithConfig(RateLimiterConfig{MinTokenInterval: time.Second, MaxTokenInterval: time.Second})
	limiter.rng = rand.New(rand.NewSource(1))
	if got := limiter.calculateInterval(); got <= 0 {
		t.Fatalf("equal token interval produced %v", got)
	}
}

func TestWaitForTokenPacesOneCredentialAndLeavesOthersAlone(t *testing.T) {
	limiter := newTestLimiter(60*time.Millisecond, 60*time.Millisecond, time.Hour)
	ctx := context.Background()
	if err := limiter.WaitForToken(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := limiter.WaitForToken(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Millisecond {
		t.Fatalf("another credential waited %v behind the first one", elapsed)
	}
	start = time.Now()
	if err := limiter.WaitForToken(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("second request on one credential waited only %v, want about 60ms", elapsed)
	}
}

func TestWaitForTokenReturnsWhenTheCallerGoesAway(t *testing.T) {
	limiter := newTestLimiter(10*time.Second, 10*time.Second, time.Hour)
	if err := limiter.WaitForToken(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := limiter.WaitForToken(ctx, "a")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForToken = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("WaitForToken kept sleeping for %v after the context ended", elapsed)
	}
}

func TestWaitForTokenReleasesSlotOfCancelledWaiter(t *testing.T) {
	limiter := newTestLimiter(time.Second, time.Second, time.Hour)
	if err := limiter.WaitForToken(context.Background(), "cred"); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_ = limiter.WaitForToken(ctx, "cred")
		}()
	}
	group.Wait()
	limiter.mu.Lock()
	ahead := time.Until(limiter.states["cred"].nextSlot)
	limiter.mu.Unlock()
	if ahead > 1500*time.Millisecond {
		t.Fatalf("nextSlot is %v in the future after 20 cancelled waiters; want <= ~1s", ahead.Round(time.Millisecond))
	}
}

func TestWaitForTokenKeepsQueueBehindLiveWaiterWhenEarlierOneCancels(t *testing.T) {
	limiter := newTestLimiter(time.Second, time.Second, time.Hour)
	if err := limiter.WaitForToken(context.Background(), "cred"); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	live, stopLive := context.WithCancel(context.Background())
	defer stopLive()
	gaveUp := make(chan struct{})
	go func() { defer close(gaveUp); _ = limiter.WaitForToken(cancelled, "cred") }()
	waitForReservations(t, limiter, "cred", 1)
	liveDone := make(chan struct{})
	go func() { defer close(liveDone); _ = limiter.WaitForToken(live, "cred") }()
	waitForReservations(t, limiter, "cred", 2)
	cancel()
	<-gaveUp
	limiter.mu.Lock()
	ahead := time.Until(limiter.states["cred"].nextSlot)
	limiter.mu.Unlock()
	// The live waiter holds the slot about 2s out; the next caller must queue
	// behind it, not take the slot the cancelled waiter gave up.
	if ahead < 2500*time.Millisecond {
		t.Fatalf("nextSlot is %v in the future; a new caller would jump ahead of the live waiter", ahead.Round(time.Millisecond))
	}
	stopLive()
	<-liveDone
}

func TestWaitForTokenKeepsGapWhenLiveWaiterIsServedAfterEarlierCancel(t *testing.T) {
	limiter := newTestLimiter(100*time.Millisecond, 100*time.Millisecond, time.Hour)
	if err := limiter.WaitForToken(context.Background(), "cred"); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan struct{})
	go func() { defer close(gaveUp); _ = limiter.WaitForToken(cancelled, "cred") }()
	waitForSlotAhead(t, limiter, "cred", 150*time.Millisecond)
	liveDone := make(chan error, 1)
	go func() { liveDone <- limiter.WaitForToken(context.Background(), "cred") }()
	waitForSlotAhead(t, limiter, "cred", 250*time.Millisecond)
	cancel()
	<-gaveUp
	if err := <-liveDone; err != nil {
		t.Fatalf("live wait: %v", err)
	}
	limiter.mu.Lock()
	ahead := time.Until(limiter.states["cred"].nextSlot)
	limiter.mu.Unlock()
	if ahead < 50*time.Millisecond {
		t.Fatalf("nextSlot is %v right after the live waiter was served; want about 100ms", ahead.Round(time.Millisecond))
	}
}

// waitForReservations polls until n waiters have reserved slots behind the
// first request on a 1s-interval limiter.
func waitForReservations(t *testing.T, limiter *RateLimiter, tokenKey string, n int) {
	t.Helper()
	waitForSlotAhead(t, limiter, tokenKey, time.Duration(n)*time.Second+500*time.Millisecond)
}

// waitForSlotAhead polls until the credential's next slot is more than want
// in the future.
func waitForSlotAhead(t *testing.T, limiter *RateLimiter, tokenKey string, want time.Duration) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		limiter.mu.Lock()
		ahead := time.Until(limiter.state(tokenKey).nextSlot)
		limiter.mu.Unlock()
		if ahead > want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("next slot never moved more than %v ahead", want)
}

func TestMarkUnavailableIsReportedUntilItExpires(t *testing.T) {
	limiter := newTestLimiter(time.Millisecond, time.Millisecond, time.Hour)
	if _, _, unavailable := limiter.TokenUnavailable("a"); unavailable {
		t.Fatal("fresh credential reported unavailable")
	}
	limiter.MarkUnavailable("a", "monthly limit reached", 50*time.Millisecond)
	reason, retryAfter, unavailable := limiter.TokenUnavailable("a")
	if !unavailable || reason != "monthly limit reached" {
		t.Fatalf("TokenUnavailable = %q, %v, %v", reason, retryAfter, unavailable)
	}
	if retryAfter <= 0 || retryAfter > 50*time.Millisecond {
		t.Fatalf("retryAfter = %v, want within (0, 50ms]", retryAfter)
	}
	if _, _, other := limiter.TokenUnavailable("b"); other {
		t.Fatal("an unrelated credential was taken out of rotation")
	}
	time.Sleep(60 * time.Millisecond)
	if _, _, still := limiter.TokenUnavailable("a"); still {
		t.Fatal("credential stayed unavailable after the cooldown elapsed")
	}
}

func TestMarkSuspendedUsesTheConfiguredCooldown(t *testing.T) {
	limiter := newTestLimiter(time.Millisecond, time.Millisecond, 80*time.Millisecond)
	limiter.MarkSuspended("a")
	reason, retryAfter, unavailable := limiter.TokenUnavailable("a")
	if !unavailable || reason != "account suspended" {
		t.Fatalf("TokenUnavailable = %q, %v, %v", reason, retryAfter, unavailable)
	}
	if retryAfter <= 0 || retryAfter > 80*time.Millisecond {
		t.Fatalf("retryAfter = %v, want the configured 80ms window", retryAfter)
	}
}

func TestConfigDefaultsFillUnsetFields(t *testing.T) {
	limiter := NewRateLimiterWithConfig(RateLimiterConfig{})
	if limiter.minTokenInterval != DefaultMinTokenInterval || limiter.maxTokenInterval != DefaultMaxTokenInterval || limiter.suspendCooldown != DefaultSuspendCooldown {
		t.Fatalf("defaults not applied: %+v", limiter)
	}
}
