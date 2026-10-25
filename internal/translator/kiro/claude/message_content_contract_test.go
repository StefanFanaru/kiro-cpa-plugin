package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nguyenha935/kiro-cpa-plugin/internal/modelcapabilities"
)

func TestBuildKiroPayloadKeepsToolOnlyTurnContentEmpty(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{"role":"user","content":"Run pwd"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"pwd"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}
		],
		"tools": [{"name":"exec_command","description":"Run a command","input_schema":{"type":"object"}}]
	}`)
	raw, _ := BuildKiroPayload(body, "claude-opus-5", "profile", "AI_EDITOR", modelcapabilities.Capability{}, "")

	var payload KiroPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload.ConversationState.History[1].AssistantResponseMessage.Content; got != "" {
		t.Fatalf("assistant tool-only content = %q, want empty", got)
	}
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.Content; got != "" {
		t.Fatalf("tool-result-only content = %q, want empty", got)
	}
}

func TestBuildKiroPayloadFlattensUndeclaredHistoricalTool(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"messages":[
			{"role":"user","content":"Run pwd"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_old","name":"OldTool","input":{"cmd":"pwd"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_old","content":"ok"}]}
		],
		"tools":[{"name":"NewTool","description":"Replacement tool","input_schema":{"type":"object"}}]
	}`)
	raw, _ := BuildKiroPayload(body, "claude-opus-5", "profile", "AI_EDITOR", modelcapabilities.Capability{}, "")
	var payload KiroPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if len(assistant.ToolUses) != 0 || !strings.Contains(assistant.Content, `<tool_use id="call_old" name="OldTool">`) {
		t.Fatalf("undeclared tool use was not flattened: %#v", assistant)
	}
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("structured tool results were retained: %#v", current.UserInputMessageContext)
	}
	if !strings.Contains(current.Content, `<tool_result id="call_old" status="success">`) {
		t.Fatalf("tool result text was not preserved: %q", current.Content)
	}
}

func TestBuildKiroPayloadAnchorsSystemInstructionsToFirstUserTurn(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"system": [{"type":"text","text":"Follow the repository rules."}],
		"messages": [
			{"role":"user","content":"Run pwd"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"pwd"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}
		],
		"tools":[{"name":"exec_command","description":"Run a command","input_schema":{"type":"object"}}]
	}`)
	raw, _ := BuildKiroPayload(body, "claude-opus-5", "profile", "AI_EDITOR", modelcapabilities.Capability{}, "")

	var payload KiroPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload.ConversationState.History[0].UserInputMessage.Content; got != "Follow the repository rules.\n\nRun pwd" {
		t.Fatalf("first user content = %q", got)
	}
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.Content; got != "" {
		t.Fatalf("tool-result current content = %q, want empty", got)
	}
}

func TestBuildKiroPayloadReplaysSignedReasoningWithoutChangingIt(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{"role":"user","content":"Think"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"internal reasoning","signature":"signed-by-upstream"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":"Continue"}
		]
	}`)
	raw, _ := BuildKiroPayload(body, "claude-opus-5", "profile", "AI_EDITOR", modelcapabilities.Capability{}, "medium")

	var payload KiroPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil || assistant.ReasoningContent == nil || assistant.ReasoningContent.ReasoningText == nil {
		t.Fatalf("assistant reasoning = %#v", assistant)
	}
	if assistant.ReasoningContent.ReasoningText.Text != "internal reasoning" || assistant.ReasoningContent.ReasoningText.Signature != "signed-by-upstream" {
		t.Fatalf("assistant reasoning = %#v", assistant.ReasoningContent.ReasoningText)
	}
	if assistant.Content != "Answer" {
		t.Fatalf("assistant content = %q", assistant.Content)
	}
}

func buildTestPayload(t *testing.T, body string) KiroPayload {
	t.Helper()
	raw, _ := BuildKiroPayload([]byte(body), "claude-opus-5", "profile", "AI_EDITOR", modelcapabilities.Capability{}, "")
	var payload KiroPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestBuildKiroPayloadKeepsToolResultImages(t *testing.T) {
	t.Parallel()
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	image := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}}`
	tools := `"tools":[{"name":"screenshot","description":"Capture","input_schema":{"type":"object"}}]`
	turn := `{"role":"user","content":"Take a screenshot"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"screenshot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"captured"},` + image + `]}]}`

	current := buildTestPayload(t, `{"messages":[`+turn+`],`+tools+`}`).ConversationState.CurrentMessage.UserInputMessage
	if len(current.Images) != 1 || current.Images[0].Format != "png" || current.Images[0].Source.Bytes != png {
		t.Fatalf("current tool_result image = %#v", current.Images)
	}
	if got := current.UserInputMessageContext.ToolResults[0].Content; len(got) != 1 || got[0].Text != "captured" {
		t.Fatalf("current tool_result text = %#v", got)
	}

	history := buildTestPayload(t, `{"messages":[`+turn+`,
		{"role":"assistant","content":"done"},
		{"role":"user","content":"again"}
	],`+tools+`}`).ConversationState.History[2].UserInputMessage
	if len(history.Images) != 1 || history.Images[0].Source.Bytes != png {
		t.Fatalf("history tool_result image = %#v", history.Images)
	}

	imageOnly := buildTestPayload(t, `{"messages":[
		{"role":"user","content":"Take a screenshot"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"screenshot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[`+image+`]}]}
	],`+tools+`}`).ConversationState.CurrentMessage.UserInputMessage
	if len(imageOnly.Images) != 1 {
		t.Fatalf("image-only tool_result image = %#v", imageOnly.Images)
	}
	if got := imageOnly.UserInputMessageContext.ToolResults[0].Content; len(got) != 1 || strings.TrimSpace(got[0].Text) == "" {
		t.Fatalf("image-only tool_result text = %#v, want placeholder", got)
	}
}
