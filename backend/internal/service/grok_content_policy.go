package service

// Ported from upstream Wei-Shaw/sub2api #4719 (grok_upstream_errors.go):
// content-policy vs account-entitlement 403 分类。仅移植请求级内容策略识别,
// 用于让内容安全 403 直接返回、不误踢/不 failover 账号。
import (
	"encoding/json"
	"net/http"
	"strings"
)

// isGrokContentPolicyRejection identifies request-scoped safety refusals from
// xAI. These failures are caused by the prompt or media, so retrying another
// OAuth account cannot change the outcome and would incorrectly drain a pool.
// Keep this matcher deliberately narrow: account entitlement and suspension
// messages may mention policy but must retain the normal account failover path.
func isGrokContentPolicyRejection(statusCode int, responseBody []byte) bool {
	if statusCode != http.StatusForbidden || len(responseBody) == 0 {
		return false
	}
	if grokAccountAccessMessage(string(responseBody)) {
		return false
	}

	var payload any
	if json.Unmarshal(responseBody, &payload) == nil {
		if grokStructuredAccountAccessMarker(payload) {
			return false
		}
		if grokStructuredContentPolicyMarker(payload) {
			return true
		}
	}

	return grokContentPolicyMessage(string(responseBody))
}

func grokStructuredAccountAccessMarker(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			normalizedKey := normalizeGrokErrorMarker(key)
			switch normalizedKey {
			case "code", "error_code", "type", "category", "reason":
				if marker, ok := child.(string); ok && isGrokAccountAccessCode(marker) {
					return true
				}
			}
			if grokStructuredAccountAccessMarker(child) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if grokStructuredAccountAccessMarker(child) {
				return true
			}
		}
	}
	return false
}

func grokStructuredContentPolicyMarker(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			normalizedKey := normalizeGrokErrorMarker(key)
			switch normalizedKey {
			case "code", "error_code", "type", "category", "reason":
				if marker, ok := child.(string); ok && isGrokContentPolicyCode(marker) {
					return true
				}
			}
			if grokStructuredContentPolicyMarker(child) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if grokStructuredContentPolicyMarker(child) {
				return true
			}
		}
	}
	return false
}

func normalizeGrokErrorMarker(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	value = strings.ReplaceAll(value, " ", "_")
	return value
}

func isGrokContentPolicyCode(value string) bool {
	switch normalizeGrokErrorMarker(value) {
	case "content_filter",
		"content_policy",
		"content_policy_violation",
		"content_moderation",
		"cyber_policy",
		"new_sensitive":
		return true
	default:
		return false
	}
}

func isGrokAccountAccessCode(value string) bool {
	switch normalizeGrokErrorMarker(value) {
	case "account_suspended",
		"account_disabled",
		"user_suspended",
		"user_disabled",
		"subscription_required",
		"entitlement_required",
		"not_entitled",
		"plan_required",
		"permission_denied":
		return true
	default:
		return false
	}
}

func grokAccountAccessMessage(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	for _, phrase := range []string{
		"account suspended",
		"account has been suspended",
		"account disabled",
		"account has been disabled",
		"user suspended",
		"user has been suspended",
		"subscription required",
		"entitlement required",
		"not entitled",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func grokContentPolicyMessage(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return false
	}

	// xAI's media safety responses use these exact phrases. They are specific
	// enough not to classify a generic account-policy or entitlement message.
	for _, phrase := range []string{
		"the moderation feature is not available",
		"image is sensitive",
		"text is sensitive",
		"prohibited content",
		"forbidden content",
		"content policy violation",
		"content policy rejection",
		"content policy rejected",
		"content moderation rejection",
		"content moderation rejected",
		"content moderation blocked",
		"request blocked by content moderation",
		"request rejected by content moderation",
		"request blocked by policy",
		"request rejected by policy",
		"request violates policy",
		"prompt violates content policy",
		"prompt violates policy",
		"input violates content policy",
		"input violates policy",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}

	return false
}
