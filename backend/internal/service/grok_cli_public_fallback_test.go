//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGrokOAuthAlwaysStartsOnCLIProxyIncludingHotmailSuperGrok(t *testing.T) {
	account := Account{
		Type:     AccountTypeOAuth,
		Platform: PlatformGrok,
		Name:     "ThospqAbadi@hotmail.com",
		Credentials: map[string]any{
			"email": "thospqabadi@hotmail.com",
		},
		Extra: map[string]any{
			"grok_billing_snapshot": map[string]any{"plan": "SuperGrok"},
			"grok_usage_snapshot": map[string]any{
				"requests": map[string]any{"limit": int64(8300), "remaining": int64(8300)},
			},
		},
	}
	require.Equal(t, xai.DefaultCLIBaseURL, account.GetGrokBaseURL())
	require.Equal(t, xai.DefaultCLIBaseURL, account.GetGrokMediaBaseURL())
}

func TestCanFallbackGrokCLIProxyToPublicAPI(t *testing.T) {
	account := &Account{Type: AccountTypeOAuth, Platform: PlatformGrok}
	require.True(t, canFallbackGrokCLIProxyToPublicAPI(account, xai.DefaultCLIBaseURL, http.StatusForbidden))
	require.False(t, canFallbackGrokCLIProxyToPublicAPI(account, xai.DefaultCLIBaseURL, http.StatusTooManyRequests))
	require.False(t, canFallbackGrokCLIProxyToPublicAPI(account, xai.DefaultBaseURL, http.StatusForbidden))
	require.False(t, canFallbackGrokCLIProxyToPublicAPI(&Account{Type: AccountTypeAPIKey, Platform: PlatformGrok}, xai.DefaultCLIBaseURL, http.StatusForbidden))
}

func TestForwardGrokResponsesFallsBackToPublicAPIOnCLIProxy403Isolated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok","input":"ping","stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	account := &Account{
		ID:          20767,
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Name:        "ThospqAbadi@hotmail.com",
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"access_token":  "token",
			"refresh_token": "refresh-token",
			"expires_at":    time.Now().Add(2 * grokTokenRefreshSkew).UTC().Format(time.RFC3339),
			"email":         "thospqabadi@hotmail.com",
		},
		Extra: map[string]any{
			"grok_billing_snapshot": map[string]any{"plan": "SuperGrok"},
		},
	}
	okBody := strings.Join([]string{
		`data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_public","model":"grok-4.3","usage":{"input_tokens":1,"output_tokens":1}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "xai-request-id": []string{"cli-403"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Access denied"}}`)),
		},
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"public-ok"}},
			Body:       io.NopCloser(strings.NewReader(okBody)),
		},
	}}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{20767: account},
		},
	}
	svc := &OpenAIGatewayService{
		httpUpstream:      upstream,
		accountRepo:       repo,
		grokTokenProvider: NewGrokTokenProvider(repo, nil),
	}

	result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok", true, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, xai.DefaultCLIBaseURL+"/responses", upstream.requests[0].URL.String())
	require.Equal(t, xai.DefaultBaseURL+"/responses", upstream.requests[1].URL.String())
	require.Equal(t, "resp_public", result.ResponseID)
}
