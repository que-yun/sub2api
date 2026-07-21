package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func ollamaCloudProAccount() *Account {
	return &Account{
		ID:          21521,
		Name:        "ollama-cloud-pro",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://ollama.com/v1",
			"model_mapping": map[string]any{
				"gpt-*":                "glm-5.2",
				"gpt-5.6-sol":          "glm-5.2",
				"glm-5.2":              "glm-5.2",
				"kimi-k2.7-code":       "kimi-k2.7-code",
				"kimi-k2.6":            "kimi-k2.6",
				"qwen3.5:397b":         "qwen3.5:397b",
				"qwen3.5-397b":         "qwen3.5:397b",
				"minimax-m3":           "minimax-m3",
				"mistral-large-3:675b": "mistral-large-3:675b",
			},
		},
		Extra: map[string]any{
			"openai_responses_supported": true,
			"use_responses_api":          true,
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func TestIsOpenAIUpstreamFailedToReadRequestBody(t *testing.T) {
	body := []byte(`{"error":{"message":"failed to read request body","type":"invalid_request_error"}}`)
	require.True(t, isOpenAIUpstreamFailedToReadRequestBody("failed to read request body", body))
	require.True(t, isOpenAIRequestBodyTooLargeError(http.StatusBadRequest, "failed to read request body", body))
	require.False(t, isOpenAIRequestBodyTooLargeError(http.StatusBadRequest, "invalid model", []byte(`{"error":{"message":"invalid model"}}`)))
}

func TestShouldRetryOpenAISameAccountLargeContextModel(t *testing.T) {
	acc := ollamaCloudProAccount()
	body := []byte(`{"error":{"message":"failed to read request body","type":"invalid_request_error"}}`)
	tried := map[string]struct{}{}

	next, ok := shouldRetryOpenAISameAccountLargeContextModel(acc, 400, "failed to read request body", body, "glm-5.2", tried)
	require.True(t, ok)
	require.Equal(t, "kimi-k2.7-code", next)

	native := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk", "base_url": "https://api.openai.com/v1"},
	}
	_, ok = shouldRetryOpenAISameAccountLargeContextModel(native, 400, "failed to read request body", body, "gpt-5.5", nil)
	require.False(t, ok)
}

func TestForward_OllamaFailedToReadBodyRetriesLargerContextModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	failBody := `{"error":{"message":"failed to read request body","type":"invalid_request_error","param":null,"code":null}}`
	okBody := `{"id":"resp_ok","object":"response","model":"kimi-k2.7-code","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`
	upstream := &httpUpstreamRecorder{
		responses: []*http.Response{
			{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(failBody)),
			},
			{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(okBody)),
			},
		},
	}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := ollamaCloudProAccount()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	requestBody := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":"hello oversized codex session"}`)
	result, err := svc.Forward(context.Background(), c, account, requestBody)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, "glm-5.2", gjson.GetBytes(upstream.bodies[0], "model").String())
	require.Equal(t, "kimi-k2.7-code", gjson.GetBytes(upstream.bodies[1], "model").String())
	require.Equal(t, "kimi-k2.7-code", result.UpstreamModel)
}

func TestForward_OllamaFailedToReadBodyExhaustsLadderThenAccountFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	failBody := `{"error":{"message":"failed to read request body","type":"invalid_request_error"}}`
	// Always fail so ladder exhausts.
	makeFail := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(failBody)),
		}
	}
	upstream := &httpUpstreamRecorder{
		// Always return body-read failure so the ladder exhausts, then account failover.
		resp: makeFail(),
	}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := ollamaCloudProAccount()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	requestBody := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":"hello oversized"}`)
	result, err := svc.Forward(context.Background(), c, account, requestBody)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.IsOpenAIRequestBodyTooLarge())
	require.GreaterOrEqual(t, len(upstream.bodies), 2)
	require.Equal(t, "glm-5.2", gjson.GetBytes(upstream.bodies[0], "model").String())
	require.Equal(t, "kimi-k2.7-code", gjson.GetBytes(upstream.bodies[1], "model").String())
	require.False(t, c.Writer.Written(), "account failover must happen before client write")
}
