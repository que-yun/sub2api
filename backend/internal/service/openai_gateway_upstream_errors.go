package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

func logOpenAIInstructionsRequiredDebug(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamStatusCode int,
	upstreamMsg string,
	requestBody []byte,
	upstreamBody []byte,
) {
	msg := strings.TrimSpace(upstreamMsg)
	if !isOpenAIInstructionsRequiredError(upstreamStatusCode, msg, upstreamBody) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	accountID := int64(0)
	accountName := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
	}

	userAgent := ""
	originator := ""
	if c != nil {
		userAgent = strings.TrimSpace(c.GetHeader("User-Agent"))
		originator = strings.TrimSpace(c.GetHeader("originator"))
	}

	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.Int("upstream_status_code", upstreamStatusCode),
		zap.String("upstream_error_message", msg),
		zap.String("request_user_agent", userAgent),
		zap.Bool("codex_official_client_match", openai.IsCodexOfficialClientByHeaders(userAgent, originator)),
	}
	fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, requestBody)

	logger.FromContext(ctx).With(fields...).Warn("OpenAI 上游返回 Instructions are required，已记录请求详情用于排查")
}

func isOpenAIInstructionsRequiredError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	hasInstructionRequired := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "instructions are required") {
			return true
		}
		if strings.Contains(lower, "required parameter: 'instructions'") {
			return true
		}
		if strings.Contains(lower, "required parameter: instructions") {
			return true
		}
		if strings.Contains(lower, "missing required parameter") && strings.Contains(lower, "instructions") {
			return true
		}
		return strings.Contains(lower, "instruction") && strings.Contains(lower, "required")
	}

	if hasInstructionRequired(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}

	errMsg := gjson.GetBytes(upstreamBody, "error.message").String()
	errMsgLower := strings.ToLower(strings.TrimSpace(errMsg))
	errCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.code").String()))
	errParam := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.param").String()))
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.type").String()))

	if errParam == "instructions" {
		return true
	}
	if hasInstructionRequired(errMsg) {
		return true
	}
	if strings.Contains(errCode, "missing_required_parameter") && strings.Contains(errMsgLower, "instructions") {
		return true
	}
	if strings.Contains(errType, "invalid_request") && strings.Contains(errMsgLower, "instructions") && strings.Contains(errMsgLower, "required") {
		return true
	}

	return false
}

func logGrokModelInputDebug(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamStatusCode int,
	requestBody []byte,
	upstreamBody []byte,
) {
	if account == nil || account.Platform != PlatformGrok || upstreamStatusCode != http.StatusUnprocessableEntity {
		return
	}
	if !strings.Contains(string(upstreamBody), "ModelInput") {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	userAgent := ""
	originator := ""
	if c != nil {
		userAgent = strings.TrimSpace(c.GetHeader("User-Agent"))
		originator = strings.TrimSpace(c.GetHeader("originator"))
	}

	logger.FromContext(ctx).With(
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", account.ID),
		zap.String("account_name", strings.TrimSpace(account.Name)),
		zap.Int("upstream_status_code", upstreamStatusCode),
		zap.String("request_user_agent", userAgent),
		zap.Bool("codex_official_client_match", openai.IsCodexOfficialClientByHeaders(userAgent, originator)),
		zap.Any("request_shape", summarizeGrokRequestShape(requestBody)),
	).Warn("Grok 上游无法反序列化 Responses input，已记录脱敏请求结构")
}

func summarizeGrokRequestShape(body []byte) map[string]any {
	shape := map[string]any{
		"body_bytes": len(body),
		"valid_json": json.Valid(body),
	}
	if !json.Valid(body) {
		return shape
	}

	root := gjson.ParseBytes(body)
	if root.IsObject() {
		shape["top_level_keys"] = sortedGJSONObjectKeys(root)
	}
	shape["model"] = root.Get("model").String()
	shape["stream"] = root.Get("stream").Bool()
	if root.Get("previous_response_id").Exists() {
		shape["has_previous_response_id"] = true
	}
	if tools := root.Get("tools"); tools.Exists() {
		shape["tools"] = summarizeGrokJSONArray(tools, 20, summarizeGrokToolShape)
	}
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		shape["tool_choice_type"] = gjsonTypeName(toolChoice)
	}

	input := root.Get("input")
	shape["input_type"] = gjsonTypeName(input)
	if input.IsArray() {
		items := input.Array()
		shape["input_count"] = len(items)
		shape["input_items"] = summarizeGrokJSONArray(input, 80, summarizeGrokInputItemShape)
	} else if input.Type == gjson.String {
		shape["input_text_bytes"] = len(input.String())
	}
	return shape
}

func summarizeGrokJSONArray(value gjson.Result, limit int, fn func(gjson.Result) map[string]any) []map[string]any {
	if !value.IsArray() || limit <= 0 {
		return nil
	}
	items := value.Array()
	out := make([]map[string]any, 0, minInt(len(items), limit))
	for i, item := range items {
		if i >= limit {
			break
		}
		summary := fn(item)
		summary["index"] = i
		out = append(out, summary)
	}
	return out
}

func summarizeGrokInputItemShape(item gjson.Result) map[string]any {
	shape := map[string]any{
		"json_type": gjsonTypeName(item),
	}
	if !item.IsObject() {
		if item.Type == gjson.String {
			shape["text_bytes"] = len(item.String())
		}
		return shape
	}

	shape["keys"] = sortedGJSONObjectKeys(item)
	itemType := strings.TrimSpace(item.Get("type").String())
	shape["type"] = itemType
	for _, field := range []string{"id", "call_id", "status", "role", "name"} {
		if v := strings.TrimSpace(item.Get(field).String()); v != "" {
			shape[field] = v
		}
	}
	if content := item.Get("content"); content.Exists() {
		shape["content_type"] = gjsonTypeName(content)
		if content.IsArray() {
			shape["content_count"] = len(content.Array())
			shape["content_items"] = summarizeGrokJSONArray(content, 20, summarizeGrokContentItemShape)
		} else if content.Type == gjson.String {
			shape["content_bytes"] = len(content.String())
		}
	}
	if summary := item.Get("summary"); summary.Exists() {
		shape["summary_type"] = gjsonTypeName(summary)
		if summary.IsArray() {
			shape["summary_count"] = len(summary.Array())
		}
	}
	if queries := item.Get("queries"); queries.Exists() {
		shape["queries_type"] = gjsonTypeName(queries)
		if queries.IsArray() {
			shape["queries_count"] = len(queries.Array())
		}
	}
	if tools := item.Get("tools"); tools.Exists() {
		shape["tools_type"] = gjsonTypeName(tools)
		if tools.IsArray() {
			shape["tools_count"] = len(tools.Array())
			shape["tools"] = summarizeGrokJSONArray(tools, 40, summarizeGrokToolShape)
		}
	}
	for _, field := range []string{"text", "output", "result", "arguments"} {
		if v := item.Get(field); v.Exists() {
			shape[field+"_type"] = gjsonTypeName(v)
			if v.Type == gjson.String {
				shape[field+"_bytes"] = len(v.String())
			}
		}
	}
	return shape
}

func summarizeGrokContentItemShape(item gjson.Result) map[string]any {
	shape := map[string]any{
		"json_type": gjsonTypeName(item),
	}
	if item.IsObject() {
		shape["keys"] = sortedGJSONObjectKeys(item)
		if t := strings.TrimSpace(item.Get("type").String()); t != "" {
			shape["type"] = t
		}
		for _, field := range []string{"text", "input_text", "output_text"} {
			if v := item.Get(field); v.Exists() {
				shape[field+"_type"] = gjsonTypeName(v)
				if v.Type == gjson.String {
					shape[field+"_bytes"] = len(v.String())
				}
			}
		}
		return shape
	}
	if item.Type == gjson.String {
		shape["text_bytes"] = len(item.String())
	}
	return shape
}

func summarizeGrokToolShape(item gjson.Result) map[string]any {
	shape := map[string]any{
		"json_type": gjsonTypeName(item),
	}
	if !item.IsObject() {
		return shape
	}
	shape["keys"] = sortedGJSONObjectKeys(item)
	for _, field := range []string{"type", "name"} {
		if v := strings.TrimSpace(item.Get(field).String()); v != "" {
			shape[field] = v
		}
	}
	if fnName := strings.TrimSpace(item.Get("function.name").String()); fnName != "" {
		shape["function_name"] = fnName
	}
	return shape
}

func sortedGJSONObjectKeys(value gjson.Result) []string {
	if !value.IsObject() {
		return nil
	}
	keys := make([]string, 0)
	value.ForEach(func(key, _ gjson.Result) bool {
		keys = append(keys, key.String())
		return true
	})
	sort.Strings(keys)
	return keys
}

func gjsonTypeName(value gjson.Result) string {
	if !value.Exists() {
		return "missing"
	}
	switch {
	case value.IsArray():
		return "array"
	case value.IsObject():
		return "object"
	case value.Type == gjson.String:
		return "string"
	case value.Type == gjson.Number:
		return "number"
	case value.Type == gjson.True || value.Type == gjson.False:
		return "bool"
	case value.Type == gjson.Null:
		return "null"
	default:
		return "json"
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isOpenAITransientProcessingError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest && upstreamStatusCode != http.StatusServiceUnavailable {
		return false
	}

	hasOpenAIServerOverloadedCode := func(payload []byte) bool {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
		if code == "" {
			code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
		}
		return code == "server_is_overloaded" || code == "slow_down"
	}

	if len(upstreamBody) > 0 && hasOpenAIServerOverloadedCode(upstreamBody) {
		return true
	}
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "an error occurred while processing your request") {
			return true
		}
		if strings.Contains(lower, "selected model is at capacity") {
			return true
		}
		return strings.Contains(lower, "you can retry your request") &&
			strings.Contains(lower, "help.openai.com") &&
			strings.Contains(lower, "request id")
	}

	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	if match(gjson.GetBytes(upstreamBody, "error.message").String()) {
		return true
	}
	return match(string(upstreamBody))
}

func isOpenAIContextWindowError(upstreamMsg string, upstreamBody []byte) bool {
	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "context_too_large") || strings.Contains(lower, "context_length_exceeded") {
			return true
		}
		if strings.Contains(lower, "maximum context length") || strings.Contains(lower, "max context length") {
			return true
		}
		hasExceeded := strings.Contains(lower, "exceed") || strings.Contains(lower, "too large") || strings.Contains(lower, "too long")
		if strings.Contains(lower, "context window") && hasExceeded {
			return true
		}
		if strings.Contains(lower, "context length") && hasExceeded {
			return true
		}
		return strings.Contains(lower, "token limit") &&
			strings.Contains(lower, "context") &&
			hasExceeded
	}

	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	for _, path := range []string{
		"error.message",
		"response.error.message",
		"message",
		"error.code",
		"response.error.code",
		"code",
	} {
		if match(gjson.GetBytes(upstreamBody, path).String()) {
			return true
		}
	}
	return match(string(upstreamBody))
}

// isGLMFamilyModel reports whether an upstream/model id is GLM family.
// Used only for context-window failover: mapped gpt-* -> glm-* accounts should
// not permanently fail long Codex sessions when another non-GLM account exists.
func isGLMFamilyModel(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return false
	}
	// Accept common vendor prefixes and aliases: glm-5.2, z-ai/glm-5.2, GLM-5.2
	if strings.Contains(normalized, "glm-") || strings.Contains(normalized, "/glm") {
		return true
	}
	if strings.HasPrefix(normalized, "glm") {
		return true
	}
	return false
}

// accountMapsRequestToGLM reports whether this account would forward the
// client-requested model to a GLM upstream model via account model_mapping.
func accountMapsRequestToGLM(account *Account, requestedModel string) bool {
	if account == nil {
		return false
	}
	req := strings.TrimSpace(requestedModel)
	if req == "" {
		return false
	}
	mapped := strings.TrimSpace(account.GetMappedModel(req))
	if mapped == "" {
		mapped = req
	}
	return isGLMFamilyModel(mapped)
}

// shouldFailoverOpenAIContextWindow decides whether a context-window upstream
// rejection should switch accounts instead of failing the client immediately.
// Policy: only auto-failover when the current account maps the request to GLM.
// Native large-context accounts (OpenAI/Grok/etc.) keep the old fail-fast path,
// because the same oversized prompt will fail deterministically there too.
func shouldFailoverOpenAIContextWindow(account *Account, requestedModel, upstreamMsg string, upstreamBody []byte) bool {
	if !isOpenAIContextWindowError(upstreamMsg, upstreamBody) {
		return false
	}
	return accountMapsRequestToGLM(account, requestedModel)
}

func (s *OpenAIGatewayService) shouldFailoverUpstreamError(statusCode int) bool {
	switch statusCode {
	case 401, 402, 403, 429, 529:
		return true
	default:
		return statusCode >= 500
	}
}

func (s *OpenAIGatewayService) shouldFailoverOpenAIUpstreamResponse(statusCode int, upstreamMsg string, upstreamBody []byte, account *Account, requestedModel string) bool {
	if isOpenAIContextWindowError(upstreamMsg, upstreamBody) {
		// GLM-mapped accounts may still be salvageable by switching to a non-GLM
		// account in the same group. Other accounts stay fail-fast.
		return shouldFailoverOpenAIContextWindow(account, requestedModel, upstreamMsg, upstreamBody)
	}
	if isOpenAIRequestBodyTooLargeError(statusCode, upstreamMsg, upstreamBody) {
		return true
	}
	// Multi-model pools (ollama/nvidia/opencode) often reject Codex tool+image
	// payloads with provider-specific 400s. After same-account VL/tool salvage,
	// account failover is still required so image requests can reach another VL pool.
	if statusCode == http.StatusBadRequest &&
		isOpenAIMultiModelCompatPoolAccount(account) &&
		isOpenAICompatibleModelPayloadIncompatError(upstreamMsg, upstreamBody) {
		return true
	}
	if s.shouldFailoverUpstreamError(statusCode) {
		return true
	}
	return isOpenAITransientProcessingError(statusCode, upstreamMsg, upstreamBody)
}

// isOpenAIMultiModelCompatPoolAccount reports OpenAI-compatible multi-model API-key
// pools where model/tool schema rejections are usually destination-specific rather
// than true client hard failures.
func isOpenAIMultiModelCompatPoolAccount(account *Account) bool {
	if account == nil || !account.IsOpenAICompatible() || account.Type != AccountTypeAPIKey {
		return false
	}
	base := strings.ToLower(strings.TrimSpace(account.GetCredential("base_url")))
	if isOllamaOpenAICompatibleAccount(account) ||
		strings.Contains(base, "nvidia.com") ||
		strings.Contains(base, "opencode.ai") {
		return true
	}
	return len(account.GetModelMapping()) > 1 || len(account.GetVisionModelMapping()) > 0
}

// isOpenAICompatibleModelPayloadIncompatError matches provider 400s that mean
// "this destination cannot accept the current tools/image payload", not a fatal
// client schema error on every account.
func isOpenAICompatibleModelPayloadIncompatError(upstreamMsg string, upstreamBody []byte) bool {
	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "invalid tool call arguments") {
			return true
		}
		if strings.Contains(lower, "extra data: line") {
			return true
		}
		if strings.Contains(lower, "does not support tools") ||
			strings.Contains(lower, "tools are not supported") ||
			strings.Contains(lower, "tool calling is not supported") ||
			strings.Contains(lower, "function calling is not supported") ||
			strings.Contains(lower, "tool use is not supported") ||
			strings.Contains(lower, "does not support function") {
			return true
		}
		if strings.Contains(lower, "does not support image") ||
			strings.Contains(lower, "images are not supported") ||
			strings.Contains(lower, "image input is not supported") ||
			strings.Contains(lower, "image_url is not supported") ||
			strings.Contains(lower, "multimodal") && strings.Contains(lower, "not support") {
			return true
		}
		if strings.Contains(lower, "unknown model") ||
			strings.Contains(lower, "model not found") ||
			strings.Contains(lower, "does not exist") && strings.Contains(lower, "model") {
			return true
		}
		return false
	}
	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	for _, path := range []string{
		"error.message",
		"response.error.message",
		"message",
	} {
		if match(gjson.GetBytes(upstreamBody, path).String()) {
			return true
		}
	}
	return match(string(upstreamBody))
}

const openAIContextWindowGLMFailoverReason = GatewayFailureReason("openai_context_window_glm_failover")

// IsOpenAIContextWindowGLMFailover reports a context-window failure that only
// happened because this account mapped the request onto a GLM model.
func (e *UpstreamFailoverError) IsOpenAIContextWindowGLMFailover() bool {
	return e != nil && e.Reason == openAIContextWindowGLMFailoverReason
}

// OpenAIRequestBodyTooLargeClientMessage is the fixed downstream message used
// after all account-specific request body limit failovers are exhausted.
const OpenAIRequestBodyTooLargeClientMessage = "Request payload is too large"

const openAIRequestBodyTooLargeReason = GatewayFailureReason("openai_request_body_too_large")

// isOpenAIUpstreamFailedToReadRequestBody matches upstream 400s where the provider
// accepted the connection but could not consume the request payload (common on
// Ollama Cloud for oversized Codex /v1/responses bodies).
func isOpenAIUpstreamFailedToReadRequestBody(upstreamMsg string, upstreamBody []byte) bool {
	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		return strings.Contains(lower, "failed to read request body") ||
			strings.Contains(lower, "unable to read request body") ||
			strings.Contains(lower, "error reading request body")
	}
	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	for _, path := range []string{
		"error.message",
		"response.error.message",
		"message",
	} {
		if match(gjson.GetBytes(upstreamBody, path).String()) {
			return true
		}
	}
	return match(string(upstreamBody))
}

func isOpenAIRequestBodyTooLargeError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if isOpenAIContextWindowError(upstreamMsg, upstreamBody) {
		return false
	}
	if statusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	// Some OpenAI-compatible gateways (notably Ollama Cloud) surface oversized
	// payloads as 400 "failed to read request body" instead of 413.
	return statusCode == http.StatusBadRequest && isOpenAIUpstreamFailedToReadRequestBody(upstreamMsg, upstreamBody)
}

// isOllamaOpenAICompatibleAccount reports accounts whose base_url points at Ollama.
func isOllamaOpenAICompatibleAccount(account *Account) bool {
	if account == nil {
		return false
	}
	base := strings.ToLower(strings.TrimSpace(account.GetCredential("base_url")))
	if base == "" {
		return false
	}
	return strings.Contains(base, "ollama.com") ||
		strings.Contains(base, "://ollama.") ||
		strings.Contains(base, ".ollama.")
}

// openAIOllamaLargeContextModelLadder is the same-account upgrade path used when
// Ollama rejects the currently mapped model (usually glm-5.2) for a large body.
// Only models that the account already exposes via model_mapping destinations
// are eligible; order prefers coding/long-context models first.
var openAIOllamaLargeContextModelLadder = []string{
	"kimi-k2.7-code",
	"kimi-k2.6",
	"kimi-k2.5",
	"qwen3.5:397b",
	"qwen3.5-397b",
	"minimax-m3",
	"mistral-large-3:675b",
	"mistral-large-3-675b",
	"deepseek-v4-pro",
	"nemotron-3-ultra",
	"nemotron-3-super",
}

func accountExposesOpenAIUpstreamModel(account *Account, model string) bool {
	if account == nil {
		return false
	}
	want := strings.TrimSpace(model)
	if want == "" {
		return false
	}
	wantLower := strings.ToLower(want)
	for _, mapped := range account.GetModelMapping() {
		if strings.EqualFold(strings.TrimSpace(mapped), want) {
			return true
		}
	}
	// Also accept exact inbound aliases that map to themselves.
	if mapped := strings.TrimSpace(account.GetMappedModel(want)); strings.EqualFold(mapped, want) || strings.EqualFold(mapped, wantLower) {
		return true
	}
	return false
}

// shouldRetryOpenAISameAccountLargeContextModel decides whether the current
// upstream rejection can be salvaged by switching to another model on the same
// Ollama account before failing over to a different account.
func shouldRetryOpenAISameAccountLargeContextModel(
	account *Account,
	statusCode int,
	upstreamMsg string,
	upstreamBody []byte,
	currentUpstreamModel string,
	tried map[string]struct{},
) (nextModel string, ok bool) {
	if !isOllamaOpenAICompatibleAccount(account) {
		return "", false
	}
	// Same trigger set as body-limit / context-window salvage: the payload is
	// too heavy for the current model/gateway path, but another long-context
	// model on this account may still accept it.
	if !isOpenAIRequestBodyTooLargeError(statusCode, upstreamMsg, upstreamBody) &&
		!isOpenAIContextWindowError(upstreamMsg, upstreamBody) &&
		!isOpenAIUpstreamFailedToReadRequestBody(upstreamMsg, upstreamBody) {
		return "", false
	}
	if tried == nil {
		tried = map[string]struct{}{}
	}
	current := strings.TrimSpace(currentUpstreamModel)
	if current != "" {
		tried[strings.ToLower(current)] = struct{}{}
	}
	for _, candidate := range openAIOllamaLargeContextModelLadder {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		if _, seen := tried[key]; seen {
			continue
		}
		if !accountExposesOpenAIUpstreamModel(account, candidate) {
			continue
		}
		// Prefer the canonical destination name from mapping when present.
		mapped := strings.TrimSpace(account.GetMappedModel(candidate))
		if mapped == "" {
			mapped = candidate
		}
		mappedKey := strings.ToLower(mapped)
		if _, seen := tried[mappedKey]; seen {
			continue
		}
		return mapped, true
	}
	return "", false
}

// openAISameAccountVisionModelLadder is the same-account multimodal fallback
// path. Image requests prefer remapping models on the selected account over
// hopping accounts solely because the first VL destination is busy/broken.
var openAISameAccountVisionModelLadder = []string{
	"meta/llama-3.2-11b-vision-instruct",
	"meta/llama-3.2-90b-vision-instruct",
	"nvidia/nemotron-nano-12b-v2-vl",
	"nvidia/llama-3.1-nemotron-nano-vl-8b-v1",
	"microsoft/phi-3-vision-128k-instruct",
	"qwen3.7-plus",
	"mimo-v2.5",
	"gemma4:31b",
	"gemma4-31b",
	"google/gemma-3-12b-it",
	"google/gemma-3-4b-it",
}

// shouldRetryOpenAISameAccountVisionModel decides whether an image-input
// upstream rejection can be salvaged by another multimodal model already
// present on the same multi-model account.
func shouldRetryOpenAISameAccountVisionModel(
	account *Account,
	statusCode int,
	upstreamMsg string,
	upstreamBody []byte,
	currentUpstreamModel string,
	tried map[string]struct{},
) (nextModel string, ok bool) {
	if account == nil || !account.IsOpenAICompatible() {
		return "", false
	}
	// Only salvage transient / model-level rejections. Auth and hard client
	// errors should not invent alternative vision destinations.
	if statusCode != http.StatusTooManyRequests &&
		statusCode != http.StatusServiceUnavailable &&
		statusCode != http.StatusBadGateway &&
		statusCode != http.StatusGatewayTimeout &&
		statusCode != http.StatusInternalServerError &&
		statusCode != http.StatusNotFound &&
		statusCode != http.StatusBadRequest {
		return "", false
	}
	msg := strings.ToLower(strings.TrimSpace(upstreamMsg) + " " + string(upstreamBody))
	// Do not spin VL models on pure context-window or auth failures.
	if isOpenAIContextWindowError(upstreamMsg, upstreamBody) {
		return "", false
	}
	if strings.Contains(msg, "invalid api key") || strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "authentication") || strings.Contains(msg, "permission") {
		return "", false
	}
	if tried == nil {
		tried = map[string]struct{}{}
	}
	current := strings.TrimSpace(currentUpstreamModel)
	if current != "" {
		tried[strings.ToLower(current)] = struct{}{}
	}

	// Prefer explicit vision mapping destinations, then catalog inference,
	// then a fixed ladder of common VL ids if the account exposes them.
	candidates := make([]string, 0, 16)
	for _, mapped := range account.GetVisionModelMapping() {
		if mapped = strings.TrimSpace(mapped); mapped != "" {
			candidates = append(candidates, mapped)
		}
	}
	if inferred, found := account.ResolveInferredVisionModel(current); found {
		candidates = append(candidates, inferred)
	}
	candidates = append(candidates, openAISameAccountVisionModelLadder...)

	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		if _, seen := tried[key]; seen {
			continue
		}
		// Candidate must either be a known mapping destination or an explicit
		// vision_model_mapping target (accountExposes checks model_mapping).
		exposed := false
		for _, dest := range account.GetModelMapping() {
			if strings.EqualFold(strings.TrimSpace(dest), candidate) || strings.EqualFold(strings.TrimSpace(dest), strings.TrimSpace(candidate)) {
				exposed = true
				break
			}
		}
		if !exposed {
			for src, dest := range account.GetModelMapping() {
				if strings.EqualFold(strings.TrimSpace(src), candidate) {
					exposed = true
					candidate = strings.TrimSpace(dest)
					if candidate == "" {
						candidate = strings.TrimSpace(src)
					}
					break
				}
			}
		}
		if !exposed {
			for _, dest := range account.GetVisionModelMapping() {
				if strings.EqualFold(strings.TrimSpace(dest), candidate) {
					exposed = true
					break
				}
			}
		}
		if !exposed {
			continue
		}
		mapped := strings.TrimSpace(candidate)
		mappedKey := strings.ToLower(mapped)
		if _, seen := tried[mappedKey]; seen {
			continue
		}
		// Skip pure text defaults such as glm-5.2.
		if openAIVisionModelNameScore(mapped) <= 0 && openAIVisionModelNameScore(candidate) <= 0 {
			continue
		}
		return mapped, true
	}
	return "", false
}


// preferOpenAIVisionChatFallback reports whether an image-input /v1/responses
// request should be converted to chat completions on this multi-model account.
// Used for ollama/nvidia/opencode pools where force_responses is flaky for VL.
func preferOpenAIVisionChatFallback(account *Account, requestedModel string) bool {
	if account == nil || !account.IsOpenAICompatible() || account.Type != AccountTypeAPIKey {
		return false
	}
	if !account.SupportsOpenAIVisionModel(requestedModel) {
		return false
	}
	base := strings.ToLower(strings.TrimSpace(account.GetCredential("base_url")))
	if isOllamaOpenAICompatibleAccount(account) ||
		strings.Contains(base, "nvidia.com") ||
		strings.Contains(base, "opencode.ai") {
		return true
	}
	// Explicit vision mapping on a multi-model catalog is also a strong signal.
	if len(account.GetVisionModelMapping()) > 0 && len(account.GetModelMapping()) > 1 {
		return true
	}
	return false
}

func newOpenAIUpstreamFailoverError(
	statusCode int,
	responseHeaders http.Header,
	responseBody []byte,
	upstreamMsg string,
	retryableOnSameAccount bool,
	account *Account,
	requestedModel string,
) *UpstreamFailoverError {
	failoverErr := &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           responseBody,
		ResponseHeaders:        responseHeaders.Clone(),
		RetryableOnSameAccount: retryableOnSameAccount,
	}
	if shouldFailoverOpenAIContextWindow(account, requestedModel, upstreamMsg, responseBody) {
		// Context-window rejections are deterministic for a given account/model.
		// Never retry the same account; switch to another account and prefer non-GLM.
		failoverErr.RetryableOnSameAccount = false
		failoverErr.Scope = GatewayFailureScopeAccount
		failoverErr.Reason = openAIContextWindowGLMFailoverReason
		failoverErr.NextAccountAction = NextAccountRetry
		return failoverErr
	}
	if isOpenAIRequestBodyTooLargeError(statusCode, upstreamMsg, responseBody) {
		failoverErr.RetryableOnSameAccount = false
		failoverErr.Scope = GatewayFailureScopeAccount
		failoverErr.Reason = openAIRequestBodyTooLargeReason
		failoverErr.NextAccountAction = NextAccountRetry
		failoverErr.ClientStatusCode = http.StatusRequestEntityTooLarge
		failoverErr.ClientMessage = OpenAIRequestBodyTooLargeClientMessage
	}
	return failoverErr
}

// IsOpenAIRequestBodyTooLarge reports whether another account may accept the
// same request even though the selected account rejected its serialized size.
func (e *UpstreamFailoverError) IsOpenAIRequestBodyTooLarge() bool {
	return e != nil && e.Reason == openAIRequestBodyTooLargeReason
}

func marshalOpenAIUpstreamJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

func openAIUpstreamErrorBodyReadLimitForConfig(cfg *config.Config) int64 {
	limit := openAIUpstreamErrorBodyReadLimit
	if cfg != nil && cfg.Gateway.LogUpstreamErrorBody && cfg.Gateway.LogUpstreamErrorBodyMaxBytes > int(limit) {
		limit = int64(cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
	}
	return limit
}

func (s *OpenAIGatewayService) readUpstreamErrorBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	cfg := (*config.Config)(nil)
	if s != nil {
		cfg = s.cfg
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, openAIUpstreamErrorBodyReadLimitForConfig(cfg)))
	return body
}

func (s *OpenAIGatewayService) handleFailoverSideEffects(ctx context.Context, resp *http.Response, account *Account, responseBody []byte, canonicalModel ...string) {
	if len(canonicalModel) > 0 {
		s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, responseBody, canonicalModel[0])
		return
	}
	s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, responseBody)
}

func (s *OpenAIGatewayService) handleErrorResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body := s.readUpstreamErrorBody(resp)
	body = s.redactAgentIdentitySensitiveBody(ctx, account, body)

	// cyber_policy 硬阻断：透传上游原始错误体给客户端（不重包成通用 502），不冷却账号。
	// 当前请求恒透传（需求1）；标记供 handler 事后写风控/邮件。400 cyber 不可 failover
	// （shouldFailoverUpstreamError(400)=false），故走到此处即可安全早返回。
	if hit, code, cyberMsg := detectOpenAICyberPolicy(body); hit {
		MarkOpsCyberPolicy(c, CyberPolicyMark{
			Code:           code,
			Message:        cyberMsg,
			Body:           truncateString(string(body), 4096),
			UpstreamStatus: resp.StatusCode,
		})
		setOpsUpstreamError(c, resp.StatusCode, cyberMsg, truncateString(string(body), 2048))
		writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}
		c.Data(resp.StatusCode, contentType, body)
		if cyberMsg == "" {
			return nil, fmt.Errorf("openai cyber_policy: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("openai cyber_policy: %s", cyberMsg)
	}

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)
	logGrokModelInputDebug(ctx, c, account, resp.StatusCode, requestBody, body)

	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		logger.LegacyPrintf("service.openai_gateway",
			"OpenAI upstream error %d (account=%d platform=%s type=%s): %s",
			resp.StatusCode,
			account.ID,
			account.Platform,
			account.Type,
			truncateForLog(body, s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes),
		)
	}

	reqModelForFailover := ""
	if len(requestedModel) > 0 {
		reqModelForFailover = strings.TrimSpace(requestedModel[0])
	}
	if isOpenAIRequestBodyTooLargeError(resp.StatusCode, upstreamMsg, body) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "failover",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, requestedModel...)
		return nil, newOpenAIUpstreamFailoverError(
			resp.StatusCode,
			resp.Header,
			body,
			upstreamMsg,
			false,
			account,
			reqModelForFailover,
		)
	}

	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		PlatformOpenAI,
		resp.StatusCode,
		body,
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	); matched {
		MarkResponseCommitted(c)
		c.JSON(status, gin.H{
			"error": gin.H{
				"type":    errType,
				"message": errMsg,
			},
		})
		if upstreamMsg == "" {
			upstreamMsg = errMsg
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Check custom error codes
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"type":    "upstream_error",
				"message": "Upstream gateway error",
			},
		})
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Handle upstream error (mark account status)
	var reqModel string
	if len(requestedModel) > 0 {
		reqModel = strings.TrimSpace(requestedModel[0])
	}
	if reqModel == "" {
		reqModel, _, _ = extractOpenAIRequestMetaFromBody(requestBody)
		reqModel = canonicalOpenAIAccountSchedulingModel(account, reqModel)
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, reqModel)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           body,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	MarkResponseCommitted(c)

	// Return appropriate error response
	var errType, errMsg string
	var statusCode int

	switch resp.StatusCode {
	case 401:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream authentication failed, please contact administrator"
	case 402:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream payment required: insufficient balance or billing issue"
	case 403:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream access forbidden, please contact administrator"
	case 429:
		statusCode = http.StatusTooManyRequests
		errType = "rate_limit_error"
		errMsg = "Upstream rate limit exceeded, please retry later"
	default:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream request failed"
	}
	if isOpenAIContextWindowError(upstreamMsg, body) && upstreamMsg != "" {
		errMsg = upstreamMsg
	}

	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": errMsg,
		},
	})

	if upstreamMsg == "" {
		return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
}

// compatErrorWriter is the signature for format-specific error writers used by
// the compat paths (Chat Completions and Anthropic Messages).
type compatErrorWriter func(c *gin.Context, statusCode int, errType, message string)

// handleCompatErrorResponse is the shared non-failover error handler for the
// Chat Completions and Anthropic Messages compat paths. It mirrors the logic of
// handleErrorResponse (passthrough rules, ShouldHandleErrorCode, rate-limit
// tracking, secondary failover) but delegates the final error write to the
// format-specific writer function.
func (s *OpenAIGatewayService) handleCompatErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
	writeError compatErrorWriter,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body := s.readUpstreamErrorBody(resp)
	body = s.redactAgentIdentitySensitiveBody(context.Background(), account, body)

	// cyber_policy：兼容路径（Chat Completions / Anthropic）以各自格式回写错误，
	// 不原样透传 responses 格式的 cyber body（否则对下游格式不合法）。cyber 是上游网络
	// 安全策略拦截，不冷却账号，故标记后直接以兼容格式回写错误并返回，跳过下方
	// handleOpenAIAccountUpstreamError（避免自定义 temp-unschedulable 规则误冷却）。
	if hit, code, cyberMsg := detectOpenAICyberPolicy(body); hit {
		MarkOpsCyberPolicy(c, CyberPolicyMark{
			Code:           code,
			Message:        cyberMsg,
			Body:           truncateString(string(body), 4096),
			UpstreamStatus: resp.StatusCode,
		})
		setOpsUpstreamError(c, resp.StatusCode, cyberMsg, truncateString(string(body), 2048))
		clientMsg := cyberMsg
		if clientMsg == "" {
			clientMsg = "Request blocked by upstream cyber-security policy"
		}
		writeError(c, resp.StatusCode, "invalid_request_error", clientMsg)
		if cyberMsg == "" {
			return nil, fmt.Errorf("openai cyber_policy: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("openai cyber_policy: %s", cyberMsg)
	}

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("Upstream error: %d", resp.StatusCode)
	}
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)

	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)

	// Apply error passthrough rules
	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c, account.Platform, resp.StatusCode, body,
		http.StatusBadGateway, "api_error", "Upstream request failed",
	); matched {
		MarkResponseCommitted(c)
		writeError(c, status, errType, errMsg)
		if upstreamMsg == "" {
			upstreamMsg = errMsg
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Check custom error codes — if the account does not handle this status,
	// return a generic error without exposing upstream details.
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		writeError(c, http.StatusInternalServerError, "api_error", "Upstream gateway error")
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Track rate limits and decide whether to trigger secondary failover.
	var modelForCooldown string
	if len(requestedModel) > 0 {
		modelForCooldown = requestedModel[0]
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(
		c.Request.Context(), account, resp.StatusCode, resp.Header, body, modelForCooldown,
	)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           body,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	MarkResponseCommitted(c)

	// Map status code to error type and write response
	errType := "api_error"
	switch {
	case resp.StatusCode == 400:
		errType = "invalid_request_error"
	case resp.StatusCode == 404:
		errType = "not_found_error"
	case resp.StatusCode == 429:
		errType = "rate_limit_error"
	case resp.StatusCode >= 500:
		errType = "api_error"
	}

	writeError(c, resp.StatusCode, errType, upstreamMsg)
	return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
}


type openaiSkipGLMMappedAccountsContextKey struct{}

// WithSkipGLMMappedAccountsForContextWindow marks subsequent account selection to
// skip accounts whose model_mapping still routes the request to GLM.
func WithSkipGLMMappedAccountsForContextWindow(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openaiSkipGLMMappedAccountsContextKey{}, true)
}

func shouldSkipGLMMappedAccountForContextWindow(ctx context.Context, account *Account, requestedModel string) bool {
	if ctx == nil {
		return false
	}
	skip, _ := ctx.Value(openaiSkipGLMMappedAccountsContextKey{}).(bool)
	if !skip {
		return false
	}
	return accountMapsRequestToGLM(account, requestedModel)
}
