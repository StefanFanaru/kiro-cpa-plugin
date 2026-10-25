package executor

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func collectStream(t *testing.T, body []byte) (bool, []cliproxyexecutor.StreamChunk) {
	t.Helper()
	out := make(chan cliproxyexecutor.StreamChunk, 128)
	ok := (&KiroExecutor{}).streamToChannel(context.Background(), bytes.NewReader(body), out, sdktranslator.FormatClaude, "claude-opus-5", nil, nil, 0)
	close(out)
	var chunks []cliproxyexecutor.StreamChunk
	for chunk := range out {
		chunks = append(chunks, chunk)
	}
	return ok, chunks
}

// AWS sends a throttle as a frame whose :message-type header is "exception",
// naming the exception and its message in headers, with or without a payload.
// Such a frame has no :event-type, so it used to be skipped as an unknown
// event: the response closed as a clean end_turn and the credential was
// credited with a success instead of a 429.
func TestExceptionFrameFailsTheStream(t *testing.T) {
	t.Parallel()

	for name, payload := range map[string]string{"with payload": `{"message":"Too many requests"}`, "no payload": ""} {
		exception := headerFrame(payload, stringHeader(":message-type", "exception"),
			stringHeader(":exception-type", "ThrottlingException"), stringHeader(":error-message", "Too many requests"))
		body := kiroEventStream(kiroEvent("assistantResponseEvent", `{"assistantResponseEvent":{"content":"partial"}}`), exception)
		ok, chunks := collectStream(t, body)
		var streamErr error
		var sawStop bool
		for _, chunk := range chunks {
			if chunk.Err != nil {
				streamErr = chunk.Err
			} else if eventData(chunk.Payload).Get("type").String() == "message_stop" {
				sawStop = true
			}
		}
		if ok || streamErr == nil || sawStop {
			t.Errorf("stream %s: ok=%v err=%v sawMessageStop=%v, want failure", name, ok, streamErr, sawStop)
		}
		if status, _ := streamErr.(interface{ StatusCode() int }); status == nil || status.StatusCode() != http.StatusTooManyRequests {
			t.Errorf("stream %s: error = %v, want status 429", name, streamErr)
		}
		_, _, _, _, _, _, err := (&KiroExecutor{}).parseEventStream(bytes.NewReader(body))
		if status, _ := err.(interface{ StatusCode() int }); status == nil || status.StatusCode() != http.StatusTooManyRequests {
			t.Errorf("non-stream %s: parseEventStream error = %v, want status 429", name, err)
		}
	}
}

// CPA fails over to another credential only when the error arrives before the
// first payload chunk. message_start went out before the event was looked at,
// so an upstream error on the first frame reached the client as a stream that
// had already started, and the request failed instead of moving on.
func TestFirstFrameErrorIsTheFirstChunk(t *testing.T) {
	t.Parallel()

	bodies := map[string][]byte{
		"exception frame": headerFrame("", stringHeader(":message-type", "exception"),
			stringHeader(":exception-type", "ThrottlingException"), stringHeader(":error-message", "Too many requests")),
	}
	for _, eventType := range []string{"internalServerException", "exception", "error"} {
		bodies[eventType] = kiroEvent(eventType, `{"message":"boom"}`)
	}
	for name, body := range bodies {
		_, chunks := collectStream(t, body)
		if len(chunks) == 0 || chunks[0].Err == nil {
			first := "none"
			if len(chunks) > 0 {
				first = eventData(chunks[0].Payload).Get("type").String()
			}
			t.Errorf("%s: first chunk is %q, want the error", name, first)
		}
	}
}
