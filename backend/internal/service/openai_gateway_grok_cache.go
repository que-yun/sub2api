package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokConversationIDHeader         = "X-Grok-Conv-Id"
	claudeCodeSessionHeader          = "X-Claude-Code-Session-Id"
	claudeCodeAgentHeader            = "X-Claude-Code-Agent-Id"
	codexTurnMetadataHeader          = "X-Codex-Turn-Metadata"
	codexWindowIDHeader              = "X-Codex-Window-Id"
	grokClientToolCacheOptInHeader   = "X-Sub2API-Grok-Client-Tool-Cache"
	grokFreeCacheNativeToolsJSON     = `[{"type":"web_search"},{"type":"x_search"}]`
	grokFreeCacheDisabledToolChoice  = "none"
	grokClientToolCacheOptInExtraKey = "grok_client_tool_cache_enabled"
	maxCodexTurnMetadataBytes        = 16 << 10
)

// Claude Code metadata.user_id often ends with _session_<uuid>.
var claudeCodeSessionSuffixPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// extractClaudeCodeSessionID resolves the Claude Code conversation id from
// headers or Anthropic/OpenAI-compatible payload metadata.
func extractClaudeCodeSessionID(c *gin.Context, body []byte) string {
	if c != nil {
		if seed := strings.TrimSpace(c.GetHeader(claudeCodeSessionHeader)); seed != "" {
			return seed
		}
	}
	return extractClaudeCodeSessionIDFromPayload(body)
}

func extractClaudeCodeSessionIDFromPayload(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String())
	if userID == "" {
		return ""
	}
	if matches := claudeCodeSessionSuffixPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		return matches[1]
	}
	// Claude Code may embed JSON: {"session_id":"..."}
	if len(userID) > 0 && userID[0] == '{' {
		if sid := strings.TrimSpace(gjson.Get(userID, "session_id").String()); sid != "" {
			return sid
		}
	}
	return ""
}

// resolveGrokCacheIdentity derives one stable, tenant-isolated routing identity
// for xAI's server-side prompt cache. The returned value is safe to expose to
// the upstream: it never contains the client's raw session identifier.
//
// A valid downstream API key is required. This intentionally fails closed on
// internal probes and incomplete request contexts instead of creating a cache
// identity that could be shared by unrelated tenants.
func resolveGrokCacheIdentity(c *gin.Context, body []byte, explicitKey, upstreamModel string) string {
	apiKeyID := getAPIKeyIDFromContext(c)
	if apiKeyID <= 0 {
		return ""
	}
	// /responses/compact rejects tool_choice and does not represent a normal
	// conversation turn. Keep both cache identity and Free-tier routing
	// augmentation out of this path.
	if isOpenAIResponsesCompactPath(c) {
		return ""
	}

	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if model == "" {
		return ""
	}

	seed := explicitGrokCacheSeed(c, body, explicitKey)
	if seed == "" {
		seed = deriveOpenAIStablePrefixSessionSeed(body)
		if seed == "" {
			// A model alone is too broad for cache routing. Preserve the
			// existing first-user-derived identity when no reusable prefix is
			// available so unrelated prompts do not share one tenant-wide key.
			seed = deriveOpenAIAnchoredContentSessionSeed(body)
		}
	}
	if seed == "" {
		return ""
	}

	// generateSessionUUID hashes the whole seed before formatting it as a UUID.
	// Include a versioned namespace so this identity cannot collide with other
	// upstream session identifiers derived by sub2api.
	isolatedSeed := fmt.Sprintf("grok-prompt-cache:v1:%d:%s:%s", apiKeyID, model, seed)
	return generateSessionUUID(isolatedSeed)
}

func explicitGrokCacheSeed(c *gin.Context, body []byte, explicitKey string) string {
	seed := ""
	if c != nil {
		// Claude Code session is the most stable multi-turn identity for
		// /v1/messages → Grok bridges. Prefer it over generic session headers
		// so prompt cache routing matches CPA behavior.
		seed = claudeCodeGrokCacheSeed(c, body)
		if seed == "" && c.Request != nil {
			seed = codexCacheSeedFromHeaders(c.Request.Header)
		}
		if seed == "" {
			seed = explicitOpenAIHeaderSessionID(c)
		}
		if seed == "" {
			seed = strings.TrimSpace(c.GetHeader(grokConversationIDHeader))
		}
	}
	if seed == "" && len(body) > 0 {
		seed = claudeCodeGrokCacheSeedFromPayload(body)
	}
	if seed == "" && len(body) > 0 {
		seed = codexCacheSeedFromPayload(body)
	}
	if seed == "" && len(body) > 0 {
		seed = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	if seed == "" {
		seed = strings.TrimSpace(explicitKey)
	}
	return seed
}

// claudeCodeGrokCacheSeed separates concurrent Claude Code agents that share
// one parent session. The value is later tenant-isolated and hashed before it
// is forwarded to xAI.
func claudeCodeGrokCacheSeed(c *gin.Context, body []byte) string {
	sessionID := extractClaudeCodeSessionID(c, body)
	if sessionID == "" {
		return ""
	}
	agentID := "main"
	if c != nil {
		if value := strings.TrimSpace(c.GetHeader(claudeCodeAgentHeader)); value != "" {
			agentID = value
		}
	}
	return "claude:" + sessionID + ":agent:" + agentID
}

func claudeCodeGrokCacheSeedFromPayload(body []byte) string {
	sessionID := extractClaudeCodeSessionIDFromPayload(body)
	if sessionID == "" {
		return ""
	}
	return "claude:" + sessionID + ":agent:main"
}

// codexCacheSeedFromHeaders extracts the stable Codex prompt-cache identity
// without changing it. Callers decide how the identity is scoped for their
// own routing purpose.
func codexCacheSeedFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if seed := codexCacheSeedFromTurnMetadata(headers.Get(codexTurnMetadataHeader)); seed != "" {
		return seed
	}
	if windowID := strings.TrimSpace(headers.Get(codexWindowIDHeader)); windowID != "" {
		return "codex:window:" + windowID
	}
	return ""
}

func codexCacheSeedFromPayload(body []byte) string {
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.IsObject() {
		return ""
	}
	if turnMetadata := metadata.Get("x-codex-turn-metadata"); turnMetadata.Exists() {
		if seed := codexCacheSeedFromTurnMetadata(turnMetadata.String()); seed != "" {
			return seed
		}
	}
	if windowID := strings.TrimSpace(metadata.Get("x-codex-window-id").String()); windowID != "" {
		return "codex:window:" + windowID
	}
	return ""
}

func codexCacheSeedFromTurnMetadata(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxCodexTurnMetadataBytes {
		return ""
	}
	var metadata struct {
		PromptCacheKey string `json:"prompt_cache_key"`
		WindowID       string `json:"window_id"`
	}
	if json.Unmarshal([]byte(value), &metadata) != nil {
		return ""
	}
	if seed := strings.TrimSpace(metadata.PromptCacheKey); seed != "" {
		return seed
	}
	if windowID := strings.TrimSpace(metadata.WindowID); windowID != "" {
		return "codex:window:" + windowID
	}
	return ""
}

func isGrokRequestContext(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, exists := c.Get("api_key")
	if !exists {
		return false
	}
	apiKey, ok := v.(*APIKey)
	return ok && apiKey != nil && apiKey.Group != nil && apiKey.Group.Platform == PlatformGrok
}

// applyGrokResponsesCacheIdentity writes the cache routing identity into an
// xAI Responses request. Existing client values are deliberately replaced by
// the tenant-isolated value to prevent collisions on shared OAuth accounts.
//
// Free OAuth requests without native search tools are routed by xAI to the
// non-cacheable build-free model. For otherwise tool-free requests, add the
// native tools with tool_choice=none: this selects the cache-capable tier
// without allowing an actual search. Explicit client function tools are handled by
// applyGrokFreeMessagesFunctionToolCacheRoute (Messages bridge and native Responses).
func applyGrokResponsesCacheIdentity(body, intentSourceBody []byte, identity string, injectFreeTierTools bool) ([]byte, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		if gjson.GetBytes(body, "prompt_cache_key").Exists() {
			return sjson.DeleteBytes(body, "prompt_cache_key")
		}
		return body, nil
	}
	out, err := sjson.SetBytes(body, "prompt_cache_key", identity)
	if err != nil {
		return nil, err
	}
	if !injectFreeTierTools {
		return out, nil
	}
	// Inspect the pre-sanitization source. patchGrokResponsesBody may remove an
	// unsupported client tool and its tool_choice; that must not turn an
	// explicit client tool intent into an eligible native-tool request.
	if hasGrokResponsesToolIntent(intentSourceBody) {
		return out, nil
	}
	out, err = sjson.SetRawBytes(out, "tools", []byte(grokFreeCacheNativeToolsJSON))
	if err != nil {
		return nil, err
	}
	return sjson.SetBytes(out, "tool_choice", grokFreeCacheDisabledToolChoice)
}

func hasGrokResponsesToolIntent(body []byte) bool {
	if gjson.GetBytes(body, "tools").Exists() || gjson.GetBytes(body, "tool_choice").Exists() {
		return true
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
			continue
		}
		tools := item.Get("tools")
		if !tools.Exists() || !tools.IsArray() || len(tools.Array()) > 0 {
			return true
		}
	}
	return false
}

// applyGrokFreeMessagesFunctionToolCacheRoute enables xAI's cache-capable
// mixed-tools route only for known Free accounts. Pure client tools default to
// the cache-capable route so an intermediate sub2api does not need to preserve
// client-specific opt-in headers. Operators can explicitly disable this per
// account when native search tools would change the desired behavior (#4486).
// Messages and Responses share this Free-account-only uplift: native
// web_search/x_search become eligible under auto selection.
func applyGrokFreeMessagesFunctionToolCacheRoute(body, intentSourceBody []byte, account *Account, cacheIdentity string) ([]byte, error) {
	allowPureClientTools, _ := grokClientToolCacheAccountPolicy(account)
	return applyGrokFreeToolCacheRoute(body, intentSourceBody, account, cacheIdentity, allowPureClientTools, true)
}

// applyGrokFreeRequestToolCacheRoute also accepts a request-scoped opt-in. The
// sub2api header is consumed locally because buildGrokResponsesRequest only
// forwards the explicitly supported OpenAI-Beta header from downstream.
func applyGrokFreeRequestToolCacheRoute(c *gin.Context, body, intentSourceBody []byte, account *Account, cacheIdentity string, extraFunctionTools ...json.RawMessage) ([]byte, error) {
	allowPureClientTools, accountPolicyExplicit := grokClientToolCacheAccountPolicy(account)
	requestOptOut := false
	if c != nil {
		switch strings.ToLower(strings.TrimSpace(c.GetHeader(grokClientToolCacheOptInHeader))) {
		case "1", "true", "yes", "on", "prefer-cache":
			allowPureClientTools = true
		case "0", "false", "no", "off":
			allowPureClientTools = false
			requestOptOut = true
		}
	}
	if !allowPureClientTools && !accountPolicyExplicit && !requestOptOut && isGrokClaudeDesktopResponsesCacheRequest(c) {
		allowPureClientTools = true
	}
	// Decouple the two levers (like the Messages route): allowPureClientTools
	// governs whether PURE client function tools (e.g. Codex shell/apply_patch)
	// get native web_search/x_search companions — default OFF so grok Build does
	// not web-search instead of running the client's tools. allowFunctionSearch
	// still converts a client-DECLARED web_search/x_search function to native form
	// (genuine search intent), unless the request explicitly opted out.
	allowFunctionSearch := !requestOptOut
	return applyGrokFreeToolCacheRoute(body, intentSourceBody, account, cacheIdentity, allowPureClientTools, allowFunctionSearch, extraFunctionTools...)
}

// grokClientToolCacheAccountPolicy is intentionally strict for configured
// values: only a JSON boolean is accepted. A missing key defaults on solely for
// accounts positively identified as Grok Free OAuth; paid, API-key, and unknown
// accounts remain fail-closed.
func grokClientToolCacheAccountPolicy(account *Account) (enabled, explicit bool) {
	if !isKnownGrokFreeAccount(account) {
		return false, false
	}
	// Default OFF (opt-in). Enabling the pure-client-tool cache route injects
	// grok's native web_search/x_search as companion tools, and grok Build then
	// keeps calling them (ignoring tool_choice) instead of the client's own
	// function tools — coding agents like Codex end up web-searching instead of
	// running shell/apply_patch. This aligns with #4486 (do not bias model tool
	// selection for pure client functions). Set extra.grok_client_tool_cache_enabled=true
	// per account to trade this off for Free-tier prompt caching.
	if account.Extra == nil {
		return false, false
	}
	value, exists := account.Extra[grokClientToolCacheOptInExtraKey]
	if !exists {
		return false, false
	}
	enabled, valid := value.(bool)
	if !valid {
		return false, true
	}
	return enabled, true
}

// isGrokClaudeDesktopResponsesCacheRequest recognizes the strict wire
// fingerprint emitted when Claude Desktop's local agent is translated by
// CC Switch into an OpenAI Responses request. Requiring every independent
// signal prevents a generic Claude-compatible client (or the Chat bridge)
// from silently opting into the mixed native/client tool route.
func isGrokClaudeDesktopResponsesCacheRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil || isOpenAIResponsesCompactPath(c) {
		return false
	}
	path := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	if !strings.HasSuffix(path, "/responses") {
		return false
	}

	if !claudeCodeUAPattern.MatchString(strings.TrimSpace(c.GetHeader("User-Agent"))) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(c.GetHeader("X-App"))) {
	case "cli", "cli-bg":
	default:
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(c.GetHeader("anthropic-client-platform")), "desktop_app") {
		return false
	}
	return strings.TrimSpace(c.GetHeader("X-Claude-Code-Session-Id")) != ""
}

func applyGrokFreeToolCacheRoute(body, intentSourceBody []byte, account *Account, cacheIdentity string, allowPureClientTools, allowFunctionSearch bool, extraFunctionTools ...json.RawMessage) ([]byte, error) {
	if strings.TrimSpace(cacheIdentity) == "" || !isKnownGrokFreeAccount(account) {
		return body, nil
	}
	intentTools := gjson.GetBytes(intentSourceBody, "tools")
	intentToolChoice := gjson.GetBytes(intentSourceBody, "tool_choice")
	if !isGrokFreeCacheFunctionToolIntent(intentTools, intentToolChoice) {
		return body, nil
	}
	if intentToolChoice.Type == gjson.String && strings.TrimSpace(intentToolChoice.String()) == grokFreeCacheDisabledToolChoice {
		// Adding native cache-routing tools cannot change behavior when the
		// client has explicitly disabled all tool execution. Client custom tools
		// are likewise irrelevant here, so they are not re-added.
		return appendGrokFreeCacheNativeToolsWithPolicy(body, true, false)
	}
	return appendGrokFreeCacheNativeToolsWithPolicy(body, allowPureClientTools, allowFunctionSearch, extraFunctionTools...)
}

func isKnownGrokFreeAccount(account *Account) bool {
	if account == nil || !account.IsGrokOAuth() {
		return false
	}
	freeSignal := false
	paidSignal := false
	inferredFreeSignal := false
	if billing, err := grokBillingSnapshotFromExtra(account.Extra); err == nil && billing != nil {
		if tier := strings.TrimSpace(billing.Plan); tier != "" {
			if isGrokFreeSubscriptionTier(tier) {
				freeSignal = true
			} else if !isGrokUnknownSubscriptionTier(tier) {
				paidSignal = true
			}
		}
		if billing.UsagePercent != nil || billing.UsedPercent != nil ||
			(billing.MonthlyLimitCents != nil && *billing.MonthlyLimitCents > 0) {
			paidSignal = true
		}
		// xAI deliberately reports an empty plan for Free accounts; only paid
		// subscriptions receive a SuperGrok plan/monthly limit. A successful
		// monthly billing observation with no paid signal is therefore positive
		// Free evidence, not an unknown tier. Keep partial probes fail-closed.
		if strings.TrimSpace(billing.MonthlyUpdatedAt) != "" ||
			(billing.StatusCode >= http.StatusOK && billing.StatusCode < http.StatusMultipleChoices &&
				!billing.Partial && len(billing.FailedWindows) == 0) {
			inferredFreeSignal = true
		}
	}
	if snapshot, err := grokQuotaSnapshotFromExtra(account.Extra); err == nil && snapshot != nil {
		if tier := strings.TrimSpace(snapshot.SubscriptionTier); tier != "" {
			if isGrokFreeSubscriptionTier(tier) {
				freeSignal = true
			} else if !isGrokUnknownSubscriptionTier(tier) {
				paidSignal = true
			}
		}
		if snapshot.Tokens != nil && snapshot.Tokens.Limit != nil &&
			xai.IsGrokFreeRolling24hTokenLimit(*snapshot.Tokens.Limit) {
			inferredFreeSignal = true
		}
	}
	if tier := strings.TrimSpace(account.GetCredential("subscription_tier")); tier != "" {
		if isGrokFreeSubscriptionTier(tier) {
			freeSignal = true
		} else if !isGrokUnknownSubscriptionTier(tier) {
			paidSignal = true
		}
	}
	// Explicit paid evidence always wins over an inferred Free signal. This
	// protects upgraded/stale accounts whose previous quota snapshot still
	// carries the historical 2M Free token limit.
	return !paidSignal && (freeSignal || inferredFreeSignal)
}

func isGrokFreeSubscriptionTier(tier string) bool {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "free", "grok-free", "grok_free", "free-tier", "free_tier", "basic", "grok-basic", "grok_basic":
		return true
	default:
		return false
	}
}

func isGrokUnknownSubscriptionTier(tier string) bool {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "", "unknown", "n/a", "none":
		return true
	default:
		return false
	}
}

func isGrokFreeCacheFunctionToolIntent(tools, toolChoice gjson.Result) bool {
	if !tools.IsArray() {
		return false
	}
	items := tools.Array()
	if len(items) == 0 {
		return false
	}
	for _, tool := range items {
		if !tool.IsObject() {
			return false
		}
		toolType := strings.TrimSpace(tool.Get("type").String())
		if _, ok := grokResponsesSupportedToolTypes[toolType]; !ok {
			return false
		}
		if toolType == "function" {
			// Responses function declarations keep name at the top level. Reject
			// Chat Completions' nested function shape and incomplete declarations.
			if strings.TrimSpace(tool.Get("name").String()) == "" || tool.Get("function").Exists() {
				return false
			}
		}
	}
	if !toolChoice.Exists() {
		return true
	}
	if toolChoice.Type != gjson.String {
		return false
	}
	// Local agent path lifts empty/auto to "required" so Grok must call tools.
	// That lift is still a free/mixed-cache eligible intent (not a forced single
	// function), so treat required the same as auto for cache routing.
	switch strings.TrimSpace(toolChoice.String()) {
	case "auto", "required", grokFreeCacheDisabledToolChoice:
		return true
	default:
		return false
	}
}

func appendMissingGrokFreeCacheNativeTools(body []byte) ([]byte, error) {
	return appendGrokFreeCacheNativeTools(body, false)
}

func appendGrokFreeCacheNativeTools(body []byte, allowPureClientTools bool) ([]byte, error) {
	return appendGrokFreeCacheNativeToolsWithPolicy(body, allowPureClientTools, true)
}

func appendGrokFreeCacheNativeToolsWithPolicy(body []byte, allowPureClientTools, allowFunctionSearch bool, extraFunctionTools ...json.RawMessage) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body, nil
	}

	items := tools.Array()
	if len(items) == 0 {
		return body, nil
	}
	hasNativeSearch := false
	for _, tool := range items {
		switch strings.TrimSpace(tool.Get("type").String()) {
		case "web_search", "x_search":
			hasNativeSearch = true
		}
	}
	if !allowPureClientTools && !allowFunctionSearch && !hasNativeSearch {
		return body, nil
	}
	merged := make([]json.RawMessage, 0, len(items)+2)
	present := make(map[string]bool, 2)
	hasCompanionTool := false
	for _, tool := range items {
		toolType := strings.TrimSpace(tool.Get("type").String())
		switch toolType {
		case "function":
			name := strings.TrimSpace(tool.Get("name").String())
			if !tool.IsObject() || name == "" || tool.Get("function").Exists() {
				return body, nil
			}
			// Grok Build may declare search as function tools. Convert to native
			// entries so Free OAuth stays cache-capable without duplicate names.
			if (name == "web_search" || name == "x_search") && allowFunctionSearch {
				if present[name] {
					continue
				}
				raw, err := json.Marshal(map[string]string{"type": name})
				if err != nil {
					return nil, err
				}
				merged = append(merged, raw)
				present[name] = true
				if allowPureClientTools {
					hasCompanionTool = true
				}
				continue
			}
			if name == "web_search" || name == "x_search" {
				// Keep the client function intact and avoid adding a same-named
				// native tool unless conversion was explicitly enabled.
				present[name] = true
			}
			hasCompanionTool = true
			merged = append(merged, json.RawMessage(tool.Raw))
		case "web_search", "x_search":
			if present[toolType] {
				continue
			}
			merged = append(merged, json.RawMessage(tool.Raw))
			present[toolType] = true
		default:
			if _, ok := grokResponsesSupportedToolTypes[toolType]; !ok {
				return body, nil
			}
			hasCompanionTool = true
			merged = append(merged, json.RawMessage(tool.Raw))
		}
	}
	if !hasCompanionTool {
		return body, nil
	}
	// Only complement missing native search tools when the request already contains
	// at least one search tool (native or function-form). Pure client function tools
	// (e.g. view_image) must not trigger injection to avoid biasing model tool
	// selection (#4486).
	if !allowPureClientTools && !present["web_search"] && !present["x_search"] {
		return body, nil
	}
	missing := make([]json.RawMessage, 0, 2)
	for _, toolType := range []string{"web_search", "x_search"} {
		if present[toolType] {
			continue
		}
		raw, err := json.Marshal(map[string]string{"type": toolType})
		if err != nil {
			return nil, err
		}
		missing = append(missing, raw)
	}
	if len(missing) > 0 {
		// Keep the web_search/x_search pair adjacent: inject the missing native
		// companion right after the existing search tool instead of at the tail,
		// so it does not drift behind later client tools (e.g. apply_patch) and
		// break the caller's original additional_tools ordering (#4486).
		insertAfter := -1
		for i, raw := range merged {
			switch strings.TrimSpace(gjson.GetBytes(raw, "type").String()) {
			case "web_search", "x_search":
				insertAfter = i
			}
		}
		if insertAfter < 0 {
			merged = append(merged, missing...)
		} else {
			tail := append([]json.RawMessage{}, merged[insertAfter+1:]...)
			merged = append(merged[:insertAfter+1], missing...)
			merged = append(merged, tail...)
		}
	}
	if len(extraFunctionTools) > 0 {
		// Re-add Codex client custom tools (e.g. apply_patch) that the base
		// promotion path dropped. They land after the web_search/x_search pair,
		// preserving the caller's original additional_tools tail order. Dedupe by
		// name so a client tool already present is not duplicated.
		existingNames := make(map[string]struct{}, len(merged))
		for _, raw := range merged {
			name := strings.TrimSpace(gjson.GetBytes(raw, "name").String())
			if name != "" {
				existingNames[name] = struct{}{}
			}
		}
		for _, extra := range extraFunctionTools {
			name := strings.TrimSpace(gjson.GetBytes(extra, "name").String())
			if name == "" {
				continue
			}
			if _, exists := existingNames[name]; exists {
				continue
			}
			existingNames[name] = struct{}{}
			merged = append(merged, extra)
		}
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "tools", encoded)
}

// applyGrokCacheHeaders applies the documented Chat Completions conversation
// routing header. The request is built from a fresh header map, so client
// supplied x-grok headers cannot override this server-derived value.
func applyGrokCacheHeaders(headers http.Header, identity string) {
	if headers == nil {
		return
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		headers.Del(grokConversationIDHeader)
		return
	}
	headers.Set(grokConversationIDHeader, identity)
}

// stripGrokChatPromptCacheKey removes the Responses-only body field after it
// has been used as an identity seed. Chat Completions routes cache by header.
func stripGrokChatPromptCacheKey(body []byte) ([]byte, error) {
	if !gjson.GetBytes(body, "prompt_cache_key").Exists() {
		return body, nil
	}
	return sjson.DeleteBytes(body, "prompt_cache_key")
}
