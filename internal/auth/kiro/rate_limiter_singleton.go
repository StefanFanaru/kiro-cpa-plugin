package kiro

import (
	log "github.com/sirupsen/logrus"
)

var globalRateLimiter = NewRateLimiter()

// GetGlobalRateLimiter returns the process-wide limiter.
func GetGlobalRateLimiter() *RateLimiter {
	return globalRateLimiter
}

// ConfigureGlobalRateLimiter applies plugin config to the process-wide
// limiter in place, keeping existing credential state.
func ConfigureGlobalRateLimiter(cfg RateLimiterConfig) {
	globalRateLimiter.Configure(cfg)
	log.Info("kiro: global RateLimiter configuration updated")
}
