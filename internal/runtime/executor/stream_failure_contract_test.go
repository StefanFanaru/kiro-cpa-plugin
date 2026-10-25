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

// An empty assistant or reasoning delta sent message_start too. An
// upstream error after it was then no longer the first chunk, and CPA did not
// fail over.
func TestEmptyFirstEventKeepsErrorFirst(t *testing.T) {
	t.Parallel()

	exception := headerFrame("", stringHeader(":message-type", "exception"),
		stringHeader(":exception-type", "ThrottlingException"), stringHeader(":error-message", "Too many requests"))
	for name, empty := range map[string][]byte{
		"assistant": kiroEvent("assistantResponseEvent", `{"assistantResponseEvent":{"content":""}}`),
		"reasoning": kiroEvent("reasoningContentEvent", `{"reasoningContentEvent":{"text":""}}`),
	} {
		_, chunks := collectStream(t, kiroEventStream(empty, exception))
		if len(chunks) == 0 || chunks[0].Err == nil {
			first := "none"
			if len(chunks) > 0 {
				first = eventData(chunks[0].Payload).Get("type").String()
			}
			t.Errorf("%s: first chunk is %q, want the error", name, first)
		}
	}
}

// Text or reasoning that arrives while a tool_use block is open would open a
// second block before the first is closed, and the remaining input fragments
// would go to the wrong index. Kiro has not been seen doing this, so the
// stream fails instead of guessing which block the events belong to.
func TestTextDuringOpenToolBlockFailsTheStream(t *testing.T) {
	t.Parallel()

	for name, interloper := range map[string][]byte{
		"text":      kiroEvent("assistantResponseEvent", `{"assistantResponseEvent":{"content":"hi"}}`),
		"reasoning": kiroEvent("reasoningContentEvent", `{"reasoningContentEvent":{"text":"hmm"}}`),
	} {
		body := kiroEventStream(
			kiroEvent("toolUseEvent", `{"toolUseEvent":{"toolUseId":"call_1","name":"write_file","input":"{\"path\":"}}`),
			interloper,
			kiroEvent("toolUseEvent", `{"toolUseEvent":{"toolUseId":"call_1","name":"write_file","input":"\"a.md\"}"}}`),
			kiroEvent("toolUseEvent", `{"toolUseEvent":{"toolUseId":"call_1","name":"write_file","stop":true}}`),
		)
		ok, chunks := collectStream(t, body)
		open := map[int64]string{}
		var problems []string
		var streamErr error
		for _, chunk := range chunks {
			if chunk.Err != nil {
				streamErr = chunk.Err
				continue
			}
			data := eventData(chunk.Payload)
			index := data.Get("index").Int()
			switch data.Get("type").String() {
			case "content_block_start":
				if len(open) > 0 {
					problems = append(problems, "start while another block is open")
				}
				open[index] = data.Get("content_block.type").String()
			case "content_block_delta":
				want := map[string]string{"input_json_delta": "tool_use", "text_delta": "text", "thinking_delta": "thinking"}[data.Get("delta.type").String()]
				if open[index] != want {
					problems = append(problems, data.Get("delta.type").String()+" on "+open[index]+" block")
				}
			case "content_block_stop":
				if _, ok := open[index]; !ok {
					problems = append(problems, "stop for a closed block")
				}
				delete(open, index)
			}
		}
		if len(problems) > 0 {
			t.Errorf("%s: block sequence broken: %v", name, problems)
		}
		if ok || streamErr == nil {
			t.Errorf("%s: ok=%v err=%v, want the stream to fail", name, ok, streamErr)
		}
	}
}
