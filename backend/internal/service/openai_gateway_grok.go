package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokComposerImageBridgeVisionModel     = "grok-build-0.1"
	grokComposerImageBridgeMaxOutputTokens = 512
	grokUpstreamUserAgent                  = "sub2api-grok/1.0"
	grokCLIVersion                         = "0.2.93"
	grokDefaultResponsesModel              = "grok-4.5"
	grokRateLimitFallbackCooldown          = 2 * time.Minute
	grokRateLimitRepeatCooldown            = 10 * time.Minute
	grokRateLimitSustainedCooldown         = 30 * time.Minute
	grokRateLimitMaxAdaptiveCooldown       = time.Hour
	grokRateLimitBackoffQuietPeriod        = time.Hour
)

func (s *OpenAIGatewayService) forwardGrokResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey {
		return nil, fmt.Errorf("grok account type %s is not supported by Responses forwarding", account.Type)
	}

	upstreamModel := resolveOpenAIForwardModelForContext(ctx, account, originalModel, "")
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = grokDefaultResponsesModel
	}
	if isGrokImageGenerationModel(upstreamModel) {
		return nil, fmt.Errorf("model %s is an image model and is not available on the Responses endpoint; use /v1/images/generations instead", upstreamModel)
	}
	patchedBody, clientToolMapping, err := patchGrokResponsesBodyWithClientTools(body, upstreamModel)
	if err != nil {
		setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"type": "invalid_request_error", "message": err.Error(), "param": "tools",
		}})
		return nil, err
	}
	setGrokResponsesClientToolMapping(c, clientToolMapping)
	// OpenAI /responses/compact is not a native xAI endpoint. Convert it into a
	// normal Grok Responses turn that asks for a structured summary, then map the
	// reply back to an OpenAI compaction item on the way out.
	if isOpenAIResponsesCompactPath(c) {
		patchedBody, err = buildGrokCompactRequestBody(patchedBody)
		if err != nil {
			return nil, err
		}
	}
	// Derive the identity from the request xAI will actually see. This makes
	// Codex Responses Lite additional_tools part of the stable tool prefix.
	cacheIdentity := resolveGrokCacheIdentity(c, patchedBody, "", upstreamModel)
	mixedCacheIntentBody := append([]byte(nil), patchedBody...)
	patchedBody, err = applyGrokResponsesCacheIdentity(patchedBody, body, cacheIdentity, account.IsGrokOAuth())
	if err != nil {
		return nil, fmt.Errorf("apply grok prompt cache identity: %w", err)
	}
	// Free OAuth + client function tools: reuse Messages mixed-tools cache route
	// (append web_search/x_search so xAI does not force non-cacheable build-free).
	// Request-scoped opt-in/out via applyGrokFreeRequestToolCacheRoute; intent comes from
	// the already-patched body so Codex additional_tools promotions stay visible.
	patchedBody, err = applyGrokFreeRequestToolCacheRoute(c, patchedBody, mixedCacheIntentBody, account, cacheIdentity)
	if err != nil {
		return nil, fmt.Errorf("apply grok Free function-tool cache route: %w", err)
	}

	// grok Build(cli-chat-proxy)自带后端 web_search，收到图片会把看图题变成多轮联网检索；
	// 含 input_image 时注入 developer 指令让它直接看图作答（用户显式要搜时仍放行）。
	// 仅针对 Build：api.x.ai 的 API Key 账号不受影响。
	if account.IsGrokOAuth() && xai.IsCLIChatProxyBaseURL(account.GetGrokBaseURL()) {
		patchedBody = appendGrokImageSearchGuard(patchedBody)
	}

	token, _, err := s.getRequestCredential(ctx, c, account)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	tryBaseURLs := []string{account.GetGrokBaseURL()}
	if account.IsGrokOAuth() && xai.IsCLIChatProxyBaseURL(tryBaseURLs[0]) {
		tryBaseURLs = append(tryBaseURLs, xai.DefaultBaseURL)
	}

	var resp *http.Response
	var lastErrBody []byte
	// GPT/OpenAI 会话切到 Grok 后，Codex 可能仍携带外来 previous_response_id。
	// 同账号允许一次 drop-and-retry，并对孤儿 function_call_output 做可重建修复。
	prevResponseRecovered := false
	// xAI 可能拒绝来自其他账号/缓存身份的 encrypted reasoning；同路由同凭证允许剥掉后重试一次。
	encryptedContentRetried := false
	for i, baseURL := range tryBaseURLs {
		for {
			upstreamReq, reqErr := buildGrokResponsesRequestWithBase(upstreamCtx, c, account, patchedBody, token, cacheIdentity, s.cfg, baseURL)
			if reqErr != nil {
				return nil, reqErr
			}
			upstreamStart := time.Now()
			currentResp, doErr := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
			SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
			if doErr != nil {
				return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, doErr, false)
			}
			if currentResp.StatusCode < 400 {
				resp = currentResp
				break
			}

			respBody := s.readUpstreamErrorBody(currentResp)
			_ = currentResp.Body.Close()
			currentResp.Body = io.NopCloser(bytes.NewReader(respBody))
			lastErrBody = respBody
			upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
			if upstreamMsg == "" {
				upstreamMsg = fmt.Sprintf("xAI upstream returned status %d", currentResp.StatusCode)
			}
			requestID := firstNonEmpty(currentResp.Header.Get("x-request-id"), currentResp.Header.Get("xai-request-id"))

			// Cross-model / stale previous_response_id: drop anchor and repair orphan tool outputs once.
			if !prevResponseRecovered &&
				gjson.GetBytes(patchedBody, "previous_response_id").Exists() &&
				(isOpenAICompatPreviousResponseNotFound(currentResp.StatusCode, upstreamMsg, respBody) ||
					isOpenAICompatPreviousResponseUnsupported(currentResp.StatusCode, upstreamMsg, respBody) ||
					isGrokOrphanFunctionCallOutputError(currentResp.StatusCode, upstreamMsg, respBody)) {
				repairedBody, repaired, repairErr := dropGrokPreviousResponseIDAndRepairToolOutputs(patchedBody)
				if repairErr == nil && repaired {
					prevResponseRecovered = true
					patchedBody = repairedBody
					slog.Info("grok responses: previous_response_id unavailable, retrying without continuation",
						"account_id", account.ID,
						"upstream_model", upstreamModel,
						"upstream_status", currentResp.StatusCode,
						"upstream_request_id", requestID,
					)
					continue
				}
			}

			// Invalid encrypted_content from another account/cache identity: strip and retry once.
			if !encryptedContentRetried && isGrokInvalidEncryptedContentResponse(currentResp.StatusCode, respBody) {
				retryBody, changed, trimErr := trimGrokInvalidEncryptedContentRetryBody(patchedBody)
				if trimErr != nil {
					return nil, fmt.Errorf("prepare Grok invalid encrypted_content retry: %w", trimErr)
				}
				if changed {
					encryptedContentRetried = true
					patchedBody = retryBody
					slog.Info("grok_invalid_encrypted_content_retry",
						"account_id", account.ID,
						"cache_identity_present", cacheIdentity != "",
						"upstream_request_id", requestID,
					)
					continue
				}
			}

			// Same-account CLI -> public API fallback on 403 only.
			if i == 0 && canFallbackGrokCLIProxyToPublicAPI(account, baseURL, currentResp.StatusCode) && len(tryBaseURLs) > 1 {
				appendGrokCLIProxyFallbackOpsEvent(c, account, currentResp.StatusCode, requestID, "cli-chat-proxy 403, retrying same account on api.x.ai")
				break
			}

			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: currentResp.StatusCode,
				UpstreamRequestID:  requestID,
				Kind:               "failover",
				Message:            upstreamMsg,
			})
			s.handleGrokAccountUpstreamError(ctx, account, currentResp.StatusCode, currentResp.Header, respBody)
			if s.shouldFailoverUpstreamError(currentResp.StatusCode) {
				return nil, &UpstreamFailoverError{
					StatusCode:             currentResp.StatusCode,
					ResponseBody:           respBody,
					ResponseHeaders:        currentResp.Header.Clone(),
					RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(currentResp.StatusCode),
				}
			}
			return s.handleErrorResponse(ctx, currentResp, c, account, patchedBody, upstreamModel)
		}
		if resp != nil {
			break
		}
	}
	if resp == nil {
		return nil, fmt.Errorf("grok upstream returned no successful response")
	}
	defer func() { _ = resp.Body.Close() }()
	_ = lastErrBody

	s.updateGrokUsageFromResponse(ctx, account, resp.Header, resp.StatusCode)

	var usage *OpenAIUsage
	var firstTokenMs *int
	responseID := ""
	if reqStream {
		if hasGrokResponsesClientToolMapping(clientToolMapping) {
			maxLineSize := defaultMaxLineSize
			if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
				maxLineSize = s.cfg.Gateway.MaxLineSize
			}
			resp.Body = newGrokResponsesClientToolStreamBody(resp.Body, clientToolMapping, maxLineSize)
		}
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

func isGrokInvalidEncryptedContentResponse(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}

	// xAI has used both flat and nested error envelopes:
	//   {"code":"invalid-argument","error":"Could not decrypt the provided encrypted_content."}
	//   {"error":{"message":"Could not decrypt the provided encrypted_content."}}
	code := strings.TrimSpace(gjson.GetBytes(body, "code").String())
	message := ""
	errNode := gjson.GetBytes(body, "error")
	switch {
	case errNode.Type == gjson.String:
		message = errNode.String()
	case errNode.IsObject():
		message = firstNonEmpty(errNode.Get("message").String(), errNode.Get("error").String())
		if code == "" {
			code = strings.TrimSpace(errNode.Get("code").String())
		}
	default:
		message = gjson.GetBytes(body, "message").String()
	}
	normalizedMessage := strings.ToLower(strings.TrimSpace(message))
	if normalizedMessage == "" {
		return false
	}

	if strings.EqualFold(code, "invalid_encrypted_content") {
		return true
	}
	// Keep the official xAI flat-code gate so unrelated 400s are not retried.
	if !strings.EqualFold(code, "invalid-argument") && code != "" {
		return false
	}
	// Nested OpenAI-style envelopes may omit top-level code; require decrypt text.
	if code == "" && !strings.Contains(normalizedMessage, "decrypt") {
		return false
	}
	return strings.Contains(normalizedMessage, "encrypted_content") &&
		(strings.Contains(normalizedMessage, "decrypt") ||
			strings.Contains(normalizedMessage, "unmodified"))
}

// requestHasGrokEncryptedReasoning reports whether the outbound Responses body
// still carries reasoning.encrypted_content that can be stripped for retry.
func requestHasGrokEncryptedReasoning(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return false
	}
	items := input.Array()
	if input.IsObject() {
		items = []gjson.Result{input}
	}
	for _, item := range items {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}
		enc := item.Get("encrypted_content")
		if enc.Exists() && enc.Type != gjson.Null && strings.TrimSpace(enc.String()) != "" {
			return true
		}
	}
	return false
}

type grokEncryptedContentStripRetriedKey struct{}

func markGrokEncryptedContentStripRetried(ctx context.Context) context.Context {
	return context.WithValue(ctx, grokEncryptedContentStripRetriedKey{}, true)
}

func grokEncryptedContentStripRetried(ctx context.Context) bool {
	v, _ := ctx.Value(grokEncryptedContentStripRetriedKey{}).(bool)
	return v
}

// stripAnthropicThinkingSignatures removes thinking.signature from Claude
// history so a different Grok OAuth account can accept multi-turn tool
// continuations after decrypt failures. Returns ok=false when nothing changed.
func stripAnthropicThinkingSignatures(body []byte) ([]byte, bool) {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"signature"`)) {
		return body, false
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return body, false
	}
	messages, ok := req["messages"].([]any)
	if !ok || len(messages) == 0 {
		return body, false
	}
	changed := false
	for _, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := block["type"].(string); typ != "thinking" {
				continue
			}
			if _, has := block["signature"]; has {
				delete(block, "signature")
				changed = true
			}
		}
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return out, true
}

func trimGrokInvalidEncryptedContentRetryBody(body []byte) ([]byte, bool, error) {
	input := gjson.GetBytes(body, "input")
	items := input.Array()
	if input.IsObject() {
		items = []gjson.Result{input}
	}

	hasEncryptedReasoning := false
	for _, item := range items {
		if strings.TrimSpace(item.Get("type").String()) == "reasoning" && item.Get("encrypted_content").Exists() {
			hasEncryptedReasoning = true
			break
		}
	}
	if !hasEncryptedReasoning {
		return body, false, nil
	}

	var requestBody map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&requestBody); err != nil {
		return nil, false, err
	}
	if !trimOpenAIEncryptedReasoningItems(requestBody) {
		return body, false, nil
	}

	retryBody, err := marshalOpenAIUpstreamJSON(requestBody)
	if err != nil {
		return nil, false, err
	}
	return retryBody, true, nil
}

func patchGrokResponsesBody(body []byte, upstreamModel string) ([]byte, error) {
	return patchGrokResponsesBodyBase(body, upstreamModel)
}

func patchGrokResponsesBodyWithClientTools(body []byte, upstreamModel string) ([]byte, apicompat.ResponsesClientToolMapping, error) {
	if !json.Valid(body) {
		return nil, apicompat.ResponsesClientToolMapping{}, fmt.Errorf("invalid json request body")
	}
	promoted, err := sanitizeGrokResponsesInput(body)
	if err != nil {
		return nil, apicompat.ResponsesClientToolMapping{}, err
	}
	adapted, mapping, err := adaptGrokResponsesClientTools(promoted)
	if err != nil {
		return nil, apicompat.ResponsesClientToolMapping{}, err
	}
	patched, err := patchGrokResponsesBodyBase(adapted, upstreamModel)
	if err != nil {
		return nil, apicompat.ResponsesClientToolMapping{}, err
	}
	return patched, mapping, nil
}

func patchGrokResponsesBodyBase(body []byte, upstreamModel string) ([]byte, error) {
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
	out, err = sanitizeGrokResponsesReasoningSummary(out)
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
	out, err = convertOpenAICompactInputsForGrok(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesInput(out)
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
	out, err = sanitizeGrokResponsesTools(out)
	if err != nil {
		return nil, err
	}
	// Agent 工具场景：把 auto 收敛为 required，避免 Grok 只输出计划而不调用客户端工具。
	out, err = requireGrokResponsesFunctionToolChoice(out)
	if err != nil {
		return nil, err
	}
	// 最终清洗 input：把 Codex/OpenAI 专有 item 归一成 xAI 可反序列化的 ModelInput 变体。
	out, err = sanitizeGrokResponsesModelInput(out)
	if err != nil {
		return nil, err
	}
	// 跨模型/自包含续链：input 已能独立重建时剥离外来 previous_response_id，
	// 避免 GPT 会话的 resp_* 锚点被带到 xAI。
	out, err = sanitizeGrokResponsesPreviousResponseID(out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sanitizeGrokResponsesReasoningSummary 收敛 Grok 请求侧 summary。
// Codex 常带 summary=auto，xAI 可能抬成 detailed 并回传长明文；请求侧尽量保持 auto。
func sanitizeGrokResponsesReasoningSummary(body []byte) ([]byte, error) {
	if !gjson.GetBytes(body, "reasoning").Exists() {
		return body, nil
	}
	summary := strings.TrimSpace(gjson.GetBytes(body, "reasoning.summary").String())
	if summary == "" || strings.EqualFold(summary, "auto") {
		return body, nil
	}
	// detailed/concise 等统一压到 auto，避免上游按 detailed 生成超长可见摘要。
	return sjson.SetBytes(body, "reasoning.summary", "auto")
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

// sanitizeGrokResponsesModelInput 只处理 xAI Responses ModelInput 明确不能反序列化的
// OpenAI/Codex 专有 item。普通 message、function_call_output 尽量保留；function_call 的
// arguments 保持为“JSON object 的字符串”（OpenAI 形态）。xAI 既不接受字段级 object，
// 也不接受解析后不是 object 的 arguments 字符串。
// previous_response_id 由 sanitizeGrokResponsesPreviousResponseID 决定是否保留：
// input 自包含时可剥离（兼容 GPT→Grok），仅依赖 previous 的工具续链则保留。
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

// sanitizeGrokResponsesPreviousResponseID 在 Grok HTTP 路径上处理跨模型续链锚点。
// Codex 从 GPT 切到 Grok 后常仍携带 OpenAI 的 previous_response_id；xAI 无法解析外来
// resp_*。当 input 已自包含（无工具输出，或每个 function_call_output 都能在 input 内
// 找到同 call_id 的 function_call 上下文）时主动剥离 previous_response_id。
// 若工具输出仍依赖 previous（孤儿 output / 仅靠 item_reference），则保留，留给上游
// 失败后的 drop-and-retry 路径再修复。
func sanitizeGrokResponsesPreviousResponseID(body []byte) ([]byte, error) {
	prev := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if prev == "" {
		// 空字符串 / null 也清掉，避免 xAI 因空锚点 400。
		if gjson.GetBytes(body, "previous_response_id").Exists() {
			return sjson.DeleteBytes(body, "previous_response_id")
		}
		return body, nil
	}

	coverage := AnalyzeToolCallOutputContextCoverageBytes(body)
	if coverage.HasFunctionCallOutput && !coverage.ContextCoversAllCallIDs {
		// 仍依赖 previous 解析工具 call；同平台 Grok 续链需要保留。
		return body, nil
	}
	return sjson.DeleteBytes(body, "previous_response_id")
}

// dropGrokPreviousResponseIDAndRepairToolOutputs 用于 previous_response 不可用时的同账号重试：
//  1. 去掉 previous_response_id
//  2. 把缺少对应 function_call 上下文的 function_call_output 转成 message，避免
//     “No tool call found for function call output”。
func dropGrokPreviousResponseIDAndRepairToolOutputs(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	out := body
	changed := false
	if gjson.GetBytes(out, "previous_response_id").Exists() {
		next, err := sjson.DeleteBytes(out, "previous_response_id")
		if err != nil {
			return body, false, err
		}
		out = next
		changed = true
	}
	repaired, repairChanged, err := repairGrokOrphanFunctionCallOutputs(out)
	if err != nil {
		return body, false, err
	}
	if repairChanged {
		out = repaired
		changed = true
	}
	return out, changed, nil
}

func isGrokOrphanFunctionCallOutputError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusUnprocessableEntity {
		return false
	}
	check := func(s string) bool {
		lower := strings.ToLower(strings.TrimSpace(s))
		if lower == "" {
			return false
		}
		return (strings.Contains(lower, "no tool call found") && strings.Contains(lower, "function call output")) ||
			(strings.Contains(lower, "tool call") && strings.Contains(lower, "function_call_output") && strings.Contains(lower, "not found")) ||
			strings.Contains(lower, "no tool call found for function call output")
	}
	if check(upstreamMsg) || check(string(upstreamBody)) {
		return true
	}
	return check(gjson.GetBytes(upstreamBody, "error.code").String()) ||
		check(gjson.GetBytes(upstreamBody, "error.message").String())
}

// repairGrokOrphanFunctionCallOutputs 将缺少同 call_id 的 function_call 上下文的
// function_call_output 折叠为 user message，保证剥离 previous / item_reference 后
// input 仍可被 xAI 接受。已有匹配 function_call 的输出保持不变。
func repairGrokOrphanFunctionCallOutputs(body []byte) ([]byte, bool, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}

	contextIDs := map[string]struct{}{}
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		if !isCodexToolCallContextItemType(itemType) && itemType != "function_call" {
			return true
		}
		callID := strings.TrimSpace(firstNonEmptyString(item.Get("call_id").String(), item.Get("id").String()))
		if callID != "" {
			contextIDs[callID] = struct{}{}
		}
		return true
	})

	items := input.Array()
	kept := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		if !item.IsObject() {
			kept = append(kept, json.RawMessage(item.Raw))
			continue
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		if !isCodexToolCallOutputItemType(itemType) && itemType != "function_call_output" {
			kept = append(kept, json.RawMessage(item.Raw))
			continue
		}
		callID := strings.TrimSpace(firstNonEmptyString(item.Get("call_id").String(), item.Get("id").String()))
		if callID != "" {
			if _, ok := contextIDs[callID]; ok {
				kept = append(kept, json.RawMessage(item.Raw))
				continue
			}
		}
		// 孤儿工具输出：折叠成可读 message，避免断链后 400。
		output := ""
		if item.Get("output").Exists() {
			if item.Get("output").Type == gjson.String {
				output = item.Get("output").String()
			} else {
				output = item.Get("output").Raw
			}
		}
		text := strings.TrimSpace(output)
		if text == "" {
			changed = true
			continue
		}
		if callID != "" {
			text = "Tool result (" + callID + "):\n" + text
		} else {
			text = "Tool result:\n" + text
		}
		raw, keep, _, err := marshalGrokModelInputMessage("user", text)
		if err != nil {
			return body, false, err
		}
		if !keep {
			changed = true
			continue
		}
		kept = append(kept, raw)
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return body, false, err
	}
	out, err := sjson.SetRawBytes(body, "input", encoded)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
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
	case "function_call":
		// OpenAI/Codex 历史里 arguments 常是 JSON 字符串；xAI 要求 object。
		return normalizeGrokFunctionCallItem(item)
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
	arguments, _ := normalizeGrokToolArgumentsJSONString(item)
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

// normalizeGrokFunctionCallItem 保留既有 function_call，只把 arguments 收敛成
// “可解析为 JSON object 的字符串”。字段本身必须是 string：写成 object 会 422 ModelInput；
// 字符串内容若不是 object，会 400 expected JSON object for tool arguments。
func normalizeGrokFunctionCallItem(item gjson.Result) (json.RawMessage, bool, bool, error) {
	if !item.IsObject() {
		return json.RawMessage(item.Raw), true, false, nil
	}
	arguments, changed := normalizeGrokToolArgumentsJSONString(item)
	if !changed {
		return json.RawMessage(item.Raw), true, false, nil
	}
	out, err := sjson.Set(item.Raw, "arguments", arguments)
	if err != nil {
		return nil, false, false, err
	}
	return json.RawMessage(out), true, true, nil
}

// normalizeGrokToolArgumentsJSONString 把 function_call/custom_tool_call 的 arguments/input
// 归一成 JSON object 的字符串表示（例如 `{"cmd":"pwd"}`）。
// - 已是合法 object 字符串：尽量原样保留
// - 字段级 object：序列化成字符串
// - 空/非 object/纯文本：包进 {"input":...} 或 "{}"
func normalizeGrokToolArgumentsJSONString(item gjson.Result) (string, bool) {
	source := item.Get("arguments")
	if !source.Exists() {
		source = item.Get("input")
	}
	if !source.Exists() {
		return "{}", true
	}
	if source.IsObject() {
		// 字段级 object 必须改成 string，否则 xAI ModelInput 422。
		// 用 Marshal 得到稳定紧凑 JSON，避免 gjson Raw 的空格差异。
		compact, err := json.Marshal(json.RawMessage(source.Raw))
		if err != nil {
			return source.Raw, true
		}
		return string(compact), true
	}
	if source.Type == gjson.String {
		raw := strings.TrimSpace(source.String())
		if raw == "" {
			return "{}", true
		}
		parsed := gjson.Parse(raw)
		if parsed.IsObject() {
			// 内容已是 object。若外层有多余空白，写回紧凑/原 raw 均可。
			if raw == source.String() && raw == parsed.Raw {
				return raw, false
			}
			// 保留解析后的稳定 object 文本。
			if raw != parsed.Raw {
				return parsed.Raw, true
			}
			return raw, false
		}
		wrapped, err := json.Marshal(map[string]any{"input": raw})
		if err != nil {
			return "{}", true
		}
		return string(wrapped), true
	}
	if source.IsArray() || source.Type == gjson.Number || source.Type == gjson.True || source.Type == gjson.False || source.Type == gjson.Null {
		wrapped, err := json.Marshal(map[string]any{"value": json.RawMessage(source.Raw)})
		if err != nil {
			return "{}", true
		}
		return string(wrapped), true
	}
	return "{}", true
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

// additional_tools is a Codex/Responses Lite private input carrier. xAI's
// Responses schema rejects the carrier itself, but accepts supported tools at
// the top level. Before dropping the carrier, convert Codex-only nested tools
// (shell/local_shell/namespace) into xAI-supported shapes and merge them into
// top-level tools so Codex Desktop still keeps executable tools.
func sanitizeGrokResponsesInput(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"additional_tools"`)) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body, nil
	}

	rawItems := input.Array()
	filtered := make([]json.RawMessage, 0, len(rawItems))
	promotedTools := make([]json.RawMessage, 0)
	for _, item := range rawItems {
		if strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
			for _, nested := range item.Get("tools").Array() {
				if converted, ok := convertGrokCodexTool(nested); ok {
					promotedTools = append(promotedTools, converted)
				} else if nested.Get("tools").IsArray() {
					// Flatten namespace.tools when present inside additional_tools.
					for _, nestedTool := range nested.Get("tools").Array() {
						if converted, ok := convertGrokCodexTool(nestedTool); ok {
							promotedTools = append(promotedTools, converted)
						}
					}
				}
			}
			continue
		}
		filtered = append(filtered, json.RawMessage(item.Raw))
	}
	changed := len(filtered) != len(rawItems)
	if !changed && len(promotedTools) == 0 {
		return body, nil
	}
	if changed {
		encoded, err := json.Marshal(filtered)
		if err != nil {
			return nil, err
		}
		body, err = sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return nil, err
		}
	}
	if len(promotedTools) == 0 {
		return body, nil
	}
	return mergeGrokResponsesTools(body, promotedTools)
}

// stripGrokUnsupportedReasoningItems 移除 input 中携带 encrypted_content 的 reasoning 项。
//
// 背景：Codex 以 OpenAI Responses 协议工作，会在每轮把上一轮返回的 reasoning.encrypted_content
// 原样回传。但 grok/xAI 无法解码 Codex 从普通 /v1/responses 回传的加密推理内容 —— 第二轮起会返回
// 400 "Could not decode the compaction blob..."。
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

func grokResponsesToolDedupKey(tool gjson.Result) string {
	toolType := strings.TrimSpace(tool.Get("type").String())
	if toolType != "" {
		if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
			return "type:" + toolType + "\x00name:" + name
		}
		if toolType == "mcp" {
			if label := strings.TrimSpace(tool.Get("server_label").String()); label != "" {
				return "type:mcp\x00server_label:" + label
			}
		}
	}
	return "json:" + normalizeCompatSeedJSON(json.RawMessage(tool.Raw))
}

// sanitizeGrokReasoningNullContent 删除 reasoning 项中的 "content": null。
// xAI 的 untagged enum 反序列化器拒收该字段，返回 422。
func sanitizeGrokReasoningNullContent(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body, nil
	}

	items := input.Array()
	changed := false
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}
		contentResult := item.Get("content")
		if contentResult.Exists() && contentResult.Type == gjson.Null {
			var err error
			body, err = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.content", i))
			if err != nil {
				return nil, err
			}
			changed = true
		}
	}
	_ = changed
	return body, nil
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
	// Codex 可能只带 tool_choice、不带 tools；xAI 会 400：
	// "A tool_choice was set on the request but no tools were specified."
	if !tools.Exists() || !tools.IsArray() {
		if gjson.GetBytes(body, "tool_choice").Exists() {
			return sjson.DeleteBytes(body, "tool_choice")
		}
		return body, nil
	}

	rawTools := tools.Array()
	filteredTools := make([]json.RawMessage, 0, len(rawTools))
	seenTools := map[string]struct{}{}
	appendTool := func(converted json.RawMessage) {
		key := grokToolDedupeKey(converted)
		if _, exists := seenTools[key]; exists {
			return
		}
		seenTools[key] = struct{}{}
		filteredTools = append(filteredTools, converted)
	}
	for _, tool := range rawTools {
		if converted, ok := convertGrokCodexTool(tool); ok {
			appendTool(converted)
			continue
		}
		// Flatten namespace / nested tools arrays that Codex may send.
		if tool.Get("tools").IsArray() {
			for _, nested := range tool.Get("tools").Array() {
				if converted, ok := convertGrokCodexTool(nested); ok {
					appendTool(converted)
				}
			}
		}
	}

	var err error
	// 即使 tool 数量不变，也可能发生 schema 归一（exec_command 参数补齐）。
	// 用序列化结果对比，避免弱 schema 原样透传。
	toolsChanged := len(filteredTools) != len(rawTools)
	if !toolsChanged {
		originalEncoded, marshalErr := json.Marshal(rawToolsAsRaw(rawTools))
		if marshalErr == nil {
			filteredEncoded, marshalErr := json.Marshal(filteredTools)
			if marshalErr == nil && !bytes.Equal(originalEncoded, filteredEncoded) {
				toolsChanged = true
			}
		} else {
			// 序列化失败时保守认为有变化，走写回路径。
			toolsChanged = true
		}
	}
	if toolsChanged {
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

// convertGrokCodexTool normalizes Codex-only tool shapes into xAI-supported
// Responses tools. Codex local shell / namespace wrappers are rewritten into
// ordinary function tools so Grok can still emit executable function_call.
func convertGrokCodexTool(tool gjson.Result) (json.RawMessage, bool) {
	if !tool.IsObject() {
		return nil, false
	}
	toolType := strings.TrimSpace(tool.Get("type").String())
	switch toolType {
	case "function":
		if !isValidGrokResponsesTool(tool) {
			return nil, false
		}
		if normalized, ok := normalizeGrokCodexFunctionTool(tool); ok {
			return normalized, true
		}
		return json.RawMessage(tool.Raw), true
	case "shell":
		// xAI hosted shell needs environment. Codex local shell does not, so
		// rewrite it to a function tool the client can still execute.
		if tool.Get("environment").Exists() {
			return json.RawMessage(tool.Raw), true
		}
		return marshalGrokCodexShellFunctionTool(tool)
	case "local_shell":
		return marshalGrokCodexShellFunctionTool(tool)
	case "namespace":
		// Flatten nested function tools; drop the namespace wrapper itself.
		// Caller loops over nested tools separately when promoting.
		return nil, false
	default:
		if _, ok := grokResponsesSupportedToolTypes[toolType]; ok && isValidGrokResponsesTool(tool) {
			return json.RawMessage(tool.Raw), true
		}
		// Some Codex payloads put function tools under namespace.tools.
		if tool.Get("tools").IsArray() {
			return nil, false
		}
		return nil, false
	}
}

func marshalGrokCodexShellFunctionTool(tool gjson.Result) (json.RawMessage, bool) {
	name := strings.TrimSpace(firstNonEmptyString(
		tool.Get("name").String(),
		tool.Get("function.name").String(),
	))
	// Keep names Codex already understands. local_shell is rewritten to
	// exec_command because current Codex Desktop/TUI primarily execute that
	// function tool for command running.
	switch strings.ToLower(name) {
	case "", "local_shell", "shell", "container.exec":
		name = "exec_command"
	}
	return marshalGrokCodexCanonicalFunctionTool(name, tool)
}

// normalizeGrokCodexFunctionTool 把 Codex 常见 shell 工具 schema 收敛到客户端可执行形态。
// 已是完整 function 声明时尽量保留；exec_command/write_stdin 补齐关键参数字段。
func normalizeGrokCodexFunctionTool(tool gjson.Result) (json.RawMessage, bool) {
	name := strings.TrimSpace(firstNonEmptyString(
		tool.Get("name").String(),
		tool.Get("function.name").String(),
	))
	switch strings.ToLower(name) {
	case "exec_command", "local_shell", "shell", "container.exec":
		return marshalGrokCodexCanonicalFunctionTool("exec_command", tool)
	case "write_stdin":
		return marshalGrokCodexCanonicalFunctionTool("write_stdin", tool)
	default:
		return nil, false
	}
}

func marshalGrokCodexCanonicalFunctionTool(name string, tool gjson.Result) (json.RawMessage, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	description := strings.TrimSpace(firstNonEmptyString(
		tool.Get("description").String(),
		tool.Get("function.description").String(),
	))
	var parameters map[string]any
	switch strings.ToLower(name) {
	case "exec_command":
		if description == "" {
			description = "Runs a command in a PTY, returning output or a session ID for ongoing interaction."
		}
		// 对齐 Codex Desktop 实际 exec_command：cmd 字符串 + 可选 workdir。
		// 额外允许常见可选字段，避免 Grok 生成后客户端拒收。
		parameters = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cmd": map[string]any{
					"type":        "string",
					"description": "Shell command to execute.",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Optional working directory.",
				},
				"yield_time_ms": map[string]any{
					"type":        "integer",
					"description": "Optional wait before yielding output.",
				},
				"max_output_tokens": map[string]any{
					"type":        "integer",
					"description": "Optional output token budget.",
				},
				"login": map[string]any{
					"type":        "boolean",
					"description": "Optional login shell semantics.",
				},
				"shell": map[string]any{
					"type":        "string",
					"description": "Optional shell binary.",
				},
				"tty": map[string]any{
					"type":        "boolean",
					"description": "Optional PTY allocation.",
				},
				"justification": map[string]any{
					"type":        "string",
					"description": "Optional user-facing approval question.",
				},
			},
			"required":             []string{"cmd"},
			"additionalProperties": false,
		}
	case "write_stdin":
		if description == "" {
			description = "Writes characters to an existing unified exec session and returns recent output."
		}
		parameters = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{
					"type":        "integer",
					"description": "Identifier of the running unified exec session.",
				},
				"chars": map[string]any{
					"type":        "string",
					"description": "Bytes to write to stdin.",
				},
				"yield_time_ms": map[string]any{
					"type":        "integer",
					"description": "Optional wait before yielding output.",
				},
				"max_output_tokens": map[string]any{
					"type":        "integer",
					"description": "Optional output token budget.",
				},
			},
			"required":             []string{"session_id"},
			"additionalProperties": false,
		}
	default:
		return nil, false
	}
	out := map[string]any{
		"type":        "function",
		"name":        name,
		"description": description,
		"parameters":  parameters,
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// requireGrokResponsesFunctionToolChoice 在请求已具备客户端 function 工具时，把 auto/空
// tool_choice 收敛为 required。这样 Grok 必须先发起工具调用，但仍由模型选择具体工具，
// 不把所有任务都硬导向 exec_command。
// 不覆盖 none 或已经点名其他 function 的显式选择；工具结果回传轮次保持原策略，避免循环。
func requireGrokResponsesFunctionToolChoice(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return body, nil
	}
	hasInitialFunctionTool := false
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) != "function" {
			continue
		}
		name := strings.TrimSpace(firstNonEmptyString(
			tool.Get("name").String(),
			tool.Get("function.name").String(),
		))
		if name != "" && !grokFunctionToolNeedsPriorOutput(tool) {
			hasInitialFunctionTool = true
			break
		}
	}
	if !hasInitialFunctionTool {
		return body, nil
	}
	if grokResponsesInputHasToolOutput(body) {
		return body, nil
	}

	toolChoice := gjson.GetBytes(body, "tool_choice")
	if toolChoice.Exists() {
		if toolChoice.Type == gjson.String {
			switch strings.ToLower(strings.TrimSpace(toolChoice.String())) {
			case "", "auto", "required":
				// 可抬升
			default:
				// none 或其他显式字符串策略保持不动
				return body, nil
			}
		} else if toolChoice.IsObject() {
			choiceType := strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String()))
			switch choiceType {
			case "", "auto", "required", "any":
				// 可抬升
			case "function":
				// 已点名具体函数，不覆盖
				return body, nil
			case "none":
				return body, nil
			default:
				// 未知对象形态：若不是 Codex shell 相关，保持原样避免误伤
				return body, nil
			}
		} else {
			return body, nil
		}
	}

	return sjson.SetBytes(body, "tool_choice", "required")
}

func grokFunctionToolNeedsPriorOutput(tool gjson.Result) bool {
	required := tool.Get("parameters.required")
	if !required.IsArray() {
		required = tool.Get("function.parameters.required")
	}
	if !required.IsArray() {
		return false
	}
	for _, item := range required.Array() {
		switch strings.ToLower(strings.TrimSpace(item.String())) {
		case "session_id", "call_id":
			return true
		}
	}
	return false
}

func grokResponsesInputHasToolOutput(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output", "tool_search_output", "local_shell_call_output":
			return true
		}
	}
	return false
}

func mergeGrokResponsesTools(body []byte, extra []json.RawMessage) ([]byte, error) {
	if len(extra) == 0 {
		return body, nil
	}
	existing := make([]json.RawMessage, 0)
	seen := map[string]struct{}{}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		for _, tool := range tools.Array() {
			if converted, ok := convertGrokCodexTool(tool); ok {
				key := grokToolDedupeKey(converted)
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				existing = append(existing, converted)
			} else if tool.Get("tools").IsArray() {
				// Flatten namespace.tools if present on top-level tools.
				for _, nested := range tool.Get("tools").Array() {
					if converted, ok := convertGrokCodexTool(nested); ok {
						key := grokToolDedupeKey(converted)
						if _, exists := seen[key]; exists {
							continue
						}
						seen[key] = struct{}{}
						existing = append(existing, converted)
					}
				}
			}
		}
	}
	for _, tool := range extra {
		key := grokToolDedupeKey(tool)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		existing = append(existing, tool)
	}
	if len(existing) == 0 {
		return body, nil
	}
	encoded, err := json.Marshal(existing)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "tools", encoded)
}

func rawToolsAsRaw(items []gjson.Result) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		out = append(out, json.RawMessage(item.Raw))
	}
	return out
}

func grokToolDedupeKey(tool json.RawMessage) string {
	t := gjson.ParseBytes(tool)
	return strings.TrimSpace(t.Get("type").String()) + "|" + strings.TrimSpace(firstNonEmptyString(
		t.Get("name").String(),
		t.Get("function.name").String(),
	))
}

func isValidGrokResponsesTool(tool gjson.Result) bool {
	if !tool.IsObject() {
		return false
	}
	toolType := strings.TrimSpace(tool.Get("type").String())
	switch toolType {
	case "shell":
		// xAI hosted shell 需要 environment；Codex local shell 无此字段，不能原样透传。
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
	// Image-description probes are auxiliary requests, not conversation turns.
	// Do not bind them to the caller's Grok prompt-cache identity.
	upstreamReq, err := buildGrokResponsesRequest(upstreamCtx, c, account, body, token, "", s.cfg)
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
		upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
		if upstreamMsg == "" {
			upstreamMsg = fmt.Sprintf("xAI image bridge upstream returned status %d", resp.StatusCode)
		}
		kind := "http_error"
		if s.shouldFailoverGrokUpstreamError(resp.StatusCode, respBody) {
			kind = "failover"
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
			Kind:               kind,
			Message:            upstreamMsg,
		})
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		if s.shouldFailoverGrokUpstreamError(resp.StatusCode, respBody) {
			return "", OpenAIUsage{}, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				ResponseHeaders:        resp.Header.Clone(),
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return "", OpenAIUsage{}, fmt.Errorf("grok composer image bridge upstream error: %s", upstreamMsg)
	}

	s.updateGrokUsageFromResponse(ctx, account, resp.Header, resp.StatusCode)
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

func buildGrokResponsesRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token, cacheIdentity string, cfg *config.Config) (*http.Request, error) {
	baseURL := ""
	if account != nil {
		baseURL = account.GetGrokBaseURL()
	}
	return buildGrokResponsesRequestWithBase(ctx, c, account, body, token, cacheIdentity, cfg, baseURL)
}

func buildGrokResponsesRequestWithBase(ctx context.Context, c *gin.Context, account *Account, body []byte, token, cacheIdentity string, cfg *config.Config, baseURL string) (*http.Request, error) {
	targetURL, err := buildGrokResponsesURLWithBase(account, cfg, baseURL)
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
	applyGrokOAuthUpstreamHeadersForBaseURL(account, req.Header, baseURL)
	applyGrokCacheHeaders(req.Header, cacheIdentity)
	if c != nil {
		if v := c.GetHeader("OpenAI-Beta"); strings.TrimSpace(v) != "" {
			req.Header.Set("OpenAI-Beta", v)
		}
	}
	// 账号级请求头覆写最后应用，使配置值优先于上面的内置默认头；
	// 打到官方 CLI 网关时身份头仍由共享传输层最终强制。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// applyGrokCLIHeaders identifies free-build CLI proxy traffic as a supported
// Grok CLI version. Public api.x.ai OAuth traffic must not send these headers.
func applyGrokCLIHeaders(headers http.Header) {
	applyGrokCLIHeadersForBaseURL(headers, xai.DefaultCLIBaseURL)
}

// applyGrokOAuthUpstreamHeaders attaches CLI identity headers only when the
// selected upstream is cli-chat-proxy.
func applyGrokOAuthUpstreamHeaders(account *Account, headers http.Header) {
	if account == nil {
		return
	}
	applyGrokOAuthUpstreamHeadersForBaseURL(account, headers, account.GetGrokBaseURL())
}

func applyGrokOAuthUpstreamHeadersForBaseURL(account *Account, headers http.Header, baseURL string) {
	if account == nil || !account.IsGrokOAuth() {
		return
	}
	applyGrokCLIHeadersForBaseURL(headers, baseURL)
}

func applyGrokCLIHeadersForBaseURL(headers http.Header, baseURL string) {
	if headers == nil {
		return
	}
	if !xai.IsCLIChatProxyBaseURL(baseURL) {
		return
	}
	headers.Set("User-Agent", grokUpstreamUserAgent)
	headers.Set("X-Grok-Client-Version", grokCLIVersion)
	headers.Set("X-Grok-Client-Mode", "interactive")
	xai.MaybeApplyCLIChatProxyHeaders(headers, baseURL)
}

func (s *OpenAIGatewayService) updateGrokUsageSnapshot(ctx context.Context, account *Account, snapshot *xai.QuotaSnapshot) {
	if s == nil || account == nil || account.ID <= 0 || snapshot == nil {
		return
	}
	accountID := account.ID
	now := time.Now()
	resetAt, hasActiveLimit := grokRateLimitResetAtForAccount(account, snapshot, now)
	if hasActiveLimit {
		normalizeGrokExhaustedWindowResets(snapshot, resetAt, now)
	}
	recovery := isSuccessfulGrokRateLimitRecovery(account, snapshot)
	critical := snapshot.StatusCode == http.StatusTooManyRequests || hasActiveLimit || recovery
	if s.codexSnapshotThrottle != nil {
		allowed := s.codexSnapshotThrottle.Allow(accountID, now)
		if !critical && !allowed {
			return
		}
	}

	stateCtx := ctx
	if hasActiveLimit {
		var cancel context.CancelFunc
		stateCtx, cancel = openAIAccountStateContext(ctx)
		defer cancel()
	}
	if s.accountRepo != nil {
		_ = s.accountRepo.UpdateExtra(stateCtx, accountID, map[string]any{
			grokQuotaSnapshotExtraKey: snapshot,
		})
	}
	// Error responses are reconciled by handleGrokAccountUpstreamError, which
	// also installs the immediate in-memory scheduling block. Successful
	// responses can still consume the last available request/token, so persist
	// that exhausted window here as a real rate limit rather than relying only
	// on the passive snapshot scheduler check.
	if hasActiveLimit {
		s.rateLimitGrok(stateCtx, account, resetAt)
	} else if recovery {
		clearGrokRateLimitAfterRecovery(stateCtx, s.accountRepo, account)
	}
}

func (s *OpenAIGatewayService) updateGrokUsageFromResponse(ctx context.Context, account *Account, headers http.Header, statusCode int) {
	snapshot := parseGrokQuotaSnapshot(headers, statusCode, time.Now())
	if snapshot != nil {
		s.updateGrokUsageSnapshot(ctx, account, snapshot)
		return
	}
	// Successful responses are recovery evidence even when the upstream omits
	// optional quota headers. Do not replace an informative stored snapshot with
	// an empty one; only clear the exact observed cooldown generation.
	recoverySnapshot := &xai.QuotaSnapshot{StatusCode: statusCode}
	if isSuccessfulGrokRateLimitRecovery(account, recoverySnapshot) {
		clearGrokRateLimitAfterRecovery(ctx, s.accountRepo, account)
	}
}

func parseGrokQuotaSnapshot(headers http.Header, statusCode int, now time.Time) *xai.QuotaSnapshot {
	snapshot := xai.ParseQuotaHeaders(headers, statusCode)
	if snapshot == nil && statusCode == http.StatusTooManyRequests {
		return &xai.QuotaSnapshot{
			StatusCode: statusCode,
			UpdatedAt:  now.UTC().Format(time.RFC3339),
		}
	}
	return snapshot
}

func normalizeGrokExhaustedWindowResets(snapshot *xai.QuotaSnapshot, resetAt, now time.Time) {
	if snapshot == nil || !resetAt.After(now) {
		return
	}
	for _, window := range []*xai.QuotaWindow{snapshot.Requests, snapshot.Tokens} {
		if window == nil || window.Remaining == nil || *window.Remaining > 0 {
			continue
		}
		candidate := time.Time{}
		if window.ResetUnix != nil && *window.ResetUnix > 0 {
			candidate = time.Unix(*window.ResetUnix, 0)
		} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetAt)); err == nil {
			candidate = parsed
		}
		if !candidate.After(now) {
			candidate = resetAt
		}
		resetUnix := candidate.Unix()
		window.ResetUnix = &resetUnix
		window.ResetAt = candidate.UTC().Format(time.RFC3339)
	}
}

func grokRateLimitResetAt(snapshot *xai.QuotaSnapshot, now time.Time) (time.Time, bool) {
	if snapshot == nil {
		return time.Time{}, false
	}

	// Retry-After is xAI's explicit retry boundary. Use the observation time so
	// a persisted snapshot does not start a fresh cooldown every time it is read.
	retryAfterExpired := false
	var resetAt time.Time
	if snapshot.RetryAfterSeconds != nil && *snapshot.RetryAfterSeconds > 0 {
		observedAt := now
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(snapshot.UpdatedAt)); err == nil {
			observedAt = parsed
		}
		retryAfterResetAt := observedAt.Add(time.Duration(*snapshot.RetryAfterSeconds) * time.Second)
		if retryAfterResetAt.After(now) {
			resetAt = retryAfterResetAt
		} else {
			retryAfterExpired = true
		}
	}

	exhausted := false
	for _, window := range []*xai.QuotaWindow{snapshot.Requests, snapshot.Tokens} {
		if window == nil || window.Remaining == nil || *window.Remaining > 0 {
			continue
		}
		exhausted = true
		candidate := time.Time{}
		if window.ResetUnix != nil && *window.ResetUnix > 0 {
			candidate = time.Unix(*window.ResetUnix, 0)
		} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetAt)); err == nil {
			candidate = parsed
		}
		if candidate.After(now) && candidate.After(resetAt) {
			resetAt = candidate
		}
	}
	if !resetAt.IsZero() {
		return resetAt, true
	}
	// An observed Retry-After is an absolute boundary once combined with the
	// snapshot timestamp. Do not turn an expired persisted snapshot into a new
	// rolling fallback cooldown, but still allow a later explicit window reset.
	if retryAfterExpired {
		return time.Time{}, false
	}
	if exhausted || snapshot.StatusCode == http.StatusTooManyRequests {
		return now.Add(grokRateLimitFallbackCooldown), true
	}
	return time.Time{}, false
}

func grokRateLimitResetAtForAccount(account *Account, snapshot *xai.QuotaSnapshot, now time.Time) (time.Time, bool) {
	resetAt, limited := grokRateLimitResetAt(snapshot, now)
	if !limited || !isGrokOAuthAccount(account) || snapshot == nil || snapshot.StatusCode != http.StatusTooManyRequests {
		return resetAt, limited
	}
	if account.RateLimitedAt == nil || account.RateLimitResetAt == nil {
		return resetAt, true
	}
	previousResetAt := *account.RateLimitResetAt
	if previousResetAt.After(now) || now.Sub(previousResetAt) > grokRateLimitBackoffQuietPeriod {
		return resetAt, true
	}
	previousCooldown := previousResetAt.Sub(*account.RateLimitedAt)
	if previousCooldown <= 0 {
		return resetAt, true
	}

	adaptiveCooldown := grokRateLimitRepeatCooldown
	switch {
	case previousCooldown >= grokRateLimitSustainedCooldown:
		adaptiveCooldown = grokRateLimitMaxAdaptiveCooldown
	case previousCooldown >= grokRateLimitRepeatCooldown:
		adaptiveCooldown = grokRateLimitSustainedCooldown
	}
	adaptiveResetAt := now.Add(adaptiveCooldown)
	if adaptiveResetAt.After(resetAt) {
		resetAt = adaptiveResetAt
	}
	return resetAt, true
}

func normalizeGrokRateLimitResetAt(account *Account, resetAt, now time.Time) time.Time {
	if !resetAt.After(now) {
		resetAt = now.Add(grokRateLimitFallbackCooldown)
	}
	if account != nil && account.RateLimitResetAt != nil && account.RateLimitResetAt.After(resetAt) {
		resetAt = *account.RateLimitResetAt
	}
	return resetAt
}

type grokRateLimitExtendingRepository interface {
	SetRateLimitedIfLater(ctx context.Context, id int64, resetAt time.Time) error
}

type grokRateLimitRecoveryRepository interface {
	ClearRateLimitIfObserved(ctx context.Context, id int64, observedLimitedAt, observedResetAt time.Time) (bool, error)
}

func isSuccessfulGrokRateLimitRecovery(account *Account, snapshot *xai.QuotaSnapshot) bool {
	return isGrokOAuthAccount(account) &&
		account.RateLimitedAt != nil &&
		account.RateLimitResetAt != nil &&
		snapshot != nil &&
		snapshot.StatusCode >= http.StatusOK &&
		snapshot.StatusCode < http.StatusMultipleChoices
}

func clearGrokRateLimitAfterRecovery(ctx context.Context, repo AccountRepository, account *Account) {
	if repo == nil || account == nil || account.RateLimitedAt == nil || account.RateLimitResetAt == nil || ctx.Err() != nil {
		return
	}
	recoveryRepo, ok := repo.(grokRateLimitRecoveryRepository)
	if !ok {
		return
	}
	_, err := recoveryRepo.ClearRateLimitIfObserved(ctx, account.ID, *account.RateLimitedAt, *account.RateLimitResetAt)
	if err != nil {
		slog.Warn("grok_rate_limit_recovery_clear_failed", "account_id", account.ID, "error", err)
	}
}

func persistGrokRateLimit(ctx context.Context, repo AccountRepository, account *Account, resetAt time.Time) {
	if repo == nil || account == nil || account.ID <= 0 {
		return
	}
	resetAt = normalizeGrokRateLimitResetAt(account, resetAt, time.Now())
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	var err error
	if extendingRepo, ok := repo.(grokRateLimitExtendingRepository); ok {
		err = extendingRepo.SetRateLimitedIfLater(stateCtx, account.ID, resetAt)
	} else {
		err = repo.SetRateLimited(stateCtx, account.ID, resetAt)
	}
	if err != nil {
		slog.Warn("persist_grok_rate_limit_failed", "account_id", account.ID, "reset_at", resetAt.UTC(), "error", err)
	}
}

func (s *OpenAIGatewayService) rateLimitGrok(ctx context.Context, account *Account, resetAt time.Time) {
	if s == nil || account == nil {
		return
	}
	resetAt = normalizeGrokRateLimitResetAt(account, resetAt, time.Now())

	runtimeUntil := resetAt
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(runtimeUntil) {
		runtimeUntil = *account.TempUnschedulableUntil
	}
	s.BlockAccountScheduling(account, runtimeUntil, "429")
	persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
}

// canFallbackGrokCLIProxyToPublicAPI reports whether this OAuth request should
// retry the same account on https://api.x.ai after cli-chat-proxy returns 403.
func canFallbackGrokCLIProxyToPublicAPI(account *Account, baseURL string, statusCode int) bool {
	if account == nil || !account.IsGrokOAuth() {
		return false
	}
	if statusCode != http.StatusForbidden {
		return false
	}
	if !xai.IsCLIChatProxyBaseURL(baseURL) {
		return false
	}
	return true
}

func appendGrokCLIProxyFallbackOpsEvent(c *gin.Context, account *Account, statusCode int, requestID, message string) {
	if c == nil || account == nil {
		return
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: statusCode,
		UpstreamRequestID:  requestID,
		Kind:               "fallback",
		Message:            message,
	})
}

// holdGrokAccountUntilSuccess sticky-blocks a Grok account (status=error,
// unschedulable) until a proven success clears the hold. Used for permanent-ish
// upstream denials that an account cannot self-clear per request: entitlement /
// permission 403 and payment / spending-limit 402. The recover probe's sticky
// queue re-tests held accounts and ClearGrokHoldAfterSuccess restores them.
func (s *OpenAIGatewayService) holdGrokAccountUntilSuccess(ctx context.Context, account *Account, reason string) {
	if s == nil || account == nil {
		return
	}
	if s.accountRepo != nil {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		_ = s.accountRepo.SetError(stateCtx, account.ID, reason)
		MarkGrokHoldUntilSuccess(stateCtx, s.accountRepo, account)
	}
	account.Status = StatusError
	account.Schedulable = false
	account.ErrorMessage = reason
	account.TempUnschedulableUntil = nil
	account.TempUnschedulableReason = ""
	s.BlockAccountScheduling(account, time.Time{}, reason)
}

func (s *OpenAIGatewayService) handleGrokAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte) {
	if s == nil || account == nil {
		return
	}
	// #4719 定点移植：请求级内容安全 403 由 prompt/媒体触发，换账号也改变不了结果，
	// 直接返回，不踢号、不 failover、不污染账号状态。账号级 entitlement/封禁 403 走下面正常路径。
	if isGrokContentPolicyRejection(statusCode, responseBody) {
		return
	}
	now := time.Now()
	s.updateGrokUsageSnapshot(ctx, account, parseGrokQuotaSnapshot(headers, statusCode, now))
	switch statusCode {
	case http.StatusUnauthorized:
		s.tempUnscheduleGrok(ctx, account, 10*time.Minute, "grok credentials unauthorized")
	case http.StatusPaymentRequired:
		// 402 = xAI payment required / spending-limit block. This is a billing-side
		// denial the account cannot self-clear per request, so sticky-hold it until a
		// proven success (recover probe) clears the hold. Without this the account
		// stays schedulable and gets reselected every request, burning the failover
		// budget and surfacing 402 to clients.
		s.holdGrokAccountUntilSuccess(ctx, account, grokPaymentRequiredReason)
	case http.StatusForbidden:
		// Free-usage exhaustion can arrive as 403/429; keep recoverable rate-limit semantics.
		if freeInfo := xai.ParseFreeUsageExhaustedBody(responseBody); freeInfo != nil && freeInfo.Exhausted {
			cooldown := freeInfo.Cooldown
			if cooldown <= 0 {
				cooldown = xai.DefaultFreeUsageCooldown
			}
			until := now.Add(cooldown)
			if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(until) {
				until = *account.RateLimitResetAt
			}
			if s.accountRepo != nil {
				persistGrokRateLimit(ctx, s.accountRepo, account, until)
				_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
					"grok_free_usage_exhausted":      true,
					"grok_free_usage_error_code":     freeInfo.ErrorCode,
					"grok_free_usage_window":         freeInfo.Window,
					"grok_free_usage_model":          freeInfo.Model,
					"grok_free_usage_cooldown_until": until.UTC().Format(time.RFC3339),
				})
			}
			return
		}

		// True permission/entitlement denials should leave the active pool.
		// Prefer sticky error immediately for explicit permission-denied bodies;
		// otherwise escalate after consecutive 403s to avoid endless short cooldowns.
		if IsGrokPermissionDeniedBody(responseBody) {
			s.holdGrokAccountUntilSuccess(ctx, account, grokHoldUntilSuccessReason)
			return
		}
		if s.rateLimitService != nil {
			upstreamMsg := strings.TrimSpace(string(responseBody))
			if s.rateLimitService.EscalateGrokForbiddenOn403(ctx, account, upstreamMsg, responseBody) {
				return
			}
		}
		// Ambiguous 403: short cooldown only.
		cooldown := 2 * time.Minute
		reason := "grok upstream 403"
		if account.IsGrokOAuth() && xai.IsCLIChatProxyBaseURL(account.GetGrokBaseURL()) {
			reason = "grok cli-chat-proxy 403"
		} else if account.IsGrokOAuth() {
			reason = "grok public api 403"
		}
		s.tempUnscheduleGrok(ctx, account, cooldown, reason)
	case http.StatusTooManyRequests:
		// updateGrokUsageSnapshot 只在快照能解析出有效配额窗口/retry-after 时才落库限流。
		// 但 cli-chat-proxy 的 429 常不带配额头(headers_observed=false)：此时上面既不写 DB
		// 限流、也不设有效冷却窗口，账号会以"DB 可用"状态被反复重新调度、持续 429，形成
		// "仪表盘一直显示可用、请求一直失败"的死循环，且耗尽状态无法经 runtime 同步回本机探测。
		// 这里补一层显式兜底：按上游语义解析冷却时长(free-usage 耗尽=滚动窗口默认 24h，普通
		// 429=2min 或 retry-after)，durable 落 rate_limit_reset_at + 设内存封禁，与探测路径
		// ApplyGrokProbeOrTestStatus 的 429 处理保持一致，让耗尽账号即时退出调度并可被同步/探测。
		cooldown, _, freeInfo, _ := xai.ResolveGrokCooldown(statusCode, headers, responseBody)
		if cooldown <= 0 {
			cooldown = 2 * time.Minute
		}
		until := now.Add(cooldown)
		// 已有更晚的限流窗口时不缩短(与 403 free-usage 分支一致)。
		if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(until) {
			until = *account.RateLimitResetAt
		}
		s.rateLimitGrok(ctx, account, until)
		if freeInfo != nil && freeInfo.Exhausted && s.accountRepo != nil {
			_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
				"grok_free_usage_exhausted":      true,
				"grok_free_usage_error_code":     freeInfo.ErrorCode,
				"grok_free_usage_window":         freeInfo.Window,
				"grok_free_usage_model":          freeInfo.Model,
				"grok_free_usage_cooldown_until": until.UTC().Format(time.RFC3339),
			})
		}
	default:
		if statusCode >= 500 {
			s.tempUnscheduleGrok(ctx, account, 2*time.Minute, "grok upstream temporary error")
		}
	}
}

func (s *OpenAIGatewayService) tempUnscheduleGrok(ctx context.Context, account *Account, cooldown time.Duration, reason string) {
	if s == nil || account == nil {
		return
	}
	until := time.Now().Add(cooldown)
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(until) {
		until = *account.TempUnschedulableUntil
	}
	s.BlockAccountScheduling(account, until, reason)
	if s.accountRepo != nil {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		_ = s.accountRepo.SetTempUnschedulable(stateCtx, account.ID, until, reason)
	}
}
