//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGeminiTokenProvider_RequestRefreshDisabledDoesNotRefresh(t *testing.T) {
	expiresAt := time.Now().Add(-1 * time.Minute).Format(time.RFC3339)
	account := &Account{
		ID:       9201,
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "rt-must-not-be-used",
			"expires_at":    expiresAt,
		},
	}
	repo := &tokenRefreshAccountRepo{}
	repo.accountsByID = map[int64]*Account{account.ID: account}
	cache := newOpenAITokenCacheStub()
	stub := &tokenRefresherStub{err: errors.New("refresh must not be called")}
	provider := NewGeminiTokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, cache), stub)
	provider.SetRequestRefreshEnabled(false)

	token, err := provider.GetAccessToken(context.Background(), account)
	require.Error(t, err)
	require.Contains(t, err.Error(), "await local credential sync")
	require.Empty(t, token)
	require.Zero(t, stub.calls)
}

func TestGeminiTokenProvider_RequestRefreshDisabledUsesValidAccessTokenWithoutRefresh(t *testing.T) {
	expiresAt := time.Now().Add(geminiTokenRefreshSkew / 2).Format(time.RFC3339)
	account := &Account{
		ID:       9202,
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "still-valid-access",
			"refresh_token": "rt-must-not-be-used",
			"expires_at":    expiresAt,
		},
	}
	repo := &tokenRefreshAccountRepo{}
	repo.accountsByID = map[int64]*Account{account.ID: account}
	cache := newOpenAITokenCacheStub()
	stub := &tokenRefresherStub{err: errors.New("refresh must not be called")}
	provider := NewGeminiTokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, cache), stub)
	provider.SetRequestRefreshEnabled(false)

	token, err := provider.GetAccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "still-valid-access", token)
	require.Zero(t, stub.calls)
}

func TestGeminiTokenProvider_StaleCacheIgnoredWhenCredentialsChanged(t *testing.T) {
	expiresAt := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	account := &Account{
		ID:       9203,
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "new-db-token",
			"expires_at":   expiresAt,
		},
	}
	cache := newOpenAITokenCacheStub()
	cacheKey := GeminiTokenCacheKey(account)
	cache.tokens[cacheKey] = "stale-cached-token"
	provider := NewGeminiTokenProvider(nil, cache, nil)

	token, err := provider.GetAccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "new-db-token", token)
}
