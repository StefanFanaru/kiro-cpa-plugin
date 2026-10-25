package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nguyenha935/kiro-cpa-plugin/internal/modelcapabilities"
)

// System instructions have to be asserted on the payload, not on the model's
// answer.
//
// An end-to-end check that asks the model to obey an instruction and greps its
// reply measures obedience, which is probabilistic and competes with the persona
// the Kiro service injects server side. The same request wording passed and failed
// on consecutive runs. What the translator owns is whether the instruction reaches
// the payload at all, and that is deterministic.
func buildFromOpenAI(t *testing.T, request map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := BuildKiroPayloadFromOpenAI(
		body, "claude-haiku-4.5", "profile",
		"KIRO_CLI", modelcapabilities.Capability{ModelID: "claude-haiku-4.5"}, "")
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	return decoded
}

func payloadText(t *testing.T, payload map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// A system message must reach the payload wherever the caller put it. The
// instruction cannot travel in a systemPrompt field, because the runtime rejects
// one, so it is folded into a user turn instead; what matters is that it survives.
func TestSystemInstructionReachesPayloadFromAnyPosition(t *testing.T) {
	const rule = "End every reply with the token ZZQ."

	cases := []struct {
		name     string
		messages []map[string]any
	}{
		{
			name: "system first with no history",
			messages: []map[string]any{
				{"role": "system", "content": rule},
				{"role": "user", "content": "Say goodbye."},
			},
		},
		{
			name: "system first with history",
			messages: []map[string]any{
				{"role": "system", "content": rule},
				{"role": "user", "content": "Hi."},
				{"role": "assistant", "content": "Hello."},
				{"role": "user", "content": "Say goodbye."},
			},
		},
		{
			name: "system after the history",
			messages: []map[string]any{
				{"role": "user", "content": "Hi."},
				{"role": "assistant", "content": "Hello."},
				{"role": "system", "content": rule},
				{"role": "user", "content": "Say goodbye."},
			},
		},
		{
			name: "developer role is treated as system",
			messages: []map[string]any{
				{"role": "developer", "content": rule},
				{"role": "user", "content": "Say goodbye."},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := buildFromOpenAI(t, map[string]any{
				"model": "claude-haiku-4.5", "messages": tc.messages,
			})
			text := payloadText(t, payload)
			if !strings.Contains(text, rule) {
				t.Fatalf("instruction missing from payload: %s", text)
			}
			// It must appear once. Being folded in twice duplicates the
			// instruction inside the conversation the model reads.
			if got := strings.Count(text, rule); got != 1 {
				t.Fatalf("instruction appears %d times, want exactly 1: %s", got, text)
			}
			// And it must never be sent as a top-level field the runtime refuses.
			if _, present := payload["systemPrompt"]; present {
				t.Fatal("payload carries a systemPrompt field, which the runtime rejects with 400")
			}
		})
	}
}

// Several system messages must all survive, in order, rather than the last one
// winning.
func TestMultipleSystemMessagesAllSurvive(t *testing.T) {
	payload := buildFromOpenAI(t, map[string]any{
		"model": "claude-haiku-4.5",
		"messages": []map[string]any{
			{"role": "system", "content": "Rule one."},
			{"role": "system", "content": "Rule two."},
			{"role": "user", "content": "Go."},
		},
	})
	text := payloadText(t, payload)
	for _, rule := range []string{"Rule one.", "Rule two."} {
		if !strings.Contains(text, rule) {
			t.Fatalf("%q missing from payload: %s", rule, text)
		}
	}
	if strings.Index(text, "Rule one.") > strings.Index(text, "Rule two.") {
		t.Fatalf("system messages were reordered: %s", text)
	}
}

// The user's own words must not be lost when an instruction is folded in front of
// them.
func TestFoldingInstructionsKeepsTheUserTurn(t *testing.T) {
	payload := buildFromOpenAI(t, map[string]any{
		"model": "claude-haiku-4.5",
		"messages": []map[string]any{
			{"role": "system", "content": "Be terse."},
			{"role": "user", "content": "What is the capital of France?"},
		},
	})
	text := payloadText(t, payload)
	if !strings.Contains(text, "What is the capital of France?") {
		t.Fatalf("user content lost: %s", text)
	}
	if !strings.Contains(text, "Be terse.") {
		t.Fatalf("instruction lost: %s", text)
	}
}

// Kiro caches the request prefix, from history[0] onward. A system message the
// client inserts mid-conversation must stay at its own position: hoisting it
// in front of the first user turn changed history[0] on the next request and
// made every later turn a cache miss.
func TestInlineSystemMessageKeepsEarlierKiroHistory(t *testing.T) {
	build := func(body string) KiroPayload {
		raw, _ := BuildKiroPayloadFromOpenAI([]byte(body), "claude-opus-5", "profile", "AI_EDITOR", modelcapabilities.Capability{}, "")
		var payload KiroPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	turn1 := build(`{"messages":[{"role":"system","content":"Base."},{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}]}`)
	turn2 := build(`{"messages":[{"role":"system","content":"Base."},{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"},{"role":"assistant","content":"a2"},{"role":"developer","content":[{"type":"text","text":"Reminder:"},{"type":"text","text":"date changed."}]},{"role":"user","content":"q3"}]}`)

	first := turn1.ConversationState.History[0].UserInputMessage.Content
	second := turn2.ConversationState.History[0].UserInputMessage.Content
	if first != "Base.\n\nq1" || second != first {
		t.Errorf("history[0] changed: %q -> %q", first, second)
	}
	if got := turn2.ConversationState.CurrentMessage.UserInputMessage.Content; got != "Reminder:\ndate changed.\nq3" {
		t.Errorf("current content = %q", got)
	}
}
