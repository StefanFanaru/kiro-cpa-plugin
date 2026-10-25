package executor

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// Kiro streams tool input as JSON fragments. Holding them until the call is
// complete kept a client silent for the whole generation of a large tool input
// (a file Write took 15s with no event), so each fragment is forwarded as it
// arrives, the way Anthropic streams input_json_delta.
func TestStreamForwardsToolInputBeforeTheCallCompletes(t *testing.T) {
	t.Parallel()

	upstream, writer := io.Pipe()
	defer upstream.Close()
	out := make(chan cliproxyexecutor.StreamChunk, 32)
	go func() {
		defer close(out)
		(&KiroExecutor{}).streamToChannel(context.Background(), upstream, out, sdktranslator.FormatClaude, "claude-opus-5", nil, nil, 0)
	}()
	go func() {
		_, _ = writer.Write(kiroEvent("toolUseEvent", `{"toolUseEvent":{"toolUseId":"call_1","name":"write_file","input":"{\"path\":\"a.md\","}}`))
	}()

	timeout := time.After(2 * time.Second)
	sawStart := false
	for {
		select {
		case chunk := <-out:
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			data := eventData(chunk.Payload)
			if data.Get("content_block.type").String() == "tool_use" {
				if data.Get("content_block.id").String() != "call_1" || data.Get("content_block.name").String() != "write_file" {
					t.Fatalf("tool block = %s", data.Raw)
				}
				sawStart = true
			}
			if data.Get("delta.type").String() == "input_json_delta" {
				if !sawStart {
					t.Fatal("input_json_delta before its tool_use block")
				}
				if got := data.Get("delta.partial_json").String(); got != `{"path":"a.md",` {
					t.Fatalf("partial_json = %q", got)
				}
				_ = writer.Close()
				return
			}
		case <-timeout:
			t.Fatal("no input_json_delta reached the client before the call completed")
		}
	}
}

func TestStreamedToolInputFragmentsFormTheCompleteInput(t *testing.T) {
	t.Parallel()

	body := kiroEventStream(
		kiroEvent("toolUseEvent", `{"toolUseEvent":{"toolUseId":"call_1","name":"write_file","input":"{\"path\":\"a.md\","}}`),
		kiroEvent("toolUseEvent", `{"toolUseEvent":{"toolUseId":"call_1","name":"write_file","input":"\"content\":\"hi\"}"}}`),
		kiroEvent("toolUseEvent", `{"toolUseEvent":{"toolUseId":"call_1","name":"write_file","stop":true}}`),
		kiroEvent("toolUseEvent", `{"toolUseEvent":{"toolUseId":"call_2","name":"list_files","stop":true}}`),
	)
	out := make(chan cliproxyexecutor.StreamChunk, 64)
	if ok := (&KiroExecutor{}).streamToChannel(context.Background(), strings.NewReader(string(body)), out, sdktranslator.FormatClaude, "claude-opus-5", nil, nil, 0); !ok {
		t.Fatal("stream failed")
	}
	close(out)

	inputs := map[int64]*strings.Builder{}
	var order []string
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		data := eventData(chunk.Payload)
		switch data.Get("type").String() {
		case "content_block_start":
			order = append(order, "start:"+data.Get("content_block.id").String())
			inputs[data.Get("index").Int()] = &strings.Builder{}
		case "content_block_delta":
			inputs[data.Get("index").Int()].WriteString(data.Get("delta.partial_json").String())
		case "content_block_stop":
			order = append(order, "stop")
		case "message_delta":
			if got := data.Get("delta.stop_reason").String(); got != "tool_use" {
				t.Fatalf("stop_reason = %q", got)
			}
		}
	}
	if got := strings.Join(order, ","); got != "start:call_1,stop,start:call_2,stop" {
		t.Fatalf("block order = %s", got)
	}
	if got := gjson.Parse(inputs[0].String()); got.Get("path").String() != "a.md" || got.Get("content").String() != "hi" {
		t.Fatalf("first input = %s", inputs[0].String())
	}
	if got := inputs[1].String(); got != "{}" {
		t.Fatalf("zero-argument input = %q", got)
	}
}
