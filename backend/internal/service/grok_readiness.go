package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

const (
	// grokHoldUntilSuccessExtraKey marks Grok accounts that must not re-enter
	// scheduling after entitlement/permission 403 until a real success clears it.
	// Temporary cooldown alone is not enough: operators need an explicit success
	// signal instead of silent auto-resume when the timer expires.
	grokHoldUntilSuccessExtraKey = "grok_hold_until_success"
	grokHoldUntilSuccessReason   = "grok entitlement or subscription tier denied"
	// grokPaymentRequiredReason marks Grok accounts sticky-blocked after an
	// upstream 402 (payment required / spending-limit). Same hold-until-success
	// lifecycle as an entitlement 403, kept as a distinct reason so operators can
	// tell billing blocks apart from permission revocations in diagnostics.
	grokPaymentRequiredReason = "grok payment required or spending limit (402)"
)

// GrokAccountRequiresSuccessBeforeSchedule reports whether a Grok account is
// sticky-blocked after a real permission/entitlement 403.
func GrokAccountRequiresSuccessBeforeSchedule(account *Account) bool {
	if account == nil || account.Platform != PlatformGrok {
		return false
	}
	if asBool(account.Extra[grokHoldUntilSuccessExtraKey]) {
		return true
	}
	if asBool(account.Extra["grok_free_usage_exhausted"]) {
		return false
	}
	snapshot, ok := account.Extra["grok_usage_snapshot"].(map[string]any)
	if !ok {
		return false
	}
	if strings.TrimSpace(snapshotString(snapshot, "observation_source")) != "active_probe" {
		return false
	}
	code := strings.TrimSpace(snapshotString(snapshot, "status_code"))
	return code == "402" || code == "403"
}

func snapshotString(snapshot map[string]any, key string) string {
	if value, ok := snapshot[key]; ok {
		return strings.TrimSpace(fmt.Sprint(value))
	}
	return ""
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes"
	default:
		return false
	}
}

// ClearGrokHoldAfterSuccess clears sticky entitlement holds and temporary
// unschedulable state after a proven successful Grok request/test/probe.
func ClearGrokHoldAfterSuccess(ctx context.Context, repo AccountRepository, rateLimit *RateLimitService, account *Account) {
	if repo == nil || account == nil || account.ID <= 0 || account.Platform != PlatformGrok {
		return
	}
	needClearTemp := account.TempUnschedulableUntil != nil || strings.TrimSpace(account.TempUnschedulableReason) != ""
	needClearHold := GrokAccountRequiresSuccessBeforeSchedule(account)
	// Proven success should also recover accounts that were marked error by a prior 403.
	needClearError := account.Status == StatusError || strings.TrimSpace(account.ErrorMessage) != "" || !account.Schedulable
	if !needClearTemp && !needClearHold && !needClearError {
		// Still reset 403 counter on success when rate-limit service is present.
		if rateLimit != nil {
			rateLimit.ResetOpenAI403Counter(ctx, account.ID)
		}
		return
	}
	if needClearTemp {
		if rateLimit != nil {
			_ = rateLimit.ClearTempUnschedulable(ctx, account.ID)
		} else {
			_ = repo.ClearTempUnschedulable(ctx, account.ID)
		}
		account.TempUnschedulableUntil = nil
		account.TempUnschedulableReason = ""
	}
	if needClearHold {
		_ = repo.UpdateExtra(ctx, account.ID, map[string]any{
			grokHoldUntilSuccessExtraKey: false,
		})
		if account.Extra == nil {
			account.Extra = map[string]any{}
		}
		account.Extra[grokHoldUntilSuccessExtraKey] = false
	}
	if needClearError {
		_ = repo.ClearError(ctx, account.ID)
		_ = repo.SetSchedulable(ctx, account.ID, true)
		account.Status = StatusActive
		account.ErrorMessage = ""
		account.Schedulable = true
	}
	if rateLimit != nil {
		rateLimit.ResetOpenAI403Counter(ctx, account.ID)
	}
}

// MarkGrokHoldUntilSuccess sticky-blocks a Grok account until success.
func MarkGrokHoldUntilSuccess(ctx context.Context, repo AccountRepository, account *Account) {
	if repo == nil || account == nil || account.ID <= 0 {
		return
	}
	_ = repo.UpdateExtra(ctx, account.ID, map[string]any{
		grokHoldUntilSuccessExtraKey: true,
	})
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	account.Extra[grokHoldUntilSuccessExtraKey] = true
}

// IsGrokPermissionDeniedBody reports whether an upstream body is a real
// chat entitlement/permission denial rather than free-usage exhaustion.
func IsGrokPermissionDeniedBody(body []byte) bool {
	if freeInfo := xai.ParseFreeUsageExhaustedBody(body); freeInfo != nil && freeInfo.Exhausted {
		return false
	}
	low := strings.ToLower(string(body))
	if strings.Contains(low, "permission-denied") {
		return true
	}
	if strings.Contains(low, "access to the chat endpoint is denied") {
		return true
	}
	if strings.Contains(low, "entitlement") && (strings.Contains(low, "denied") || strings.Contains(low, "subscription")) {
		return true
	}
	if strings.Contains(low, "subscription tier") {
		return true
	}
	return false
}

// MarkGrokTokenAcquisitionFailure persists unrecoverable OAuth token failures
// seen by admin test / explicit health checks. Recoverable transport noise is
// left untouched so a single blip does not kill the account.
func MarkGrokTokenAcquisitionFailure(ctx context.Context, repo AccountRepository, account *Account, err error) {
	if repo == nil || account == nil || account.ID <= 0 || err == nil {
		return
	}
	msg := strings.ToLower(err.Error())
	permanent := strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "refresh token has been revoked") ||
		(strings.Contains(msg, "refresh_token") && strings.Contains(msg, "revoked")) ||
		strings.Contains(msg, "oauth refresh account state changed") ||
		strings.Contains(msg, "account state changed") ||
		strings.Contains(msg, "token_expired") ||
		strings.Contains(msg, "app_session_terminated") ||
		strings.Contains(msg, "access token is missing") ||
		strings.Contains(msg, "refresh token is missing") ||
		strings.Contains(msg, "no refresh token")
	if !permanent {
		until := time.Now().Add(10 * time.Minute)
		_ = repo.SetTempUnschedulable(ctx, account.ID, until, "grok oauth token acquisition failed")
		account.TempUnschedulableUntil = &until
		account.TempUnschedulableReason = "grok oauth token acquisition failed"
		return
	}
	errMsg := "oauth refresh account state changed / token reauth required"
	if strings.Contains(msg, "invalid_grant") || strings.Contains(msg, "revoked") {
		errMsg = "oauth refresh token revoked / reauth required"
	}
	_ = repo.SetError(ctx, account.ID, errMsg)
	account.Status = StatusError
	account.Schedulable = false
	account.ErrorMessage = errMsg
	account.TempUnschedulableUntil = nil
	account.TempUnschedulableReason = ""
}

// ApplyGrokProbeOrTestStatus applies the same readiness side effects that
// production traffic uses when an admin probe/test hits upstream.
//
// Rules:
//   - 401/403/429 readiness failures must remove the account from scheduling
//   - true permission-denied 403 is sticky until success (no silent resume)
//   - free-usage-exhausted remains recoverable rate-limit cooldown
//   - HTTP 200 clears sticky hold / temp unschedulable
func ApplyGrokProbeOrTestStatus(
	ctx context.Context,
	repo AccountRepository,
	rateLimit *RateLimitService,
	account *Account,
	statusCode int,
	headers http.Header,
	body []byte,
	source string,
) {
	if repo == nil || account == nil || account.Platform != PlatformGrok {
		return
	}
	if statusCode == http.StatusOK {
		ClearGrokHoldAfterSuccess(ctx, repo, rateLimit, account)
		return
	}

	now := time.Now()
	// Prefer free-usage semantics even when xAI returns it as 403.
	if freeInfo := xai.ParseFreeUsageExhaustedBody(body); freeInfo != nil && freeInfo.Exhausted {
		cooldown := freeInfo.Cooldown
		if cooldown <= 0 {
			cooldown = xai.DefaultFreeUsageCooldown
		}
		until := now.Add(cooldown)
		if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(until) {
			until = *account.RateLimitResetAt
		}
		_ = repo.SetRateLimited(ctx, account.ID, until)
		account.RateLimitResetAt = &until
		account.RateLimitedAt = &now
		extra := map[string]any{
			"grok_free_usage_exhausted":      true,
			"grok_free_usage_error_code":     freeInfo.ErrorCode,
			"grok_free_usage_window":         freeInfo.Window,
			"grok_free_usage_model":          freeInfo.Model,
			"grok_free_usage_cooldown_until": until.UTC().Format(time.RFC3339),
		}
		if freeInfo.ActualTokens != nil {
			extra["grok_free_usage_actual_tokens"] = *freeInfo.ActualTokens
		}
		if freeInfo.LimitTokens != nil {
			extra["grok_free_usage_limit_tokens"] = *freeInfo.LimitTokens
		}
		_ = repo.UpdateExtra(ctx, account.ID, extra)
		return
	}

	switch statusCode {
	case http.StatusPaymentRequired:
		// xAI returns a non-free-usage 402 when the account has no usable
		// entitlement. It is not a rate limit: keep it out of scheduling until
		// a later real chat success proves that the entitlement has recovered.
		_ = repo.SetError(ctx, account.ID, grokPaymentRequiredReason)
		account.Status = StatusError
		account.Schedulable = false
		account.ErrorMessage = grokPaymentRequiredReason
		account.TempUnschedulableUntil = nil
		account.TempUnschedulableReason = ""
		MarkGrokHoldUntilSuccess(ctx, repo, account)
	case http.StatusUnauthorized:
		until := now.Add(10 * time.Minute)
		if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(until) {
			until = *account.TempUnschedulableUntil
		}
		_ = repo.SetTempUnschedulable(ctx, account.ID, until, "grok oauth token unauthorized")
		account.TempUnschedulableUntil = &until
		account.TempUnschedulableReason = "grok oauth token unauthorized"
	case http.StatusForbidden:
		// Admin probes/tests are explicit health checks. A non-free-usage 403 means
		// the Grok chat endpoint is not available for this account, so persist it
		// as error instead of leaving it as active with a sticky temporary hold.
		_ = repo.SetError(ctx, account.ID, grokHoldUntilSuccessReason)
		account.Status = StatusError
		account.Schedulable = false
		account.ErrorMessage = grokHoldUntilSuccessReason
		account.TempUnschedulableUntil = nil
		account.TempUnschedulableReason = ""
		MarkGrokHoldUntilSuccess(ctx, repo, account)
		_ = source // reserved for future per-source metrics/logging
	case http.StatusTooManyRequests:
		cooldown, reason, freeInfo, _ := xai.ResolveGrokCooldown(statusCode, headers, body)
		if cooldown <= 0 {
			cooldown = 2 * time.Minute
		}
		if strings.TrimSpace(reason) == "" {
			reason = "grok rate limited"
		}
		until := now.Add(cooldown)
		if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(until) {
			until = *account.RateLimitResetAt
		}
		_ = repo.SetRateLimited(ctx, account.ID, until)
		account.RateLimitResetAt = &until
		account.RateLimitedAt = &now
		if freeInfo != nil && freeInfo.Exhausted {
			extra := map[string]any{
				"grok_free_usage_exhausted":      true,
				"grok_free_usage_error_code":     freeInfo.ErrorCode,
				"grok_free_usage_window":         freeInfo.Window,
				"grok_free_usage_model":          freeInfo.Model,
				"grok_free_usage_cooldown_until": until.UTC().Format(time.RFC3339),
			}
			if freeInfo.ActualTokens != nil {
				extra["grok_free_usage_actual_tokens"] = *freeInfo.ActualTokens
			}
			if freeInfo.LimitTokens != nil {
				extra["grok_free_usage_limit_tokens"] = *freeInfo.LimitTokens
			}
			_ = repo.UpdateExtra(ctx, account.ID, extra)
		}
		_ = reason
	}
}
