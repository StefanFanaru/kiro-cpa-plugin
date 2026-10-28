// Package common provides shared utilities for Kiro translators.
package common

import (
	"encoding/json"

	"github.com/tidwall/gjson"
)

// MergeAdjacentMessages merges adjacent messages with the same role.
// This reduces API call complexity and improves compatibility.
// Based on AIClient-2-API implementation.
// NOTE: Tool messages are NOT merged because each has a unique tool_call_id that must be preserved.
func MergeAdjacentMessages(messages []gjson.Result) []gjson.Result {
	if len(messages) <= 1 {
		return messages
	}

	var merged []gjson.Result
	for _, msg := range messages {
		if len(merged) == 0 {
			merged = append(merged, msg)
			continue
		}

		lastMsg := merged[len(merged)-1]
		currentRole := msg.Get("role").String()
		lastRole := lastMsg.Get("role").String()

		// Don't merge tool messages - each has a unique tool_call_id
		if currentRole == "tool" || lastRole == "tool" {
			merged = append(merged, msg)
			continue
		}

		if currentRole == lastRole {
			// Merge content from current message into last message
			mergedContent := mergeMessageContent(lastMsg, msg)
			var mergedToolCalls []interface{}
			if currentRole == "assistant" {
				// Preserve assistant tool_calls when adjacent assistant messages are merged.
				mergedToolCalls = mergeToolCalls(lastMsg.Get("tool_calls"), msg.Get("tool_calls"))
			}

			// Start from the original message so provider-specific metadata (for
			// example reasoning_content, refusal, name, or annotations) is not
			// silently discarded while adjacent content is coalesced.
			mergedMsg := createMergedMessageFrom(lastMsg, lastRole, mergedContent, mergedToolCalls)
			merged[len(merged)-1] = gjson.Parse(mergedMsg)
		} else {
			merged = append(merged, msg)
		}
	}

	return merged
}

// mergeMessageContent merges the content of two messages with the same role.
// Handles both string content and array content (with text, tool_use, tool_result blocks).
func mergeMessageContent(msg1, msg2 gjson.Result) string {
	content1 := msg1.Get("content")
	content2 := msg2.Get("content")

	// Extract content blocks from both messages
	var blocks1, blocks2 []map[string]interface{}

	if content1.IsArray() {
		for _, block := range content1.Array() {
			blocks1 = append(blocks1, blockToMap(block))
		}
	} else if content1.Type == gjson.String {
		blocks1 = append(blocks1, map[string]interface{}{
			"type": "text",
			"text": content1.String(),
		})
	}

	if content2.IsArray() {
		for _, block := range content2.Array() {
			blocks2 = append(blocks2, blockToMap(block))
		}
	} else if content2.Type == gjson.String {
		blocks2 = append(blocks2, map[string]interface{}{
			"type": "text",
			"text": content2.String(),
		})
	}

	// Merge text blocks if both end/start with text. A block declaring
	// type "text" is not proof that "text" holds a string: the client sends
	// this JSON, so null or a number reaches the map as nil or float64 and an
	// unchecked assertion would panic. A panic here kills the whole CPA
	// process, because neither cliproxyPluginCall nor the host's C bridge
	// recovers. Blocks that do not carry two strings are left unmerged.
	if len(blocks1) > 0 && len(blocks2) > 0 {
		if blocks1[len(blocks1)-1]["type"] == "text" && blocks2[0]["type"] == "text" {
			text1, ok1 := blocks1[len(blocks1)-1]["text"].(string)
			text2, ok2 := blocks2[0]["text"].(string)
			if ok1 && ok2 {
				// Merge the last text block of msg1 with the first text block of msg2
				blocks1[len(blocks1)-1]["text"] = text1 + "\n" + text2
				blocks2 = blocks2[1:] // Remove the merged block from blocks2
			}
		}
	}

	// Combine all blocks
	allBlocks := append(blocks1, blocks2...)

	// Convert to JSON
	result, _ := json.Marshal(allBlocks)
	return string(result)
}

// blockToMap converts a gjson.Result block to a map[string]interface{}
func blockToMap(block gjson.Result) map[string]interface{} {
	result := make(map[string]interface{})
	block.ForEach(func(key, value gjson.Result) bool {
		result[key.String()] = mergeJSONValue(value)
		return true
	})
	return result
}

// mergeJSONValue preserves numbers at every depth while retaining the existing
// map encoding for key order and string escaping.
func mergeJSONValue(value gjson.Result) interface{} {
	if value.IsObject() {
		return blockToMap(value)
	}
	if value.IsArray() {
		items := make([]interface{}, 0, len(value.Array()))
		for _, item := range value.Array() {
			items = append(items, mergeJSONValue(item))
		}
		return items
	}
	if value.Type == gjson.Number {
		return json.Number(value.Raw)
	}
	return value.Value()
}

func createMergedMessageFrom(original gjson.Result, role, content string, toolCalls []interface{}) string {
	msg := blockToMap(original)
	msg["role"] = role
	msg["content"] = json.RawMessage(content)
	if role == "assistant" {
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		} else {
			delete(msg, "tool_calls")
		}
	}
	result, _ := json.Marshal(msg)
	return string(result)
}

// mergeToolCalls combines tool_calls from two assistant messages while preserving order.
func mergeToolCalls(tc1, tc2 gjson.Result) []interface{} {
	var merged []interface{}

	if tc1.IsArray() {
		for _, tc := range tc1.Array() {
			merged = append(merged, mergeJSONValue(tc))
		}
	}
	if tc2.IsArray() {
		for _, tc := range tc2.Array() {
			merged = append(merged, mergeJSONValue(tc))
		}
	}

	return merged
}
