package service

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// grok Build(cli-chat-proxy)自带后端 web_search。收到图片 + 开放式问题（如「这是什么」）
// 时，它不直接看图作答，而是把从图里读到的词拿去多轮联网检索（实测一次触发 22 次
// web_search、正文输出为空），在 Codex 等 agent 客户端表现为「只回一句 preamble 就没
// 下文」。这里在含图片的请求里注入一条 developer 指令：直接看图作答，仅当用户显式要求
// 上网时才搜索。实测该指令可把 web_search 从 22 收敛到 0 且正文完整，用户明确要搜时仍放行。
// marker 用方括号而非尖括号：encoding/json 默认会把 < > 转义成 < >，
// 若用尖括号，注入后 raw JSON 里是转义形态，幂等检查用字面串匹配会漏判、导致
// previous_response_id 续接时重复注入。方括号不受 HTML 转义影响，匹配稳定。
const (
	grokImageSearchGuardMarker = "[sub2api-grok-image-guard]"
	grokImageSearchGuardText   = grokImageSearchGuardMarker + " " +
		"The user's message includes an image. Read the image and answer directly from its visual content. " +
		"Do NOT use web search or any search tool to investigate the image unless the user's own message " +
		"explicitly asks you to search the internet or look up external / latest information. For a plain " +
		"\"what is this / describe this image\" request, answer from the image alone without searching."
)

// appendGrokImageSearchGuard 往 grok Build responses body 的 input 数组注入图片搜索抑制指令。
// 仅在请求含 input_image 时生效；无图片 / 已注入过 / 解析失败时原样返回 body，绝不破坏请求。
// 调用方需先确认账号是 grok Build（cli-chat-proxy），本函数只负责「含图片 → 注入」这一步。
func appendGrokImageSearchGuard(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// 无图片：不干预 grok 默认行为（纯文本仍可正常联网搜索）。
	if !OpenAIRequestBodyHasImageInput(body) {
		return body
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	// 幂等：previous_response_id 续接或重试时避免重复注入。
	if strings.Contains(input.Raw, grokImageSearchGuardMarker) {
		return body
	}

	var items []apicompat.ResponsesInputItem
	if err := json.Unmarshal([]byte(input.Raw), &items); err != nil {
		return body
	}
	if len(items) == 0 {
		return body
	}

	content, err := json.Marshal([]apicompat.ResponsesContentPart{{
		Type: "input_text",
		Text: grokImageSearchGuardText,
	}})
	if err != nil {
		return body
	}
	guard := apicompat.ResponsesInputItem{
		Type:    "message",
		Role:    "developer",
		Content: content,
	}

	// 插到前导 developer 消息之后、首条非 developer 之前，保持系统指令块紧凑、语义靠前。
	insertAt := 0
	for insertAt < len(items) && items[insertAt].Type == "message" && items[insertAt].Role == "developer" {
		insertAt++
	}
	items = append(items, apicompat.ResponsesInputItem{})
	copy(items[insertAt+1:], items[insertAt:])
	items[insertAt] = guard

	encoded, err := json.Marshal(items)
	if err != nil {
		return body
	}
	out, err := sjson.SetRawBytes(body, "input", encoded)
	if err != nil {
		return body
	}
	logger.L().Debug("grok image search guard injected")
	return out
}
