package executor

import (
	"errors"
	"golang.org/x/sys/windows"
)

func isRetryablePlatformSocketError(err error) bool {
	for _, cause := range []error{windows.WSAECONNRESET, windows.WSAECONNREFUSED, windows.WSAECONNABORTED, windows.WSAETIMEDOUT, windows.WSAENETUNREACH, windows.WSAEHOSTUNREACH} {
		if errors.Is(err, cause) {
			return true
		}
	}
	return false
}
