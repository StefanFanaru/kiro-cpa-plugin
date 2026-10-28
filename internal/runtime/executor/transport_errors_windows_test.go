package executor

import (
	"golang.org/x/sys/windows"
	"net"
	"os"
	"testing"
)

func TestRetryableWindowsSocketErrors(t *testing.T) {
	for _, cause := range []error{windows.WSAECONNREFUSED, windows.WSAECONNRESET, windows.WSAECONNABORTED, windows.WSAETIMEDOUT, windows.WSAENETUNREACH, windows.WSAEHOSTUNREACH} {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connectex", cause)}
		if !isRetryableError(err) {
			t.Errorf("Windows socket error %v was not retryable", err)
		}
	}
}
