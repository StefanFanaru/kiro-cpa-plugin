package common

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// sameRoleRun builds n adjacent user messages, which all merge into one.
func sameRoleRun(n, textBytes int) []gjson.Result {
	text := strings.Repeat("y", textBytes)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q}]}`, text)
	}
	return gjson.Parse("[" + strings.Join(parts, ",") + "]").Array()
}

// claudeCodeHistory builds a Claude Code shaped conversation: assistant
// tool_use turns answered by 4 KiB tool_result turns, with a user reminder
// after every 10th tool_result that merges into it.
func claudeCodeHistory(exchanges int) []gjson.Result {
	result := strings.Repeat("r", 4096)
	parts := []string{`{"role":"user","content":"start"}`}
	for i := 0; i < exchanges; i++ {
		parts = append(parts,
			fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"step %d"},{"type":"tool_use","id":"toolu_%d","name":"Read","input":{"file_path":"/src/f%d.go"}}]}`, i, i, i),
			fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":%q}]}`, i, result))
		if i%10 == 9 {
			parts = append(parts, `{"role":"user","content":[{"type":"text","text":"<system-reminder>keep going</system-reminder>"}]}`)
		}
	}
	return gjson.Parse("[" + strings.Join(parts, ",") + "]").Array()
}

func benchMerge(b *testing.B, messages []gjson.Result) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MergeAdjacentMessages(messages)
	}
}

func BenchmarkMergeSameRoleRun100(b *testing.B) { benchMerge(b, sameRoleRun(100, 4096)) }
func BenchmarkMergeSameRoleRun400(b *testing.B) { benchMerge(b, sameRoleRun(400, 4096)) }
func BenchmarkMergeClaudeCodeHistory(b *testing.B) {
	benchMerge(b, claudeCodeHistory(100))
}
