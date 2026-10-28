//go:build !windows

package executor

func isRetryablePlatformSocketError(error) bool { return false }
