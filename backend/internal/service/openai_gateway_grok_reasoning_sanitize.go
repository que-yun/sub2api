package service

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Grok/xAI 会把详细思考以 reasoning.summary_text 明文回传；Codex 按 OpenAI
// Responses 事件渲染后会像“把思考全打印出来”。OpenAI GPT 路径通常以
// encrypted_content 续上下文，可见 summary 很短或可折叠。
// 对 Grok 响应做可见层脱敏：丢掉 summary 流事件，清空 reasoning.summary 明文。

func shouldSanitizeGrokVisibleReasoning(account *Account) bool {
	return account != nil && account.Platform == PlatformGrok
}

func isGrokHiddenReasoningSSEEventType(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_text.delta",
		"response.reasoning_text.done":
		return true
	default:
		return false
	}
}

// sanitizeGrokVisibleReasoningSSELine 处理单行 SSE。
// drop=true 表示整行不写给客户端；changed 表示 data 行内容被改写。
func sanitizeGrokVisibleReasoningSSELine(line string) (out string, drop bool, changed bool) {
	if eventType, ok := extractOpenAISSEEventLine(line); ok {
		if isGrokHiddenReasoningSSEEventType(eventType) {
			return "", true, false
		}
		return line, false, false
	}

	data, ok := extractOpenAISSEDataLine(line)
	if !ok {
		return line, false, false
	}
	dataBytes := []byte(data)
	if !gjson.ValidBytes(dataBytes) {
		return line, false, false
	}

	eventType := strings.TrimSpace(gjson.GetBytes(dataBytes, "type").String())
	if isGrokHiddenReasoningSSEEventType(eventType) {
		return "", true, false
	}

	sanitized, scrubbed := sanitizeGrokVisibleReasoningJSON(dataBytes)
	if !scrubbed {
		return line, false, false
	}
	// 保留原始 "data:" 与空格风格
	prefixLen := len(line) - len(data)
	if prefixLen < 0 {
		prefixLen = 0
	}
	return line[:prefixLen] + string(sanitized), false, true
}

// sanitizeGrokVisibleReasoningJSON 清空 Responses JSON 里对客户端可见的 reasoning 明文。
// 保留 reasoning item 骨架（id/type/status），避免破坏协议形状；只去掉 summary 文本。
func sanitizeGrokVisibleReasoningJSON(body []byte) ([]byte, bool) {
	if len(bytes.TrimSpace(body)) == 0 || !gjson.ValidBytes(body) {
		return body, false
	}

	out := body
	changed := false

	// 顶层 reasoning.summary=detailed 会强化客户端展示；收敛为 auto。
	if summary := strings.TrimSpace(gjson.GetBytes(out, "reasoning.summary").String()); summary != "" && !strings.EqualFold(summary, "auto") {
		if next, err := sjson.SetBytes(out, "reasoning.summary", "auto"); err == nil {
			out = next
			changed = true
		}
	}
	if summary := strings.TrimSpace(gjson.GetBytes(out, "response.reasoning.summary").String()); summary != "" && !strings.EqualFold(summary, "auto") {
		if next, err := sjson.SetBytes(out, "response.reasoning.summary", "auto"); err == nil {
			out = next
			changed = true
		}
	}

	if next, ok := scrubGrokReasoningSummariesAtPath(out, "output"); ok {
		out = next
		changed = true
	}
	if next, ok := scrubGrokReasoningSummariesAtPath(out, "response.output"); ok {
		out = next
		changed = true
	}

	// output_item.added/done 的 item 本体
	if strings.TrimSpace(gjson.GetBytes(out, "item.type").String()) == "reasoning" {
		if next, ok := scrubGrokReasoningItemSummary(out, "item"); ok {
			out = next
			changed = true
		}
	}

	return out, changed
}

func scrubGrokReasoningSummariesAtPath(body []byte, path string) ([]byte, bool) {
	arr := gjson.GetBytes(body, path)
	if !arr.IsArray() {
		return body, false
	}
	out := body
	changed := false
	for i, item := range arr.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}
		itemPath := fmt.Sprintf("%s.%d", path, i)
		next, ok := scrubGrokReasoningItemSummary(out, itemPath)
		if !ok {
			continue
		}
		out = next
		changed = true
	}
	return out, changed
}

func scrubGrokReasoningItemSummary(body []byte, itemPath string) ([]byte, bool) {
	summary := gjson.GetBytes(body, itemPath+".summary")
	if !summary.Exists() {
		return body, false
	}
	// 已是空数组则不动
	if summary.IsArray() && len(summary.Array()) == 0 {
		return body, false
	}
	next, err := sjson.SetRawBytes(body, itemPath+".summary", []byte("[]"))
	if err != nil {
		return body, false
	}
	return next, true
}


// sanitizeGrokVisibleReasoningSSEBody 对整段 SSE 文本逐行脱敏，用于 SSE→JSON 失败回退路径。
func sanitizeGrokVisibleReasoningSSEBody(body string) string {
	if body == "" {
		return body
	}
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		sanitized, drop, _ := sanitizeGrokVisibleReasoningSSELine(line)
		if drop {
			continue
		}
		out = append(out, sanitized)
	}
	return strings.Join(out, "\n")
}
