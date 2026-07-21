//go:build unit

package xai

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseQuotaHeaders(t *testing.T) {
	t.Parallel()

	headers := http.Header{}
	headers.Set("x-ratelimit-limit-requests", "100")
	headers.Set("x-ratelimit-remaining-requests", "25")
	headers.Set("x-ratelimit-reset-requests", "1893456000")
	headers.Set("x-ratelimit-limit-tokens", "1000000")
	headers.Set("x-ratelimit-remaining-tokens", "750000")
	headers.Set("retry-after", "60")
	headers.Set("xai-subscription-tier", "supergrok")
	headers.Set("xai-entitlement-status", "active")
	headers.Set("authorization", "should-not-be-copied")

	snapshot := ParseQuotaHeaders(headers, http.StatusTooManyRequests)
	require.NotNil(t, snapshot)
	require.Equal(t, http.StatusTooManyRequests, snapshot.StatusCode)
	require.True(t, snapshot.HeadersObserved)
	require.NotEmpty(t, snapshot.LastHeadersSeenAt)
	require.Equal(t, int64(100), *snapshot.Requests.Limit)
	require.Equal(t, int64(25), *snapshot.Requests.Remaining)
	require.Equal(t, int64(1893456000), *snapshot.Requests.ResetUnix)
	require.Equal(t, "2030-01-01T00:00:00Z", snapshot.Requests.ResetAt)
	require.Equal(t, int64(1000000), *snapshot.Tokens.Limit)
	require.Equal(t, int64(750000), *snapshot.Tokens.Remaining)
	require.Equal(t, 60, *snapshot.RetryAfterSeconds)
	require.Equal(t, "supergrok", snapshot.SubscriptionTier)
	require.Equal(t, "active", snapshot.EntitlementStatus)
	require.Contains(t, snapshot.Headers, "x-ratelimit-limit-requests")
	require.NotContains(t, snapshot.Headers, "authorization")
}

func TestParseQuotaHeadersReturnsNilForMissingHeaders(t *testing.T) {
	t.Parallel()

	require.Nil(t, ParseQuotaHeaders(http.Header{}, http.StatusOK))
}

func TestObserveQuotaHeadersRecordsNoHeaderProbe(t *testing.T) {
	t.Parallel()

	snapshot := ObserveQuotaHeaders(http.Header{}, http.StatusOK, "active_probe")
	require.NotNil(t, snapshot)
	require.False(t, snapshot.HeadersObserved)
	require.Equal(t, http.StatusOK, snapshot.StatusCode)
	require.Equal(t, "active_probe", snapshot.ObservationSource)
	require.NotEmpty(t, snapshot.LastProbeAt)
	require.Empty(t, snapshot.LastHeadersSeenAt)
	require.Empty(t, snapshot.Headers)
	require.Nil(t, snapshot.Requests)
	require.Nil(t, snapshot.Tokens)
}


func TestParseFreeUsageExhaustedBody(t *testing.T) {
	t.Parallel()

	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1033696/1000000. Upgrade to a Grok subscription for higher limits: https://grok.com/supergrok"}`)
	info := ParseFreeUsageExhaustedBody(body)
	require.NotNil(t, info)
	require.True(t, info.Exhausted)
	require.Equal(t, FreeUsageErrorCodeExhausted, info.ErrorCode)
	require.Equal(t, FreeUsageWindowRolling24h, info.Window)
	require.Equal(t, 24*time.Hour, info.Cooldown)
	require.Equal(t, "grok-4.5-build-free", info.Model)
	require.NotNil(t, info.ActualTokens)
	require.Equal(t, int64(1033696), *info.ActualTokens)
	require.NotNil(t, info.LimitTokens)
	require.Equal(t, int64(1000000), *info.LimitTokens)
}

func TestResolveGrokCooldownFreeUsageExhausted(t *testing.T) {
	t.Parallel()

	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1033696/1000000."}`)
	headers := http.Header{}
	headers.Set("x-ratelimit-limit-tokens", "1000000")
	headers.Set("x-ratelimit-remaining-tokens", "0")
	cooldown, reason, freeInfo, snapshot := ResolveGrokCooldown(http.StatusTooManyRequests, headers, body)
	require.NotNil(t, freeInfo)
	require.True(t, freeInfo.Exhausted)
	require.Equal(t, 24*time.Hour, cooldown)
	require.Contains(t, reason, "free usage exhausted")
	require.NotNil(t, snapshot)
	require.True(t, snapshot.FreeUsageExhausted)
	require.Equal(t, FreeUsageWindowRolling24h, snapshot.FreeUsageWindow)
}

func TestResolveGrokCooldownPlainRateLimit(t *testing.T) {
	t.Parallel()

	headers := http.Header{}
	headers.Set("Retry-After", "45")
	cooldown, reason, freeInfo, _ := ResolveGrokCooldown(http.StatusTooManyRequests, headers, []byte(`{"error":"rate limited"}`))
	require.Nil(t, freeInfo)
	require.Equal(t, 45*time.Second, cooldown)
	require.Equal(t, "grok rate limited", reason)
}

func TestIsGrokFreeRolling24hTokenLimit(t *testing.T) {
	t.Parallel()

	require.True(t, IsGrokFreeRolling24hTokenLimit(GrokFreeRolling24hTokenLimit))
	require.True(t, IsGrokFreeRolling24hTokenLimit(2_000_000), "legacy snapshots remain classifiable")
	require.False(t, IsGrokFreeRolling24hTokenLimit(3_000_000))

}
