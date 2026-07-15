package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokComposerImageBridgeVisionModel     = "grok-build-0.1"
	grokComposerImageBridgeMaxOutputTokens = 512
	grokCustomToolInputSchema              = `{"type":"object","properties":{"input":{"type":"string","description":"The raw input for this tool, passed through verbatim."}},"required":["input"]}`
	grokToolSearchProxyName                = "tool_search"
	grokToolSearchProxySchema              = `{"type":"object","properties":{"query":{"type":"string","description":"Search query for tools or connectors to load."},"limit":{"type":"integer","description":"Maximum number of tool groups to return."}},"required":["query"]}`
	grokChatToolNameMaxLen                 = 64
)

type grokResponsesToolBridge struct {
	CustomTools        map[string]bool
	ToolSearchDeclared bool
	NamespaceTools     map[string]apicompat.NamespacedToolName
}

// grokResponsesToolBridgeSession 在单次请求内携带工具还原元数据，并在流式
// 场景下用 item_id/call_id 跟踪 custom 工具调用（xAI 的 arguments 增量事件通常不带 name）。
type grokResponsesToolBridgeSession struct {
	Bridge       grokResponsesToolBridge
	customItemID map[string]struct{}
}

type grokResponsesToolBridgeContextKey struct{}

func (s *OpenAIGatewayService) forwardGrokResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if account.Type != AccountTypeOAuth {
		return nil, fmt.Errorf("grok account type %s is not supported by subscription forwarding", account.Type)
	}

	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = "grok-4.3"
	}
	patchedBody, err := patchGrokResponsesBody(body, upstreamModel)
	if err != nil {
		return nil, err
	}
	toolBridge := extractGrokResponsesToolBridge(body)
	if toolBridge.hasClientExecutableTools() {
		ctx = context.WithValue(ctx, grokResponsesToolBridgeContextKey{}, &grokResponsesToolBridgeSession{
			Bridge:       toolBridge,
			customItemID: make(map[string]struct{}),
		})
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	upstreamReq, err := buildGrokResponsesRequest(upstreamCtx, c, account, patchedBody, token)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamStart := time.Now()
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		s.updateGrokUsageSnapshot(ctx, account.ID, xai.ParseQuotaHeaders(resp.Header, resp.StatusCode))
		upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
		if upstreamMsg == "" {
			upstreamMsg = fmt.Sprintf("xAI upstream returned status %d", resp.StatusCode)
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
			Kind:               "failover",
			Message:            upstreamMsg,
		})
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return s.handleErrorResponse(ctx, resp, c, account, patchedBody, upstreamModel)
	}

	s.updateGrokUsageSnapshot(ctx, account.ID, xai.ParseQuotaHeaders(resp.Header, resp.StatusCode))

	var usage *OpenAIUsage
	var firstTokenMs *int
	responseID := ""
	if reqStream {
		streamResult, err := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, upstreamModel)
		if err != nil {
			return nil, err
		}
		usage = streamResult.usage
		firstTokenMs = streamResult.firstTokenMs
		responseID = strings.TrimSpace(streamResult.responseID)
	} else {
		nonStreamResult, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
		if err != nil {
			return nil, err
		}
		usage = nonStreamResult.usage
		responseID = strings.TrimSpace(nonStreamResult.responseID)
	}

	if usage == nil {
		usage = &OpenAIUsage{}
	}
	reasoningEffort := extractOpenAIReasoningEffortFromBody(patchedBody, originalModel)
	return &OpenAIForwardResult{
		RequestID:       firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
		ResponseID:      responseID,
		Usage:           *usage,
		Model:           originalModel,
		UpstreamModel:   upstreamModel,
		ReasoningEffort: reasoningEffort,
		Stream:          reqStream,
		OpenAIWSMode:    false,
		ResponseHeaders: resp.Header.Clone(),
		Duration:        time.Since(startTime),
		FirstTokenMs:    firstTokenMs,
	}, nil
}

func patchGrokResponsesBody(body []byte, upstreamModel string) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid json request body")
	}
	out, err := sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesModelCapabilities(out, upstreamModel)
	if err != nil {
		return nil, err
	}
	for _, unsupportedField := range []string{"prompt_cache_retention", "safety_identifier"} {
		if gjson.GetBytes(out, unsupportedField).Exists() {
			out, err = sjson.DeleteBytes(out, unsupportedField)
			if err != nil {
				return nil, err
			}
		}
	}
	if strings.EqualFold(upstreamModel, "grok-4.5") {
		for _, unsupportedField := range []string{"presence_penalty", "presencePenalty", "frequency_penalty", "frequencyPenalty", "stop"} {
			if gjson.GetBytes(out, unsupportedField).Exists() {
				out, err = sjson.DeleteBytes(out, unsupportedField)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	out, err = sanitizeGrokResponsesUnsupportedFields(out)
	if err != nil {
		return nil, err
	}
	out, err = liftGrokResponsesAdditionalTools(out)
	if err != nil {
		return nil, err
	}
	out, err = ensureGrokResponsesWebSearchTool(out)
	if err != nil {
		return nil, err
	}
	out, err = normalizeGrokResponsesClientExecutableTools(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesTools(out)
	if err != nil {
		return nil, err
	}
	out, err = stripGrokUnsupportedReasoningItems(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokReasoningNullContent(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesModelInput(out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func sanitizeGrokResponsesModelCapabilities(body []byte, upstreamModel string) ([]byte, error) {
	if !grokModelRejectsReasoningEffort(upstreamModel) {
		return body, nil
	}

	out := body
	for _, field := range []string{"reasoning", "reasoning_effort", "reasoningEffort"} {
		if !gjson.GetBytes(out, field).Exists() {
			continue
		}
		var err error
		out, err = sjson.DeleteBytes(out, field)
		if err != nil {
			return nil, fmt.Errorf("remove unsupported Grok Composer %s: %w", field, err)
		}
	}
	return out, nil
}

func grokModelRejectsReasoningEffort(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = strings.TrimSpace(model[slash+1:])
	}
	switch model {
	case "grok-composer", "grok-composer-2.5-fast", "composer-2.5":
		return true
	default:
		return false
	}
}

func liftGrokResponsesAdditionalTools(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}

	liftedTools := make([]json.RawMessage, 0)
	for _, item := range input.Array() {
		if !item.IsObject() || strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
			continue
		}
		tools := item.Get("tools")
		if !tools.IsArray() {
			continue
		}
		for _, tool := range tools.Array() {
			if normalized, err := normalizeGrokAdditionalTools(tool); err != nil {
				return nil, err
			} else if len(normalized) > 0 {
				liftedTools = append(liftedTools, normalized...)
			}
		}
	}
	if len(liftedTools) == 0 {
		return body, nil
	}

	existing := gjson.GetBytes(body, "tools")
	merged := make([]json.RawMessage, 0, len(liftedTools)+len(existing.Array()))
	if existing.IsArray() {
		for _, tool := range existing.Array() {
			merged = append(merged, json.RawMessage(tool.Raw))
		}
	}
	merged = append(merged, liftedTools...)
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "tools", encoded)
}

func ensureGrokResponsesWebSearchTool(body []byte) ([]byte, error) {
	if !hasGrokResponsesAdditionalTools(body) || hasGrokResponsesToolType(body, "web_search") {
		return body, nil
	}

	existing := gjson.GetBytes(body, "tools")
	merged := make([]json.RawMessage, 0, len(existing.Array())+1)
	if existing.IsArray() {
		for _, tool := range existing.Array() {
			merged = append(merged, json.RawMessage(tool.Raw))
		}
	}
	merged = append(merged, json.RawMessage(`{"type":"web_search"}`))
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "tools", encoded)
}

func hasGrokResponsesAdditionalTools(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if item.IsObject() && strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
			return true
		}
	}
	return false
}

func hasGrokResponsesToolType(body []byte, toolType string) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == toolType {
			return true
		}
	}
	return false
}

func normalizeGrokAdditionalTools(tool gjson.Result) ([]json.RawMessage, error) {
	if !tool.IsObject() {
		return nil, nil
	}
	normalized, err := grokResponsesToolToUpstreamTools(tool)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

func normalizeGrokResponsesClientExecutableTools(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body, nil
	}
	rawTools := tools.Array()
	normalizedTools := make([]json.RawMessage, 0, len(rawTools))
	changed := false
	for _, tool := range rawTools {
		normalized, err := grokResponsesToolToUpstreamTools(tool)
		if err != nil {
			return nil, err
		}
		if len(normalized) != 1 || (len(normalized) == 1 && string(normalized[0]) != tool.Raw) {
			changed = true
		}
		normalizedTools = append(normalizedTools, normalized...)
	}
	if !changed {
		return body, nil
	}
	if len(normalizedTools) == 0 {
		return sjson.DeleteBytes(body, "tools")
	}
	encoded, err := json.Marshal(normalizedTools)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "tools", encoded)
}

func grokResponsesToolToUpstreamTools(tool gjson.Result) ([]json.RawMessage, error) {
	if !tool.IsObject() {
		return nil, nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(tool.Raw), &payload); err != nil {
		return nil, err
	}
	toolType := strings.TrimSpace(firstNonEmptyString(payload["type"]))
	switch toolType {
	case "custom":
		name := strings.TrimSpace(firstNonEmptyString(payload["name"]))
		if name == "" {
			return nil, nil
		}
		return []json.RawMessage{marshalGrokFunctionToolPayload(name, firstNonEmptyString(payload["description"]), json.RawMessage(grokCustomToolInputSchema), payload["strict"])}, nil
	case "tool_search":
		return []json.RawMessage{marshalGrokFunctionToolPayload(grokToolSearchProxyName, "Search and load Codex tools, plugins, connectors, and MCP namespaces for the current task.", json.RawMessage(grokToolSearchProxySchema), nil)}, nil
	case "namespace":
		return grokNamespaceChildrenToFunctionTools(payload)
	case "local_shell":
		// Codex local_shell is a client-side tool. xAI's hosted shell tool has a
		// different schema and needs an environment, so it is not promoted here.
		return nil, nil
	default:
		if _, ok := grokResponsesSupportedToolTypes[toolType]; !ok || !isValidGrokResponsesTool(tool) {
			return nil, nil
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{raw}, nil
	}
}

func grokNamespaceChildrenToFunctionTools(payload map[string]any) ([]json.RawMessage, error) {
	namespace := strings.TrimSpace(firstNonEmptyString(payload["name"], payload["namespace"]))
	if namespace == "" {
		return nil, nil
	}
	children, _ := payload["tools"].([]any)
	if len(children) == 0 {
		children, _ = payload["children"].([]any)
	}
	out := make([]json.RawMessage, 0, len(children))
	for _, rawChild := range children {
		child, ok := rawChild.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(child["type"])) != "function" {
			continue
		}
		name := strings.TrimSpace(firstNonEmptyString(child["name"]))
		if name == "" {
			continue
		}
		parameters := json.RawMessage(`{"type":"object","properties":{}}`)
		if raw, ok := child["parameters"]; ok {
			if encoded, err := json.Marshal(raw); err == nil && json.Valid(encoded) {
				parameters = encoded
			}
		}
		out = append(out, marshalGrokFunctionToolPayload(grokFlattenNamespaceToolName(namespace, name), firstNonEmptyString(child["description"]), parameters, child["strict"]))
	}
	return out, nil
}

func marshalGrokFunctionToolPayload(name, description string, parameters json.RawMessage, strict any) json.RawMessage {
	payload := map[string]any{
		"type": "function",
		"name": name,
	}
	if description != "" {
		payload["description"] = description
	}
	if len(parameters) > 0 && json.Valid(parameters) {
		var decoded any
		if err := json.Unmarshal(parameters, &decoded); err == nil {
			payload["parameters"] = decoded
		}
	}
	if strict != nil {
		payload["strict"] = strict
	}
	raw, _ := json.Marshal(payload)
	return raw
}

func grokFlattenNamespaceToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= grokChatToolNameMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(sum[:4])
	prefixLen := grokChatToolNameMaxLen - len(suffix)
	var prefix strings.Builder
	for _, ch := range full {
		if prefix.Len()+len(string(ch)) > prefixLen {
			break
		}
		_, _ = prefix.WriteRune(ch)
	}
	return prefix.String() + suffix
}

func extractGrokResponsesToolBridge(body []byte) grokResponsesToolBridge {
	var req apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return grokResponsesToolBridge{}
	}
	tools, err := apicompat.EffectiveResponsesTools(&req)
	if err != nil {
		return grokResponsesToolBridge{}
	}
	return grokResponsesToolBridge{
		CustomTools:        apicompat.CustomToolNames(tools),
		ToolSearchDeclared: apicompat.HasToolSearchTool(tools),
		NamespaceTools:     apicompat.NamespaceToolNames(tools),
	}
}

func (b grokResponsesToolBridge) hasClientExecutableTools() bool {
	return len(b.CustomTools) > 0 || b.ToolSearchDeclared || len(b.NamespaceTools) > 0
}

func grokResponsesToolBridgeSessionFromContext(ctx context.Context) (*grokResponsesToolBridgeSession, bool) {
	if ctx == nil {
		return nil, false
	}
	session, ok := ctx.Value(grokResponsesToolBridgeContextKey{}).(*grokResponsesToolBridgeSession)
	if !ok || session == nil || !session.Bridge.hasClientExecutableTools() {
		return nil, false
	}
	return session, true
}

func grokResponsesToolBridgeFromContext(ctx context.Context) (grokResponsesToolBridge, bool) {
	session, ok := grokResponsesToolBridgeSessionFromContext(ctx)
	if !ok {
		return grokResponsesToolBridge{}, false
	}
	return session.Bridge, true
}

func (s *grokResponsesToolBridgeSession) rememberCustomItem(ids ...string) {
	if s == nil {
		return
	}
	if s.customItemID == nil {
		s.customItemID = make(map[string]struct{})
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		s.customItemID[id] = struct{}{}
	}
}

func (s *grokResponsesToolBridgeSession) isCustomItem(id string) bool {
	if s == nil || s.customItemID == nil {
		return false
	}
	_, ok := s.customItemID[strings.TrimSpace(id)]
	return ok
}

func transformGrokResponsesToolBridgeBody(ctx context.Context, body []byte) []byte {
	bridge, ok := grokResponsesToolBridgeFromContext(ctx)
	if !ok || len(bytes.TrimSpace(body)) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	updated := body
	changed := false
	for _, path := range []string{"output", "response.output"} {
		next, pathChanged := transformGrokResponsesToolBridgeOutputArray(updated, path, bridge)
		if pathChanged {
			updated = next
			changed = true
		}
	}
	if !changed {
		return body
	}
	return updated
}

func transformGrokResponsesToolBridgeSSEData(ctx context.Context, data []byte) (updated []byte, changed bool, drop bool) {
	session, ok := grokResponsesToolBridgeSessionFromContext(ctx)
	if !ok || len(bytes.TrimSpace(data)) == 0 || !gjson.ValidBytes(data) {
		return data, false, false
	}
	bridge := session.Bridge
	updated = data
	if item := gjson.GetBytes(updated, "item"); item.Exists() && item.IsObject() {
		if transformed, itemChanged := transformGrokResponsesToolBridgeOutputItem([]byte(item.Raw), bridge); itemChanged {
			if next, err := sjson.SetRawBytes(updated, "item", transformed); err == nil {
				updated = next
				changed = true
			}
		}
		itemType := strings.TrimSpace(gjson.GetBytes(updated, "item.type").String())
		if itemType == "custom_tool_call" || bridge.CustomTools[strings.TrimSpace(gjson.GetBytes(updated, "item.name").String())] {
			session.rememberCustomItem(
				gjson.GetBytes(updated, "item.id").String(),
				gjson.GetBytes(updated, "item.call_id").String(),
				gjson.GetBytes(updated, "item_id").String(),
			)
		}
	}
	eventType := strings.TrimSpace(gjson.GetBytes(updated, "type").String())
	if eventType == "response.function_call_arguments.delta" {
		next, eventChanged, eventDrop := transformGrokResponsesToolBridgeFunctionArgumentsDelta(updated, session)
		if eventDrop {
			return data, true, true
		}
		if eventChanged {
			updated = next
			changed = true
		}
	}
	if eventType == "response.function_call_arguments.done" {
		next, eventChanged, eventDrop := transformGrokResponsesToolBridgeFunctionArgumentsDone(updated, session)
		if eventDrop {
			return data, true, true
		}
		if eventChanged {
			updated = next
			changed = true
		}
	}
	for _, path := range []string{"output", "response.output"} {
		next, pathChanged := transformGrokResponsesToolBridgeOutputArray(updated, path, bridge)
		if pathChanged {
			updated = next
			changed = true
		}
	}
	return updated, changed, false
}

func transformGrokResponsesToolBridgeOutputArray(data []byte, path string, bridge grokResponsesToolBridge) ([]byte, bool) {
	output := gjson.GetBytes(data, path)
	if !output.Exists() || !output.IsArray() {
		return data, false
	}
	items := output.Array()
	updatedItems := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		raw := []byte(item.Raw)
		transformed, itemChanged := transformGrokResponsesToolBridgeOutputItem(raw, bridge)
		if itemChanged {
			changed = true
		}
		updatedItems = append(updatedItems, json.RawMessage(transformed))
	}
	if !changed {
		return data, false
	}
	encoded, err := json.Marshal(updatedItems)
	if err != nil {
		return data, false
	}
	updated, err := sjson.SetRawBytes(data, path, encoded)
	if err != nil {
		return data, false
	}
	return updated, true
}

func transformGrokResponsesToolBridgeOutputItem(raw []byte, bridge grokResponsesToolBridge) ([]byte, bool) {
	if strings.TrimSpace(gjson.GetBytes(raw, "type").String()) != "function_call" {
		return raw, false
	}
	name := strings.TrimSpace(gjson.GetBytes(raw, "name").String())
	if name == "" {
		return raw, false
	}
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		return raw, false
	}
	arguments := strings.TrimSpace(gjson.GetBytes(raw, "arguments").String())
	switch {
	case bridge.CustomTools[name]:
		item["type"] = "custom_tool_call"
		item["input"] = grokExtractCustomToolCallInput(arguments)
		delete(item, "arguments")
	case bridge.ToolSearchDeclared && name == grokToolSearchProxyName:
		item["type"] = "tool_search_call"
		item["execution"] = "client"
		item["arguments"] = grokToolSearchArgumentsValue(arguments)
		delete(item, "name")
	case bridge.NamespaceTools != nil:
		ns, exists := bridge.NamespaceTools[name]
		if !exists {
			return raw, false
		}
		item["name"] = ns.Name
		item["namespace"] = ns.Namespace
	default:
		return raw, false
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

func transformGrokResponsesToolBridgeFunctionArgumentsDelta(data []byte, session *grokResponsesToolBridgeSession) ([]byte, bool, bool) {
	if !grokSSEEventTargetsCustomTool(data, session) {
		return data, false, false
	}
	// custom/freeform 的 arguments 是包裹 input 的 JSON；中间增量无法稳定还原成自由文本，
	// 对齐 apicompat：流中丢弃 delta，收尾时用 custom_tool_call_input.done 一次性下发。
	return data, true, true
}

func transformGrokResponsesToolBridgeFunctionArgumentsDone(data []byte, session *grokResponsesToolBridgeSession) ([]byte, bool, bool) {
	if !grokSSEEventTargetsCustomTool(data, session) {
		return data, false, false
	}
	input := grokExtractCustomToolCallInput(gjson.GetBytes(data, "arguments").String())
	updated, err := sjson.SetBytes(data, "type", "response.custom_tool_call_input.done")
	if err != nil {
		return data, false, false
	}
	updated, err = sjson.SetBytes(updated, "input", input)
	if err != nil {
		return data, false, false
	}
	updated, _ = sjson.DeleteBytes(updated, "arguments")
	return updated, true, false
}

func grokSSEEventTargetsCustomTool(data []byte, session *grokResponsesToolBridgeSession) bool {
	if session == nil {
		return false
	}
	name := strings.TrimSpace(gjson.GetBytes(data, "name").String())
	if name != "" && session.Bridge.CustomTools[name] {
		return true
	}
	for _, key := range []string{"item_id", "call_id", "id"} {
		if session.isCustomItem(gjson.GetBytes(data, key).String()) {
			return true
		}
	}
	return false
}

func grokExtractCustomToolCallInput(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return trimmed
	}
	if raw, ok := obj["input"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return trimmed
	}
	if len(obj) == 0 {
		return ""
	}
	return trimmed
}

func grokToolSearchArgumentsValue(arguments string) any {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return map[string]any{}
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
		return decoded
	}
	return arguments
}

// stripGrokUnsupportedReasoningItems 移除 input 中携带 encrypted_content 的 reasoning 项。
//
// 背景：Codex 以 OpenAI Responses 协议工作，会在每轮把上一轮返回的 reasoning.encrypted_content
// 原样回传。但 grok/xAI 无法解码 Codex 从普通 /v1/responses 回传的加密推理内容 —— 第二轮起会返回
// 400 "Could not decode the compaction blob. Ensure it is unmodified from the compact response."
// 已验证 blob 字节在请求/响应两侧均未被 sub2api 改动、token 也未轮换，属于 grok 协议不兼容。
//
// 因此在转发给 grok 前剥离这些 reasoning 项，让 grok 用普通消息历史续接对话。
// 代价：损失跨轮的显式推理链延续，但对话内容与正确性不受影响；否则每轮第二次请求必定失败。
// 仅按 encrypted_content 判定，只影响 grok 加密推理场景，不误删普通 message/工具项。
func stripGrokUnsupportedReasoningItems(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	kept := make([]json.RawMessage, 0, len(input.Array()))
	removed := false
	for _, item := range input.Array() {
		if item.Get("type").String() == "reasoning" && item.Get("encrypted_content").Exists() {
			removed = true
			continue
		}
		// 用 RawMessage 保留每个保留项的原始字节，不重排/转义。
		kept = append(kept, json.RawMessage(item.Raw))
	}
	if !removed {
		return body, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "input", encoded)
}

// sanitizeGrokReasoningNullContent 删除 reasoning 项中的 content:null。
// xAI 的 untagged enum 反序列化器会拒收该字段并返回 422。
func sanitizeGrokReasoningNullContent(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}

	for i := len(input.Array()) - 1; i >= 0; i-- {
		item := input.Array()[i]
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}
		content := item.Get("content")
		if !content.Exists() || content.Type != gjson.Null {
			continue
		}
		var err error
		body, err = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.content", i))
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// sanitizeGrokResponsesModelInput 只处理 xAI Responses ModelInput 明确不能反序列化的
// OpenAI/Codex 专有 item。普通 message、function_call、function_call_output 原样保留；
// previous_response_id 也保留，让 Grok 继续使用自己的上一轮响应做上下文锚点。
func sanitizeGrokResponsesModelInput(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}

	items := input.Array()
	kept := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		raw, keep, itemChanged, err := normalizeGrokResponsesModelInputItem(item)
		if err != nil {
			return nil, err
		}
		if itemChanged {
			changed = true
		}
		if !keep {
			changed = true
			continue
		}
		kept = append(kept, raw)
	}
	if !changed {
		return body, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "input", encoded)
}

func normalizeGrokResponsesModelInputItem(item gjson.Result) (json.RawMessage, bool, bool, error) {
	if !item.IsObject() {
		return json.RawMessage(item.Raw), true, false, nil
	}

	itemType := strings.TrimSpace(item.Get("type").String())
	switch itemType {
	case "item_reference", "additional_tools":
		return nil, false, true, nil
	case "web_search_call", "file_search_call", "computer_call", "compaction", "compaction_trigger", "compaction_summary":
		if text := extractGrokInputItemText(item); text != "" {
			return marshalGrokModelInputMessage("assistant", text)
		}
		return nil, false, true, nil
	case "input_text":
		text := strings.TrimSpace(item.Get("text").String())
		if text == "" {
			return nil, false, true, nil
		}
		return marshalGrokModelInputMessage("user", text)
	case "output_text":
		text := strings.TrimSpace(item.Get("text").String())
		if text == "" {
			return nil, false, true, nil
		}
		return marshalGrokModelInputMessage("assistant", text)
	case "text":
		text := strings.TrimSpace(item.Get("text").String())
		if text == "" {
			return nil, false, true, nil
		}
		return marshalGrokModelInputMessage("user", text)
	case "reasoning":
		if text := extractGrokReasoningSummaryText(item); text != "" {
			return marshalGrokModelInputMessage("assistant", text)
		}
		return nil, false, true, nil
	case "local_shell_call", "tool_search_call", "custom_tool_call", "mcp_tool_call", "tool_call":
		return marshalGrokFunctionCallLikeItem(item)
	case "tool_search_output", "custom_tool_call_output", "mcp_tool_call_output":
		return marshalGrokFunctionCallOutputLikeItem(item)
	default:
		return json.RawMessage(item.Raw), true, false, nil
	}
}

func marshalGrokModelInputMessage(role, text string) (json.RawMessage, bool, bool, error) {
	out := map[string]any{
		"type":    "message",
		"role":    role,
		"content": text,
	}
	raw, err := json.Marshal(out)
	return raw, true, true, err
}

func marshalGrokFunctionCallLikeItem(item gjson.Result) (json.RawMessage, bool, bool, error) {
	name := strings.TrimSpace(firstNonEmptyString(
		item.Get("name").String(),
		item.Get("tool_name").String(),
		item.Get("function.name").String(),
	))
	if name == "" {
		name = "tool"
	}
	callID := strings.TrimSpace(firstNonEmptyString(item.Get("call_id").String(), item.Get("id").String()))
	arguments := "{}"
	if item.Get("arguments").Exists() {
		if item.Get("arguments").Type == gjson.String {
			arguments = item.Get("arguments").String()
		} else {
			arguments = item.Get("arguments").Raw
		}
	}
	out := map[string]any{
		"type":      "function_call",
		"name":      name,
		"arguments": arguments,
	}
	if callID != "" {
		out["call_id"] = callID
	}
	raw, err := json.Marshal(out)
	return raw, true, true, err
}

func marshalGrokFunctionCallOutputLikeItem(item gjson.Result) (json.RawMessage, bool, bool, error) {
	callID := strings.TrimSpace(firstNonEmptyString(item.Get("call_id").String(), item.Get("id").String()))
	output := ""
	if item.Get("output").Exists() {
		if item.Get("output").Type == gjson.String {
			output = item.Get("output").String()
		} else {
			output = item.Get("output").Raw
		}
	}
	if callID == "" {
		if strings.TrimSpace(output) == "" {
			return nil, false, true, nil
		}
		return marshalGrokModelInputMessage("user", output)
	}
	out := map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  output,
	}
	raw, err := json.Marshal(out)
	return raw, true, true, err
}

func extractGrokReasoningSummaryText(item gjson.Result) string {
	var parts []string
	summary := item.Get("summary")
	if summary.IsArray() {
		for _, entry := range summary.Array() {
			text := strings.TrimSpace(entry.Get("text").String())
			if text == "" && entry.Type == gjson.String {
				text = strings.TrimSpace(entry.String())
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func extractGrokInputItemText(item gjson.Result) string {
	candidates := []string{
		item.Get("text").String(),
		item.Get("content").String(),
		item.Get("output").String(),
		item.Get("result").String(),
		item.Get("query").String(),
		item.Get("action.query").String(),
	}
	for _, candidate := range candidates {
		if text := strings.TrimSpace(candidate); text != "" {
			return text
		}
	}
	if item.Get("content").IsArray() {
		var parts []string
		for _, part := range item.Get("content").Array() {
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	if item.Get("queries").IsArray() {
		var parts []string
		for _, query := range item.Get("queries").Array() {
			if text := strings.TrimSpace(query.String()); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

var grokResponsesUnsupportedRecursiveFields = map[string]struct{}{
	"external_web_access": {},
}

func sanitizeGrokResponsesUnsupportedFields(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"external_web_access"`)) {
		return body, nil
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if !deleteJSONFields(payload, grokResponsesUnsupportedRecursiveFields) {
		return body, nil
	}
	return json.Marshal(payload)
}

func deleteJSONFields(value any, fields map[string]struct{}) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for field := range fields {
			if _, ok := typed[field]; ok {
				delete(typed, field)
				changed = true
			}
		}
		for _, child := range typed {
			if deleteJSONFields(child, fields) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range typed {
			if deleteJSONFields(child, fields) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

var grokResponsesSupportedToolTypes = map[string]struct{}{
	"code_execution":     {},
	"code_interpreter":   {},
	"collections_search": {},
	"file_search":        {},
	"function":           {},
	"mcp":                {},
	"shell":              {},
	"web_search":         {},
	"x_search":           {},
}

func sanitizeGrokResponsesTools(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		if gjson.GetBytes(body, "tool_choice").Exists() {
			return sjson.DeleteBytes(body, "tool_choice")
		}
		return body, nil
	}

	rawTools := tools.Array()
	filteredTools := make([]json.RawMessage, 0, len(rawTools))
	for _, tool := range rawTools {
		toolType := strings.TrimSpace(tool.Get("type").String())
		if _, ok := grokResponsesSupportedToolTypes[toolType]; ok && isValidGrokResponsesTool(tool) {
			filteredTools = append(filteredTools, json.RawMessage(tool.Raw))
		}
	}

	var err error
	if len(filteredTools) != len(rawTools) {
		if len(filteredTools) == 0 {
			body, err = sjson.DeleteBytes(body, "tools")
		} else {
			var encoded []byte
			encoded, err = json.Marshal(filteredTools)
			if err != nil {
				return nil, err
			}
			body, err = sjson.SetRawBytes(body, "tools", encoded)
		}
		if err != nil {
			return nil, err
		}
	}

	toolChoice := gjson.GetBytes(body, "tool_choice")
	if !toolChoice.Exists() {
		return body, nil
	}
	if shouldDropGrokToolChoice(toolChoice, filteredTools) {
		body, err = sjson.DeleteBytes(body, "tool_choice")
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

func isValidGrokResponsesTool(tool gjson.Result) bool {
	if !tool.IsObject() {
		return false
	}
	toolType := strings.TrimSpace(tool.Get("type").String())
	switch toolType {
	case "shell":
		return tool.Get("environment").Exists()
	default:
		return true
	}
}

func shouldDropGrokToolChoice(toolChoice gjson.Result, tools []json.RawMessage) bool {
	if len(tools) == 0 {
		return true
	}
	if !toolChoice.IsObject() {
		return false
	}
	choiceType := strings.TrimSpace(toolChoice.Get("type").String())
	if choiceType == "" {
		return false
	}
	if _, ok := grokResponsesSupportedToolTypes[choiceType]; !ok {
		return true
	}
	if choiceType == "function" {
		choiceName := strings.TrimSpace(toolChoice.Get("name").String())
		if choiceName == "" {
			choiceName = strings.TrimSpace(toolChoice.Get("function.name").String())
		}
		if choiceName == "" {
			return false
		}
		for _, tool := range tools {
			var item struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(tool, &item); err != nil {
				continue
			}
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = strings.TrimSpace(item.Function.Name)
			}
			if strings.TrimSpace(item.Type) == "function" && name == choiceName {
				return false
			}
		}
		return true
	}
	return false
}

func (s *OpenAIGatewayService) bridgeGrokComposerImageInputs(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) ([]byte, OpenAIUsage, bool, error) {
	if !shouldBridgeGrokComposerImageInputs(body) {
		return body, OpenAIUsage{}, false, nil
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, OpenAIUsage{}, false, fmt.Errorf("parse grok composer image bridge request: %w", err)
	}

	imageURLs := collectGrokComposerImageURLs(reqBody)
	if len(imageURLs) == 0 {
		return body, OpenAIUsage{}, false, nil
	}

	descriptions := make([]string, 0, len(imageURLs))
	var bridgeUsage OpenAIUsage
	for index, imageURL := range imageURLs {
		description, usage, err := s.describeGrokComposerImage(ctx, c, account, token, imageURL, index+1)
		if err != nil {
			return body, bridgeUsage, false, err
		}
		descriptions = append(descriptions, description)
		addOpenAIUsage(&bridgeUsage, usage)
	}

	if !rewriteGrokComposerImagesAsText(reqBody, descriptions) {
		return body, bridgeUsage, false, nil
	}
	bridgedBody, err := marshalOpenAIUpstreamJSON(reqBody)
	if err != nil {
		return body, bridgeUsage, false, fmt.Errorf("serialize grok composer image bridge request: %w", err)
	}
	return bridgedBody, bridgeUsage, true, nil
}

func shouldBridgeGrokComposerImageInputs(body []byte) bool {
	if len(body) == 0 || !isGrokComposerModel(gjson.GetBytes(body, "model").String()) {
		return false
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return false
	}
	return openAIJSONValueMayContainImageInput(messages)
}

func isGrokComposerModel(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	if model == "" {
		return false
	}
	if strings.Contains(model, "/") {
		parts := strings.Split(model, "/")
		model = strings.TrimSpace(parts[len(parts)-1])
	}
	return strings.Contains(model, "composer")
}

func collectGrokComposerImageURLs(reqBody map[string]any) []string {
	messages, ok := reqBody["messages"].([]any)
	if !ok {
		return nil
	}

	var imageURLs []string
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msgMap["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			if imageURL := grokComposerImageURLFromPart(part); imageURL != "" {
				imageURLs = append(imageURLs, imageURL)
			}
		}
	}
	return imageURLs
}

func grokComposerImageURLFromPart(part any) string {
	partMap, ok := part.(map[string]any)
	if !ok {
		return ""
	}
	if strings.TrimSpace(strings.ToLower(fmt.Sprint(partMap["type"]))) != "image_url" {
		return ""
	}
	switch imageURL := partMap["image_url"].(type) {
	case string:
		return normalizeGrokComposerImageURL(imageURL)
	case map[string]any:
		raw, _ := imageURL["url"].(string)
		return normalizeGrokComposerImageURL(raw)
	default:
		return ""
	}
}

func normalizeGrokComposerImageURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || isEmptyBase64DataURI(trimmed) {
		return ""
	}
	return trimmed
}

func (s *OpenAIGatewayService) describeGrokComposerImage(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	token string,
	imageURL string,
	index int,
) (string, OpenAIUsage, error) {
	body, err := buildGrokComposerImageDescriptionBody(imageURL, index)
	if err != nil {
		return "", OpenAIUsage{}, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	upstreamReq, err := buildGrokResponsesRequest(upstreamCtx, c, account, body, token)
	releaseUpstreamCtx()
	if err != nil {
		return "", OpenAIUsage{}, fmt.Errorf("build grok composer image bridge request: %w", err)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return "", OpenAIUsage{}, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		s.updateGrokUsageSnapshot(ctx, account.ID, xai.ParseQuotaHeaders(resp.Header, resp.StatusCode))
		upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
		if upstreamMsg == "" {
			upstreamMsg = fmt.Sprintf("xAI image bridge upstream returned status %d", resp.StatusCode)
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
			Kind:               "failover",
			Message:            upstreamMsg,
		})
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			return "", OpenAIUsage{}, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return "", OpenAIUsage{}, fmt.Errorf("grok composer image bridge upstream error: %s", upstreamMsg)
	}

	s.updateGrokUsageSnapshot(ctx, account.ID, xai.ParseQuotaHeaders(resp.Header, resp.StatusCode))
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, nil)
	if err != nil {
		return "", OpenAIUsage{}, fmt.Errorf("read grok composer image bridge response: %w", err)
	}

	var parsed apicompat.ResponsesResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", OpenAIUsage{}, fmt.Errorf("parse grok composer image bridge response: %w", err)
	}
	description := strings.TrimSpace(grokResponsesOutputText(&parsed))
	if description == "" {
		return "", copyOpenAIUsageFromResponsesUsage(parsed.Usage), fmt.Errorf("grok composer image bridge returned empty description")
	}
	return description, copyOpenAIUsageFromResponsesUsage(parsed.Usage), nil
}

func buildGrokComposerImageDescriptionBody(imageURL string, index int) ([]byte, error) {
	prompt := fmt.Sprintf("Describe image %d in concise, factual text for a downstream coding/composer model. Include visible text, UI elements, diagrams, errors, and spatial relationships. Do not mention that you are an image analysis bridge.", index)
	req := map[string]any{
		"model":             grokComposerImageBridgeVisionModel,
		"stream":            false,
		"store":             false,
		"max_output_tokens": grokComposerImageBridgeMaxOutputTokens,
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": prompt},
					map[string]any{"type": "input_image", "image_url": imageURL},
				},
			},
		},
	}
	return marshalOpenAIUpstreamJSON(req)
}

func grokResponsesOutputText(resp *apicompat.ResponsesResponse) string {
	if resp == nil {
		return ""
	}
	var parts []string
	for _, output := range resp.Output {
		for _, content := range output.Content {
			if content.Type == "output_text" || content.Type == "text" || content.Type == "input_text" {
				if text := strings.TrimSpace(content.Text); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func rewriteGrokComposerImagesAsText(reqBody map[string]any, descriptions []string) bool {
	messages, ok := reqBody["messages"].([]any)
	if !ok {
		return false
	}

	imageIndex := 0
	changed := false
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msgMap["content"].([]any)
		if !ok {
			continue
		}
		var textParts []string
		messageChanged := false
		for _, part := range parts {
			if imageURL := grokComposerImageURLFromPart(part); imageURL != "" {
				if imageIndex < len(descriptions) {
					textParts = append(textParts, fmt.Sprintf("Image %d description: %s", imageIndex+1, strings.TrimSpace(descriptions[imageIndex])))
				}
				imageIndex++
				messageChanged = true
				continue
			}
			if text := grokComposerTextFromPart(part); text != "" {
				textParts = append(textParts, text)
			}
		}
		if messageChanged {
			msgMap["content"] = strings.Join(textParts, "\n\n")
			changed = true
		}
	}
	return changed
}

func grokComposerTextFromPart(part any) string {
	partMap, ok := part.(map[string]any)
	if !ok {
		return ""
	}
	partType := strings.TrimSpace(strings.ToLower(fmt.Sprint(partMap["type"])))
	switch partType {
	case "text", "input_text":
		text, _ := partMap["text"].(string)
		return strings.TrimSpace(text)
	default:
		return ""
	}
}

func addOpenAIUsage(dst *OpenAIUsage, usage OpenAIUsage) {
	if dst == nil {
		return
	}
	dst.InputTokens += usage.InputTokens
	dst.ImageInputTokens += usage.ImageInputTokens
	dst.OutputTokens += usage.OutputTokens
	dst.CacheCreationInputTokens += usage.CacheCreationInputTokens
	dst.CacheReadInputTokens += usage.CacheReadInputTokens
	dst.ImageOutputTokens += usage.ImageOutputTokens
}

func buildGrokResponsesRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string) (*http.Request, error) {
	targetURL, err := xai.BuildResponsesURL(account.GetGrokBaseURL())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	xai.ApplyGrokBuildHeaders(req.Header)
	if c != nil {
		if v := c.GetHeader("OpenAI-Beta"); strings.TrimSpace(v) != "" {
			req.Header.Set("OpenAI-Beta", v)
		}
	}
	return req, nil
}

func (s *OpenAIGatewayService) updateGrokUsageSnapshot(ctx context.Context, accountID int64, snapshot *xai.QuotaSnapshot) {
	if s == nil || s.accountRepo == nil || accountID <= 0 || snapshot == nil {
		return
	}
	if s.codexSnapshotThrottle != nil && !s.codexSnapshotThrottle.Allow(accountID, time.Now()) {
		return
	}
	_ = s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		grokQuotaSnapshotExtraKey: snapshot,
	})
}

func (s *OpenAIGatewayService) handleGrokAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte) {
	if s == nil || account == nil {
		return
	}
	switch statusCode {
	case http.StatusUnauthorized:
		s.updateGrokUsageSnapshot(ctx, account.ID, xai.ObserveQuotaHeaders(headers, statusCode, "upstream_error"))
		// 无 refresh_token 的 OAuth 账号无法在冷却期内自愈(后台刷新也会跳过)，直接永久 error，
		// 避免冷却结束后再被选中产生无意义的 502(对齐 OpenAI 的 401 无 refresh_token 分支)。
		if strings.TrimSpace(account.GetCredential("refresh_token")) == "" && s.rateLimitService != nil {
			s.rateLimitService.DisableAccountOnAuthError(ctx, account, "Grok 401: refresh_token missing, cannot recover")
			return
		}
		s.tempUnscheduleGrok(ctx, account, 10*time.Minute, "grok oauth token unauthorized")
	case http.StatusForbidden:
		snapshot := xai.ObserveQuotaHeaders(headers, statusCode, "upstream_error")
		// 防御：free-usage-exhausted 通常是 429，但也可能以 403 返回；那是可恢复限流(滚动窗口)，
		// 绝不能升级为永久 error，否则会误杀只是免费额度耗尽的账号。
		if freeInfo := xai.ParseFreeUsageExhaustedBody(responseBody); freeInfo != nil && freeInfo.Exhausted {
			cooldown := freeInfo.Cooldown
			if cooldown <= 0 {
				cooldown = xai.DefaultFreeUsageCooldown
			}
			freeInfo.ApplyToSnapshot(snapshot, time.Now())
			s.updateGrokUsageSnapshot(ctx, account.ID, snapshot)
			s.tempUnscheduleGrok(ctx, account, cooldown, "grok free usage exhausted (rolling window)")
			return
		}
		s.updateGrokUsageSnapshot(ctx, account.ID, snapshot)
		// 真·permission-denied：对齐 OpenAI，用连续 403 计数升级——连续达阈值即永久 error 移出调度，
		// 避免 xAI 回收权限后的死号在"30 分钟冷却 → 放回 → 又 403"里无限 churn。未达阈值则临时冷却。
		if s.rateLimitService != nil && s.rateLimitService.EscalateGrokForbiddenOn403(ctx, account, sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(responseBody)), responseBody) {
			return
		}
		// xAI chat endpoint permission-denied is an entitlement state, not a short RPM throttle.
		// Keep it out of rotation for a full day, and sticky-hold until a proven success.
		// Timer expiry alone must NOT silently put the account back into the pool.
		s.tempUnscheduleGrok(ctx, account, 24*time.Hour, grokHoldUntilSuccessReason)
		MarkGrokHoldUntilSuccess(ctx, s.accountRepo, account)
	case http.StatusTooManyRequests:
		// Free Build exhaustion is a rolling window (body: subscription:free-usage-exhausted).
		// Plain RPM 429 still uses Retry-After / short cooldown.
		cooldown, reason, freeInfo, snapshot := xai.ResolveGrokCooldown(statusCode, headers, responseBody)
		if snapshot != nil {
			s.updateGrokUsageSnapshot(ctx, account.ID, snapshot)
		}
		if freeInfo != nil && freeInfo.Exhausted {
			// Keep extra free-usage metadata for quota UI / ops.
			if s.accountRepo != nil {
				extra := map[string]any{
					"grok_free_usage_exhausted":  true,
					"grok_free_usage_error_code": freeInfo.ErrorCode,
					"grok_free_usage_window":     freeInfo.Window,
					"grok_free_usage_model":      freeInfo.Model,
				}
				if freeInfo.ActualTokens != nil {
					extra["grok_free_usage_actual_tokens"] = *freeInfo.ActualTokens
				}
				if freeInfo.LimitTokens != nil {
					extra["grok_free_usage_limit_tokens"] = *freeInfo.LimitTokens
				}
				if cooldown > 0 {
					extra["grok_free_usage_cooldown_until"] = time.Now().UTC().Add(cooldown).Format(time.RFC3339)
				}
				stateCtx, cancel := openAIAccountStateContext(ctx)
				_ = s.accountRepo.UpdateExtra(stateCtx, account.ID, extra)
				cancel()
			}
		}
		if cooldown <= 0 {
			cooldown = 2 * time.Minute
		}
		if strings.TrimSpace(reason) == "" {
			reason = "grok rate limited"
		}
		s.tempUnscheduleGrok(ctx, account, cooldown, reason)
	default:
		// Some free-usage exhaustion responses may arrive as 403; still parse body.
		if freeInfo := xai.ParseFreeUsageExhaustedBody(responseBody); freeInfo != nil && freeInfo.Exhausted {
			cooldown := freeInfo.Cooldown
			if cooldown <= 0 {
				cooldown = xai.DefaultFreeUsageCooldown
			}
			snapshot := xai.ParseQuotaHeaders(headers, statusCode)
			if snapshot == nil {
				snapshot = &xai.QuotaSnapshot{StatusCode: statusCode, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
			}
			freeInfo.ApplyToSnapshot(snapshot, time.Now())
			s.updateGrokUsageSnapshot(ctx, account.ID, snapshot)
			s.tempUnscheduleGrok(ctx, account, cooldown, "grok free usage exhausted (rolling window)")
			return
		}
		if statusCode >= 500 {
			s.tempUnscheduleGrok(ctx, account, 2*time.Minute, "grok upstream temporary error")
		}
	}
}

// tempUnscheduleGrok cools a Grok account after upstream errors.
//
// Rate-limit style failures (429 / free-usage-exhausted) write ONLY rate_limit_reset_at so
// the admin UI badge shows "限流" with an auto-resume time AND the "限流中" filter matches
// (same shape as OpenAI 429). Auth / entitlement / transport failures write
// temp_unschedulable_until instead (temporary unschedulable). The two lanes stay disjoint so
// the status badge and the admin status filter never disagree.
func (s *OpenAIGatewayService) tempUnscheduleGrok(ctx context.Context, account *Account, cooldown time.Duration, reason string) {
	if s == nil || account == nil {
		return
	}
	until := time.Now().Add(cooldown)
	// Do not shorten an already longer pause from either lane.
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(until) {
		until = *account.TempUnschedulableUntil
	}
	if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(until) {
		until = *account.RateLimitResetAt
	}
	s.BlockAccountScheduling(account, until, reason)
	if s.accountRepo == nil {
		return
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()

	if isGrokRateLimitCooldownReason(reason) {
		// 限流(429 / free-usage-exhausted)只写 rate_limit_reset_at，与 OpenAI 429 完全一致。
		// IsSchedulable 已按 rate_limit_reset_at 排除调度，无需再写 temp_unschedulable_until。
		// 此前额外写 temp_unschedulable_until 会导致：admin 的"限流中"筛选(要求 temp_unschedulable
		// 为空)查不到这些账号、只落到"临时不可调度"筛选，却又与状态徽标(按 rate_limit_reset_at
		// 显示"限流中")自相矛盾。free-usage 明细已存于 extra(grok_free_usage_*)，无需占用
		// temp_unschedulable_reason。
		_ = s.accountRepo.SetRateLimited(stateCtx, account.ID, until)
		account.RateLimitResetAt = &until
		now := time.Now()
		account.RateLimitedAt = &now
		return
	}

	_ = s.accountRepo.SetTempUnschedulable(stateCtx, account.ID, until, reason)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = reason
}

func isGrokRateLimitCooldownReason(reason string) bool {
	r := strings.ToLower(strings.TrimSpace(reason))
	if r == "" {
		return false
	}
	return strings.Contains(r, "rate limited") ||
		strings.Contains(r, "free usage exhausted") ||
		strings.Contains(r, "free-usage-exhausted")
}
