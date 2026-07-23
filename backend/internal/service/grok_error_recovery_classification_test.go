//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClassifyGrokErrorRecoveryFailure_PaymentRequiredWrappedAsBadGateway(t *testing.T) {
	class, code := classifyGrokErrorRecoveryFailure(`error: code=502 reason="GROK_QUOTA_PROBE_UPSTREAM_ERROR" message="upstream returned 402 for probe model \"grok-4.5\": {\"code\":\"personal-team-blocked:spending-limit\"}"`)

	require.Equal(t, grokErrorRecoveryClassPaymentRequired, class)
	require.Equal(t, 402, code)
	require.True(t, isGrokVPSProbeTerminalStatus(code))
}

func TestGrokErrorRecoveryCandidate_PendingVPSProbeBypassesMinReprobeInterval(t *testing.T) {
	now := time.Date(2026, 7, 23, 0, 45, 0, 0, time.UTC)
	acc := &Account{
		ID:           5,
		Platform:     PlatformGrok,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		ErrorMessage: grokHoldUntilSuccessReason,
		Credentials: map[string]any{
			"refresh_token": "rt-pending",
		},
		Extra: map[string]any{
			grokHoldUntilSuccessExtraKey:         true,
			grokVPSProbeRequestedAtExtraKey:      now.Add(-2 * time.Minute).Format(time.RFC3339),
			grokErrorRecoveryLastProbeAtExtraKey: now.Add(-time.Minute).Format(time.RFC3339),
		},
	}

	require.True(t, isGrokErrorRecoveryCandidate(acc, now, 6*time.Hour))
}
