package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// forwardResponsesViaRawChatCompletions serves /v1/responses clients through an
// upstream that only supports /v1/chat/completions.
func (s *OpenAIGatewayService) forwardResponsesViaRawChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	// Third-party OpenAI-compatible chat fallbacks still need Codex shell tools
	// rewritten into function tools before Responses->Chat conversion.
	if shouldSanitizeOpenAICompatibleResponsesModelInput(account) {
		if normalizedBody, changed, normErr := normalizeOpenAICompatibleCodexToolsOnly(body); normErr != nil {
			return nil, normErr
		} else if changed {
			body = normalizedBody
			logger.L().Debug("openai responses chat fallback: normalized codex tools",
				zap.Int64("account_id", account.ID),
			)
		}
	}

	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		writeOpenAIResponsesFallbackError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	originalModel := strings.TrimSpace(responsesReq.Model)
	if originalModel == "" {
		writeOpenAIResponsesFallbackError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}

	clientStream := responsesReq.Stream
	serviceTier := extractOpenAIServiceTierFromBody(body)
	// custom 工具（如 codex 的 exec）降级为 function 工具转发，回程需按名字还原为
	// custom_tool_call 项，先记下名字集合；tool_search 工具同理，回程还原为
	// tool_search_call 项；namespace 子工具（如 MCP 工具）摊平转发，回程按映射还原
	// 为带 namespace 字段的 function_call 项。
	effectiveTools, err := apicompat.EffectiveResponsesTools(&responsesReq)
	if err != nil {
		writeOpenAIResponsesFallbackError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, fmt.Errorf("resolve responses tools: %w", err)
	}
	customTools := apicompat.CustomToolNames(effectiveTools)
	toolSearch := apicompat.HasToolSearchTool(effectiveTools)
	namespaceTools := apicompat.NamespaceToolNames(effectiveTools)

	chatReq, err := apicompat.ResponsesToChatCompletionsRequest(&responsesReq)
	if err != nil {
		writeOpenAIResponsesFallbackError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, fmt.Errorf("convert responses to chat completions: %w", err)
	}

	billingModel := resolveOpenAIForwardModelForContext(ctx, account, originalModel, "")
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, upstreamModel, billingModel, originalModel)
	// 国产模型默认 effort 补充：需要 mappedModel 判定，推迟到 billingModel 算出之后。
	reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, billingModel)
	chatReq.Model = upstreamModel
	if clientStream {
		chatReq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completions fallback request: %w", err)
	}
	chatBody, err = s.applyOpenAIFastPolicyToBody(ctx, account, upstreamModel, chatBody)
	if err != nil {
		var blocked *OpenAIFastBlockedError
		if errors.As(err, &blocked) {
			writeOpenAIFastPolicyBlockedResponse(c, blocked)
		}
		return nil, err
	}
	if serviceTier == nil {
		serviceTier = extractOpenAIServiceTierFromBody(chatBody)
	}

	logger.L().Debug("openai responses: forwarding via raw chat completions",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", clientStream),
	)

	// Build and send upstream request via the shared CC pipeline.
	// Image-input requests may need same-account VL model retries before
	// account failover: multi-model pools often keep many vision destinations.
	apiKey, targetURL, err := s.resolveCCFallbackTarget(account)
	if err != nil {
		return nil, err
	}
	visionTried := map[string]struct{}{}
	if OpenAIImageInputIntentFromContext(ctx) && strings.TrimSpace(upstreamModel) != "" {
		visionTried[strings.ToLower(strings.TrimSpace(upstreamModel))] = struct{}{}
	}
	// One-shot salvage: some VL destinations reject Codex tool schemas while still
	// accepting plain multimodal chat. Prefer recognizing the image over failing hard.
	toolsStrippedForVision := false

	for {
		resp, sendErr := s.sendCCUpstreamRequest(ctx, c, account, targetURL, chatBody, clientStream, apiKey, account.GetOpenAIUserAgent(), "")
		if sendErr != nil {
			return nil, sendErr
		}

		if resp.StatusCode >= 400 {
			respBody, upstreamMsg := s.readOpenAIUpstreamError(resp)
			_ = resp.Body.Close()
			if OpenAIImageInputIntentFromContext(ctx) {
				if nextModel, ok := shouldRetryOpenAISameAccountVisionModel(
					account,
					resp.StatusCode,
					upstreamMsg,
					respBody,
					upstreamModel,
					visionTried,
				); ok {
					prevModel := upstreamModel
					upstreamModel = nextModel
					billingModel = nextModel
					chatReq.Model = nextModel
					var marshalErr error
					chatBody, marshalErr = json.Marshal(chatReq)
					if marshalErr != nil {
						return nil, fmt.Errorf("marshal chat completions vision retry body: %w", marshalErr)
					}
					chatBody, marshalErr = s.applyOpenAIFastPolicyToBody(ctx, account, upstreamModel, chatBody)
					if marshalErr != nil {
						var blocked *OpenAIFastBlockedError
						if errors.As(marshalErr, &blocked) {
							writeOpenAIFastPolicyBlockedResponse(c, blocked)
						}
						return nil, marshalErr
					}
					visionTried[strings.ToLower(nextModel)] = struct{}{}
					logger.LegacyPrintf(
						"service.openai_gateway",
						"[OpenAI] Vision same-account model retry (responses->chat): %s -> %s (account: %s status=%d msg=%s)",
						prevModel,
						nextModel,
						account.Name,
						resp.StatusCode,
						truncateString(upstreamMsg, 120),
					)
					continue
				}
				// Same model, drop tools/functions so pure VL models can still describe the image.
				if !toolsStrippedForVision &&
					(len(chatReq.Tools) > 0 || len(chatReq.Functions) > 0 || len(chatReq.ToolChoice) > 0 || len(chatReq.FunctionCall) > 0) &&
					isOpenAICompatibleModelPayloadIncompatError(upstreamMsg, respBody) {
					toolsStrippedForVision = true
					chatReq.Tools = nil
					chatReq.Functions = nil
					chatReq.ToolChoice = nil
					chatReq.FunctionCall = nil
					chatReq.ParallelToolCalls = nil
					// Drop empty custom/tool-search recovery sets; response is text-only now.
					customTools = nil
					toolSearch = false
					namespaceTools = nil
					var marshalErr error
					chatBody, marshalErr = json.Marshal(chatReq)
					if marshalErr != nil {
						return nil, fmt.Errorf("marshal chat completions vision tools-stripped body: %w", marshalErr)
					}
					chatBody, marshalErr = s.applyOpenAIFastPolicyToBody(ctx, account, upstreamModel, chatBody)
					if marshalErr != nil {
						var blocked *OpenAIFastBlockedError
						if errors.As(marshalErr, &blocked) {
							writeOpenAIFastPolicyBlockedResponse(c, blocked)
						}
						return nil, marshalErr
					}
					logger.LegacyPrintf(
						"service.openai_gateway",
						"[OpenAI] Vision tools stripped for same-account retry (responses->chat): model=%s account=%s status=%d msg=%s",
						upstreamModel,
						account.Name,
						resp.StatusCode,
						truncateString(upstreamMsg, 120),
					)
					continue
				}
			}
			// Re-wrap body for shared error handlers that may re-read it.
			resp.Body = io.NopCloser(strings.NewReader(string(respBody)))
			if foErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel); foErr != nil {
				return nil, foErr
			}
			return s.handleErrorResponse(ctx, resp, c, account, chatBody, billingModel)
		}

		if clientStream {
			return s.streamChatCompletionsAsResponses(c, resp, originalModel, customTools, toolSearch, namespaceTools, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
		}
		return s.bufferChatCompletionsAsResponses(c, resp, originalModel, customTools, toolSearch, namespaceTools, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
	}
}

func (s *OpenAIGatewayService) bufferChatCompletionsAsResponses(
	c *gin.Context,
	resp *http.Response,
	originalModel string,
	customTools map[string]bool,
	toolSearch bool,
	namespaceTools map[string]apicompat.NamespacedToolName,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	serviceTier *string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")
	ccResp, usage, err := s.readCCUpstreamJSONResponse(c, resp, writeOpenAIResponsesFallbackError)
	if err != nil {
		return nil, err
	}
	responsesResp := apicompat.ChatCompletionsResponseToResponses(ccResp, originalModel, customTools, toolSearch, namespaceTools)

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.JSON(http.StatusOK, responsesResp)

	return &OpenAIForwardResult{
		RequestID:       requestID,
		Usage:           usage,
		Model:           originalModel,
		BillingModel:    billingModel,
		UpstreamModel:   upstreamModel,
		ReasoningEffort: reasoningEffort,
		ServiceTier:     serviceTier,
		Stream:          false,
		Duration:        time.Since(startTime),
	}, nil
}

func (s *OpenAIGatewayService) streamChatCompletionsAsResponses(
	c *gin.Context,
	resp *http.Response,
	originalModel string,
	customTools map[string]bool,
	toolSearch bool,
	namespaceTools map[string]apicompat.NamespacedToolName,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	serviceTier *string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")
	writeStreamHeaders := s.newStreamHeaderWriter(c, resp.Header)

	state := apicompat.NewChatCompletionsToResponsesStreamState(originalModel)
	state.CustomTools = customTools
	state.ToolSearchDeclared = toolSearch
	state.NamespaceTools = namespaceTools
	clientDisconnected := false

	writeEvents := func(events []apicompat.ResponsesStreamEvent) {
		if clientDisconnected || len(events) == 0 {
			return
		}
		writeStreamHeaders()
		for _, event := range events {
			sse, err := apicompat.ResponsesEventToSSE(event)
			if err != nil {
				logger.L().Warn("openai responses chat fallback: failed to marshal stream event",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				continue
			}
			if _, err := fmt.Fprint(c.Writer, sse); err != nil {
				clientDisconnected = true
				logger.L().Debug("openai responses chat fallback: client disconnected, continuing to drain upstream for billing",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				return
			}
		}
		c.Writer.Flush()
	}

	scan := s.scanCCStream(resp, "openai responses chat fallback", requestID, startTime, func(chunk *apicompat.ChatCompletionsChunk) {
		writeEvents(apicompat.ChatCompletionsChunkToResponsesEvents(chunk, state))
	})

	if scan.Err != nil {
		return &OpenAIForwardResult{
			RequestID:       requestID,
			Usage:           scan.Usage,
			Model:           originalModel,
			BillingModel:    billingModel,
			UpstreamModel:   upstreamModel,
			ReasoningEffort: reasoningEffort,
			ServiceTier:     serviceTier,
			Stream:          true,
			Duration:        time.Since(startTime),
			FirstTokenMs:    scan.FirstTokenMs,
		}, fmt.Errorf("stream usage incomplete: %w", scan.Err)
	}

	writeEvents(apicompat.FinalizeChatCompletionsResponsesStream(state))
	if !clientDisconnected {
		writeStreamHeaders()
		if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
			clientDisconnected = true
		}
		if !clientDisconnected {
			c.Writer.Flush()
		}
	}
	if !scan.SawDone {
		logCCStreamMissingDoneSentinel("openai responses chat fallback", requestID)
	}

	return &OpenAIForwardResult{
		RequestID:       requestID,
		Usage:           scan.Usage,
		Model:           originalModel,
		BillingModel:    billingModel,
		UpstreamModel:   upstreamModel,
		ReasoningEffort: reasoningEffort,
		ServiceTier:     serviceTier,
		Stream:          true,
		Duration:        time.Since(startTime),
		FirstTokenMs:    scan.FirstTokenMs,
	}, nil
}

func chatChunkStartsResponsesOutput(chunk *apicompat.ChatCompletionsChunk) bool {
	if chunk == nil {
		return false
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != nil || choice.Delta.ReasoningContent != nil || len(choice.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}
