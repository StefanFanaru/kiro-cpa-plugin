package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

// The expected messages were recorded from the pairwise implementation that
// re-serialized the merged message after every member of a run. Merging a run
// in one pass must keep the output byte for byte, because Kiro's prompt cache
// matches the serialized request prefix.
func TestMergeAdjacentMessagesOutputBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single message is returned unchanged",
			in:   `[{"role":"user","content":"only","extra":[]}]`,
			want: []string{
				`{"role":"user","content":"only","extra":[]}`,
			},
		},
		{
			name: "string contents join with newline",
			in:   `[{"role":"user","content":"a"},{"role":"user","content":"b <&>"},{"role":"user","content":"c"}]`,
			want: []string{
				`{"content":[{"text":"a\nb \u003c\u0026\u003e\nc","type":"text"}],"role":"user"}`,
			},
		},
		{
			name: "block arrays join boundary text blocks",
			in:   `[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA="}}]},{"role":"user","content":[{"type":"text","text":"b"},{"type":"text","text":"c"}]},{"role":"user","content":[{"type":"text","text":"d"}]}]`,
			want: []string{
				`{"content":[{"cache_control":{"type":"ephemeral"},"text":"a","type":"text"},{"source":{"data":"AAA=","media_type":"image/png","type":"base64"},"type":"image"},{"text":"b","type":"text"},{"text":"c\nd","type":"text"}],"role":"user"}`,
			},
		},
		{
			name: "mixed string and block content",
			in:   `[{"role":"user","content":"a"},{"role":"user","content":[{"type":"text","text":"b"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA="}}]},{"role":"user","content":"c"},{"role":"user","content":null},{"role":"user","content":"d"}]`,
			want: []string{
				`{"content":[{"text":"a\nb","type":"text"},{"image_url":{"url":"data:image/png;base64,AAA="},"type":"image_url"},{"text":"c\nd","type":"text"}],"role":"user"}`,
			},
		},
		{
			name: "tool_result blocks stay separate",
			in:   `[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"n":12345678901234567890,"list":[],"nested":[[1,{"b":2,"a":1}]]}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out"}]}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"more","is_error":true}]},{"role":"user","content":"reminder"}]`,
			want: []string{
				`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"n":12345678901234567890,"list":[],"nested":[[1,{"b":2,"a":1}]]}}]}`,
				`{"content":[{"content":[{"text":"out","type":"text"}],"tool_use_id":"t1","type":"tool_result"},{"content":"more","is_error":true,"tool_use_id":"t2","type":"tool_result"},{"text":"reminder","type":"text"}],"role":"user"}`,
			},
		},
		{
			name: "assistant run keeps first metadata and all tool_calls",
			in:   `[{"role":"assistant","content":"one","reasoning_content":"r1","annotations":[]},{"role":"assistant","content":"two","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{}"},"extra":[]}],"reasoning_content":"r2"},{"role":"assistant","content":[{"type":"text","text":"three"}],"tool_calls":[{"id":"call_2","type":"function","function":{"name":"Write","arguments":"{}"}}]}]`,
			want: []string{
				`{"annotations":null,"content":[{"text":"one\ntwo\nthree","type":"text"}],"reasoning_content":"r1","role":"assistant","tool_calls":[{"extra":[],"function":{"arguments":"{}","name":"Read"},"id":"call_1","type":"function"},{"function":{"arguments":"{}","name":"Write"},"id":"call_2","type":"function"}]}`,
			},
		},
		{
			name: "assistant run without tool_calls drops the field",
			in:   `[{"role":"assistant","content":"a","tool_calls":[]},{"role":"assistant","content":"b","tool_calls":{"id":"x"}},{"role":"assistant","content":"c"}]`,
			want: []string{
				`{"content":[{"text":"a\nb\nc","type":"text"}],"role":"assistant"}`,
			},
		},
		{
			name: "non-string text is not joined",
			in:   `[{"role":"user","content":[{"type":"text","text":"a"}]},{"role":"user","content":[{"type":"text","text":null}]},{"role":"user","content":[{"type":"text","text":"b"}]}]`,
			want: []string{
				`{"content":[{"text":"a","type":"text"},{"text":null,"type":"text"},{"text":"b","type":"text"}],"role":"user"}`,
			},
		},
		{
			name: "tool messages and role changes split runs",
			in:   `[{"role":"system","content":"s1"},{"role":"system","content":"s2"},{"role":"user","content":"u"},{"role":"tool","tool_call_id":"a","content":"r1"},{"role":"tool","tool_call_id":"b","content":"r2"},{"role":"user","content":"u2"},{"role":"user","content":"u3"}]`,
			want: []string{
				`{"content":[{"text":"s1\ns2","type":"text"}],"role":"system"}`,
				`{"role":"user","content":"u"}`,
				`{"role":"tool","tool_call_id":"a","content":"r1"}`,
				`{"role":"tool","tool_call_id":"b","content":"r2"}`,
				`{"content":[{"text":"u2\nu3","type":"text"}],"role":"user"}`,
			},
		},
	}

	for _, tc := range cases {
		merged := MergeAdjacentMessages(gjson.Parse(tc.in).Array())
		if len(merged) != len(tc.want) {
			t.Errorf("%s: got %d messages, want %d", tc.name, len(merged), len(tc.want))
			continue
		}
		for i, want := range tc.want {
			if merged[i].Raw != want {
				t.Errorf("%s: message %d\ngot  %s\nwant %s", tc.name, i, merged[i].Raw, want)
			}
		}
	}
}
