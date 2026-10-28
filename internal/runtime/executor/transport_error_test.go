package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestRetryableTransportErrorsUseValues(t *testing.T) {
	for _, cause := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH, io.EOF, io.ErrUnexpectedEOF} {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", cause)}
		if !isRetryableError(fmt.Errorf("request: %w", err)) {
			t.Errorf("wrapped %v was not retryable", cause)
		}
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, errors.New("connection refused"), errors.New("timeout in client input"), &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EINVAL}} {
		if isRetryableError(err) {
			t.Errorf("%v was retryable without a transient error value", err)
		}
	}
}
