//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

type grokOAuthClientStub struct {
	refreshResponse *xai.TokenResponse
	exchangeCalls   int
}

func (s *grokOAuthClientStub) ExchangeCode(context.Context, string, string, string, string, string) (*xai.TokenResponse, error) {
	s.exchangeCalls++
	return &xai.TokenResponse{}, nil
}

func (s *grokOAuthClientStub) RefreshToken(context.Context, string, string, string) (*xai.TokenResponse, error) {
	return s.refreshResponse, nil
}

func TestGrokOAuthServiceRefreshTokenPreservesOriginalRefreshTokenWhenNotRotated(t *testing.T) {
	svc := NewGrokOAuthService(nil, &grokOAuthClientStub{
		refreshResponse: &xai.TokenResponse{
			AccessToken: "new-access-token",
			TokenType:   "Bearer",
			ExpiresIn:   3600,
		},
	})
	defer svc.Stop()

	info, err := svc.RefreshToken(context.Background(), "original-refresh-token", "", "client-id")
	require.NoError(t, err)
	require.Equal(t, "new-access-token", info.AccessToken)
	require.Equal(t, "original-refresh-token", info.RefreshToken)
	require.Equal(t, "client-id", info.ClientID)
	require.Equal(t, xai.DefaultReferrer, info.Referrer)
}

func TestGrokOAuthServiceBuildAccountCredentialsStoresTokenResponseExtra(t *testing.T) {
	svc := NewGrokOAuthService(nil, nil)
	defer svc.Stop()

	info := svc.tokenInfoFromResponse(&xai.TokenResponse{
		AccessToken:  "new-access-token",
		RefreshToken: "new-refresh-token",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		Referrer:     "grok-build",
		Extra: map[string]any{
			"team_id":     "team-123",
			"accessToken": "***",
		},
		ExtraKeys: []string{"accessToken", "team_id"},
	}, "client-id", nil)

	creds := svc.BuildAccountCredentials(info)
	require.Equal(t, "grok-build", creds["referrer"])
	require.Equal(t, []string{"accessToken", "team_id"}, creds["oauth_token_response_extra_keys"])
	extra, ok := creds["oauth_token_response_extra"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "team-123", extra["team_id"])
	require.Equal(t, "***", extra["accessToken"])
	summary, ok := creds["oauth_token_response_summary"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, summary["has_access_token"])
	require.Equal(t, true, summary["has_refresh_token"])
	require.Equal(t, "Bearer", summary["token_type"])
	require.Equal(t, "grok-build", summary["referrer"])
	require.Equal(t, []string{"accessToken", "team_id"}, summary["extra_keys"])
}

func TestGrokOAuthServiceExchangeCodeRequiresStateForCallbackURLAndConsumesSession(t *testing.T) {
	client := &grokOAuthClientStub{}
	svc := NewGrokOAuthService(nil, client)
	defer svc.Stop()

	auth, err := svc.GenerateAuthURL(context.Background(), nil, "")
	require.NoError(t, err)

	_, err = svc.ExchangeCode(context.Background(), &GrokExchangeCodeInput{
		SessionID: auth.SessionID,
		Code:      "http://127.0.0.1:56121/callback?code=code-without-state",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "GROK_OAUTH_STATE_REQUIRED")
	require.Zero(t, client.exchangeCalls)

	_, err = svc.ExchangeCode(context.Background(), &GrokExchangeCodeInput{
		SessionID: auth.SessionID,
		Code:      "code-with-state",
		State:     auth.State,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "GROK_OAUTH_SESSION_NOT_FOUND")
	require.Zero(t, client.exchangeCalls)
}
