package xai

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const GrokFreeRolling24hTokenLimit int64 = 1_000_000

var grokFreeRolling24hTokenLimits = map[int64]struct{}{
	GrokFreeRolling24hTokenLimit: {},
	2_000_000:                    {}, // Legacy Free limit observed before July 2026.
}

func IsGrokFreeRolling24hTokenLimit(limit int64) bool {
	_, ok := grokFreeRolling24hTokenLimits[limit]
	return ok
}

type QuotaWindow struct {
	Limit     *int64 `json:"limit,omitempty"`
	Remaining *int64 `json:"remaining,omitempty"`
	ResetUnix *int64 `json:"reset_unix,omitempty"`
	ResetAt   string `json:"reset_at,omitempty"`
}

type QuotaSnapshot struct {
	Requests          *QuotaWindow      `json:"requests,omitempty"`
	Tokens            *QuotaWindow      `json:"tokens,omitempty"`
	RetryAfterSeconds *int              `json:"retry_after_seconds,omitempty"`
	SubscriptionTier  string            `json:"subscription_tier,omitempty"`
	EntitlementStatus string            `json:"entitlement_status,omitempty"`
	// Free Build rolling-window fields (parsed from free-usage-exhausted body or rate headers).
	FreeUsageExhausted     bool   `json:"free_usage_exhausted,omitempty"`
	FreeUsageErrorCode     string `json:"free_usage_error_code,omitempty"`
	FreeUsageModel         string `json:"free_usage_model,omitempty"`
	FreeUsageWindow        string `json:"free_usage_window,omitempty"` // e.g. rolling_24h
	FreeUsageActualTokens  *int64 `json:"free_usage_actual_tokens,omitempty"`
	FreeUsageLimitTokens   *int64 `json:"free_usage_limit_tokens,omitempty"`
	FreeUsageCooldownUntil string `json:"free_usage_cooldown_until,omitempty"`
	StatusCode        int               `json:"status_code,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	HeadersObserved   bool              `json:"headers_observed"`
	ObservationSource string            `json:"observation_source,omitempty"`
	LastProbeAt       string            `json:"last_probe_at,omitempty"`
	LastHeadersSeenAt string            `json:"last_headers_seen_at,omitempty"`
	UpdatedAt         string            `json:"updated_at"`
}

func (s *QuotaSnapshot) HasObservedHeaders() bool {
	if s == nil {
		return false
	}
	return s.HeadersObserved ||
		s.Requests != nil ||
		s.Tokens != nil ||
		s.RetryAfterSeconds != nil ||
		s.SubscriptionTier != "" ||
		s.EntitlementStatus != "" ||
		len(s.Headers) > 0
}

var quotaHeaderAllowlist = []string{
	"x-ratelimit-limit-requests",
	"x-ratelimit-remaining-requests",
	"x-ratelimit-reset-requests",
	"x-ratelimit-limit-tokens",
	"x-ratelimit-remaining-tokens",
	"x-ratelimit-reset-tokens",
	"retry-after",
	"x-subscription-tier",
	"xai-subscription-tier",
	"x-entitlement-status",
	"xai-entitlement-status",
}

func ParseQuotaHeaders(headers http.Header, statusCode int) *QuotaSnapshot {
	return parseQuotaHeaders(headers, statusCode, "", false)
}

func ObserveQuotaHeaders(headers http.Header, statusCode int, source string) *QuotaSnapshot {
	return parseQuotaHeaders(headers, statusCode, source, true)
}

func parseQuotaHeaders(headers http.Header, statusCode int, source string, keepEmpty bool) *QuotaSnapshot {
	if headers == nil && !keepEmpty {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	snapshot := &QuotaSnapshot{
		Requests:          parseQuotaWindow(headers, "requests"),
		Tokens:            parseQuotaWindow(headers, "tokens"),
		StatusCode:        statusCode,
		Headers:           make(map[string]string),
		ObservationSource: strings.TrimSpace(source),
		UpdatedAt:         now,
	}
	if snapshot.ObservationSource == "active_probe" {
		snapshot.LastProbeAt = now
	}
	if retryAfter := parseRetryAfter(headers.Get("retry-after")); retryAfter != nil {
		snapshot.RetryAfterSeconds = retryAfter
	}
	snapshot.SubscriptionTier = firstHeader(headers, "xai-subscription-tier", "x-subscription-tier")
	snapshot.EntitlementStatus = firstHeader(headers, "xai-entitlement-status", "x-entitlement-status")

	for _, name := range quotaHeaderAllowlist {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			snapshot.Headers[name] = value
		}
	}

	if snapshot.Requests == nil &&
		snapshot.Tokens == nil &&
		snapshot.RetryAfterSeconds == nil &&
		snapshot.SubscriptionTier == "" &&
		snapshot.EntitlementStatus == "" &&
		len(snapshot.Headers) == 0 {
		if keepEmpty {
			return snapshot
		}
		return nil
	}
	snapshot.HeadersObserved = true
	snapshot.LastHeadersSeenAt = now
	return snapshot
}

func parseQuotaWindow(headers http.Header, dimension string) *QuotaWindow {
	window := &QuotaWindow{
		Limit:     parseInt64Ptr(headers.Get("x-ratelimit-limit-" + dimension)),
		Remaining: parseInt64Ptr(headers.Get("x-ratelimit-remaining-" + dimension)),
	}
	if reset := parseResetHeader(headers.Get("x-ratelimit-reset-" + dimension)); reset != nil {
		window.ResetUnix = reset
		window.ResetAt = time.Unix(*reset, 0).UTC().Format(time.RFC3339)
	}
	if window.Limit == nil && window.Remaining == nil && window.ResetUnix == nil {
		return nil
	}
	return window
}

func parseResetHeader(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if value > 1_000_000_000_000 {
			value = value / 1000
		}
		return &value
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		value := t.Unix()
		return &value
	}
	return nil
}

func parseRetryAfter(raw string) *int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if value, err := strconv.Atoi(raw); err == nil {
		return &value
	}
	if t, err := http.ParseTime(raw); err == nil {
		seconds := int(time.Until(t).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		return &seconds
	}
	return nil
}

func parseInt64Ptr(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &value
}

func firstHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}


// Free Build usage is a rolling window (currently 24h) and is only fully described
// in the 429 body for subscription:free-usage-exhausted. Success responses expose
// x-ratelimit-limit/remaining-* but usually omit reset timestamps on cli-chat-proxy.
const (
	FreeUsageErrorCodeExhausted = "subscription:free-usage-exhausted"
	FreeUsageWindowRolling24h   = "rolling_24h"
	DefaultFreeUsageCooldown    = 24 * time.Hour
)

var (
	freeUsageTokenPairRe = regexp.MustCompile(`(?i)tokens\s*\(actual/limit\)\s*:\s*(\d+)\s*/\s*(\d+)`)
	freeUsageRollingRe   = regexp.MustCompile(`(?i)rolling\s+(\d+)\s*-?\s*hour`)
	freeUsageModelRe     = regexp.MustCompile(`(?i)model\s+([a-z0-9._-]+-build-free)`)
)

// FreeUsageInfo is the structured free-tier exhaustion signal from xAI.
type FreeUsageInfo struct {
	Exhausted     bool
	ErrorCode     string
	Model         string
	Window        string
	ActualTokens  *int64
	LimitTokens   *int64
	Cooldown       time.Duration
	Source        string // body|headers
	RawMessage    string
}

func (info *FreeUsageInfo) ApplyToSnapshot(snapshot *QuotaSnapshot, now time.Time) {
	if info == nil || snapshot == nil || !info.Exhausted {
		return
	}
	snapshot.FreeUsageExhausted = true
	if info.ErrorCode != "" {
		snapshot.FreeUsageErrorCode = info.ErrorCode
	}
	if info.Model != "" {
		snapshot.FreeUsageModel = info.Model
	}
	if info.Window != "" {
		snapshot.FreeUsageWindow = info.Window
	}
	if info.ActualTokens != nil {
		snapshot.FreeUsageActualTokens = info.ActualTokens
	}
	if info.LimitTokens != nil {
		snapshot.FreeUsageLimitTokens = info.LimitTokens
	}
	if info.Cooldown > 0 {
		snapshot.FreeUsageCooldownUntil = now.UTC().Add(info.Cooldown).Format(time.RFC3339)
	}
	if snapshot.EntitlementStatus == "" {
		snapshot.EntitlementStatus = "free_usage_exhausted"
	}
}

// ParseFreeUsageExhaustedBody extracts rolling free-tier exhaustion from 429 JSON/text.
// Example body:
// {"code":"subscription:free-usage-exhausted","error":"... rolling 24-hour window — tokens (actual/limit): 1033696/1000000 ..."}
func ParseFreeUsageExhaustedBody(body []byte) *FreeUsageInfo {
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return nil
	}

	code := ""
	message := raw
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		if v, ok := payload["code"].(string); ok {
			code = strings.TrimSpace(v)
		}
		if v, ok := payload["error"].(string); ok && strings.TrimSpace(v) != "" {
			message = strings.TrimSpace(v)
		} else if errObj, ok := payload["error"].(map[string]any); ok {
			if v, ok := errObj["message"].(string); ok && strings.TrimSpace(v) != "" {
				message = strings.TrimSpace(v)
			}
			if v, ok := errObj["code"].(string); ok && code == "" {
				code = strings.TrimSpace(v)
			}
		}
		if v, ok := payload["message"].(string); ok && message == raw {
			message = strings.TrimSpace(v)
		}
	}

	lowerMsg := strings.ToLower(message)
	lowerCode := strings.ToLower(code)
	isFreeExhausted := strings.Contains(lowerCode, "free-usage-exhausted") ||
		strings.Contains(lowerMsg, "free-usage-exhausted") ||
		strings.Contains(lowerMsg, "used all the included free usage") ||
		(strings.Contains(lowerMsg, "free usage") && strings.Contains(lowerMsg, "resets over a rolling"))
	if !isFreeExhausted {
		return nil
	}

	info := &FreeUsageInfo{
		Exhausted:  true,
		ErrorCode:  firstNonEmptyString(code, FreeUsageErrorCodeExhausted),
		Window:     FreeUsageWindowRolling24h,
		Cooldown:   DefaultFreeUsageCooldown,
		Source:     "body",
		RawMessage: message,
	}

	if m := freeUsageRollingRe.FindStringSubmatch(message); len(m) == 2 {
		if hours, err := strconv.Atoi(m[1]); err == nil && hours > 0 {
			info.Window = "rolling_" + m[1] + "h"
			info.Cooldown = time.Duration(hours) * time.Hour
		}
	}
	if m := freeUsageTokenPairRe.FindStringSubmatch(message); len(m) == 3 {
		if actual, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			info.ActualTokens = &actual
		}
		if limit, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			info.LimitTokens = &limit
		}
	}
	if m := freeUsageModelRe.FindStringSubmatch(message); len(m) == 2 {
		info.Model = m[1]
	}
	return info
}

// DetectFreeUsageExhaustedFromHeaders treats remaining<=0 on free rate-limit headers as exhausted.
// Useful when upstream 429 body is missing but remaining counters already hit zero.
func DetectFreeUsageExhaustedFromHeaders(headers http.Header) *FreeUsageInfo {
	if headers == nil {
		return nil
	}
	reqRem := parseInt64Ptr(headers.Get("x-ratelimit-remaining-requests"))
	tokRem := parseInt64Ptr(headers.Get("x-ratelimit-remaining-tokens"))
	tokLimit := parseInt64Ptr(headers.Get("x-ratelimit-limit-tokens"))
	reqLimit := parseInt64Ptr(headers.Get("x-ratelimit-limit-requests"))

	exhausted := false
	if tokRem != nil && *tokRem <= 0 {
		exhausted = true
	}
	if reqRem != nil && *reqRem <= 0 {
		exhausted = true
	}
	// free pool commonly uses 1e6 token window; overshoot actual is only in body, but remaining=0 is enough.
	if !exhausted {
		return nil
	}
	info := &FreeUsageInfo{
		Exhausted:    true,
		ErrorCode:    FreeUsageErrorCodeExhausted,
		Window:       FreeUsageWindowRolling24h,
		Cooldown:      DefaultFreeUsageCooldown,
		Source:       "headers",
		ActualTokens: nil,
		LimitTokens:  tokLimit,
	}
	if tokLimit != nil && tokRem != nil {
		// remaining=0 => actual roughly at/over limit; keep limit only.
		_ = reqLimit
	}
	return info
}

// ResolveGrokCooldown picks account cooldown for a Grok upstream error.
// free-usage-exhausted uses rolling window (default 24h); plain 429 uses Retry-After or 2m.
func ResolveGrokCooldown(statusCode int, headers http.Header, body []byte) (cooldown time.Duration, reason string, freeInfo *FreeUsageInfo, snapshot *QuotaSnapshot) {
	snapshot = ParseQuotaHeaders(headers, statusCode)
	freeInfo = ParseFreeUsageExhaustedBody(body)
	if freeInfo == nil {
		freeInfo = DetectFreeUsageExhaustedFromHeaders(headers)
	}
	now := time.Now()
	if freeInfo != nil && freeInfo.Exhausted {
		if snapshot == nil {
			snapshot = &QuotaSnapshot{StatusCode: statusCode, UpdatedAt: now.UTC().Format(time.RFC3339)}
		}
		freeInfo.ApplyToSnapshot(snapshot, now)
		cooldown = freeInfo.Cooldown
		if cooldown <= 0 {
			cooldown = DefaultFreeUsageCooldown
		}
		return cooldown, "grok free usage exhausted (rolling window)", freeInfo, snapshot
	}

	if statusCode == http.StatusTooManyRequests {
		cooldown = 2 * time.Minute
		if snapshot != nil && snapshot.RetryAfterSeconds != nil && *snapshot.RetryAfterSeconds > 0 {
			cooldown = time.Duration(*snapshot.RetryAfterSeconds) * time.Second
		}
		return cooldown, "grok rate limited", freeInfo, snapshot
	}
	return 0, "", freeInfo, snapshot
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
