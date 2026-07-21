//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// setup-token 是可刷新凭证，401 不得永久 SetError；
// VPS request-refresh-off 时应标记 Pending local credential sync。
func TestRateLimitService_HandleUpstreamError_AnthropicSetupToken401TempUnschedulable(t *testing.T) {
	t.Run("local_authority_refresh_enabled", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		invalidator := &tokenCacheInvalidatorRecorder{}
		cfg := &config.Config{
			TokenRefresh: config.TokenRefreshConfig{RequestRefreshEnabled: true},
			RateLimit:    config.RateLimitConfig{OAuth401CooldownMinutes: 10},
		}
		service := NewRateLimitService(repo, nil, cfg, nil, nil)
		service.SetTokenCacheInvalidator(invalidator)
		account := &Account{
			ID:       21330,
			Platform: PlatformAnthropic,
			Type:     AccountTypeSetupToken,
			Credentials: map[string]any{
				"refresh_token": "rt-setup",
				"access_token":  "at-old",
			},
		}

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte(`{"error":{"type":"authentication_error","message":"invalid_token"}}`))

		require.True(t, shouldDisable)
		require.Equal(t, 0, repo.setErrorCalls, "setup-token 401 must not permanent SetError")
		require.Equal(t, 1, repo.tempCalls)
		require.Equal(t, int64(21330), repo.lastTempID)
		require.Contains(t, repo.lastTempReason, "OAuth 401")
		require.Len(t, invalidator.accounts, 1)
	})

	t.Run("vps_request_refresh_off_pending_local_sync", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		invalidator := &tokenCacheInvalidatorRecorder{}
		cfg := &config.Config{
			TokenRefresh: config.TokenRefreshConfig{RequestRefreshEnabled: false},
			RateLimit:    config.RateLimitConfig{OAuth401CooldownMinutes: 5},
		}
		service := NewRateLimitService(repo, nil, cfg, nil, nil)
		service.SetTokenCacheInvalidator(invalidator)
		account := &Account{
			ID:       21330,
			Platform: PlatformAnthropic,
			Type:     AccountTypeSetupToken,
			Credentials: map[string]any{
				"refresh_token": "rt-setup",
				"access_token":  "at-stale",
			},
		}

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`))

		require.True(t, shouldDisable)
		require.Equal(t, 0, repo.setErrorCalls, "VPS setup-token 401 must not permanent SetError")
		require.Equal(t, 1, repo.tempCalls)
		require.Contains(t, repo.lastTempReason, "Pending local credential sync")
		require.Len(t, invalidator.accounts, 1)
	})
}
