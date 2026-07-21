package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func glmMappedOpenAIAccount(id int64, name string) *Account {
	return &Account{
		ID:          id,
		Name:        name,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.example.test",
			"model_mapping": map[string]any{
				"gpt-5.5":     "z-ai/glm-5.2",
				"gpt-5.6-sol": "z-ai/glm-5.2",
				"gpt-*":       "z-ai/glm-5.2",
			},
		},
		Extra: map[string]any{
			"openai_responses_supported": true,
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func nativeOpenAIAccount(id int64, name string) *Account {
	return &Account{
		ID:          id,
		Name:        name,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.example.test",
		},
		Extra: map[string]any{
			"openai_responses_supported": true,
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func TestIsGLMFamilyModel(t *testing.T) {
	require.True(t, isGLMFamilyModel("z-ai/glm-5.2"))
	require.True(t, isGLMFamilyModel("glm-5.2"))
	require.True(t, isGLMFamilyModel("GLM-4.6"))
	require.False(t, isGLMFamilyModel("gpt-5.5"))
	require.False(t, isGLMFamilyModel(""))
}

func TestAccountMapsRequestToGLM(t *testing.T) {
	glmAcc := glmMappedOpenAIAccount(21546, "nvidia-glm")
	nativeAcc := nativeOpenAIAccount(1, "native")
	require.True(t, accountMapsRequestToGLM(glmAcc, "gpt-5.5"))
	require.True(t, accountMapsRequestToGLM(glmAcc, "gpt-5.6-sol"))
	require.False(t, accountMapsRequestToGLM(nativeAcc, "gpt-5.5"))
	require.False(t, accountMapsRequestToGLM(nil, "gpt-5.5"))
}

func TestShouldFailoverOpenAIContextWindow_OnlyWhenMappedToGLM(t *testing.T) {
	body := []byte(`{"message":"This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens. Please reduce the length of the messages.","type":"Bad Request","code":400}`)
	msg := "This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens."
	glmAcc := glmMappedOpenAIAccount(21546, "nvidia-glm")
	nativeAcc := nativeOpenAIAccount(1, "native")

	require.True(t, shouldFailoverOpenAIContextWindow(glmAcc, "gpt-5.5", msg, body))
	require.False(t, shouldFailoverOpenAIContextWindow(nativeAcc, "gpt-5.5", msg, body))
	require.False(t, shouldFailoverOpenAIContextWindow(glmAcc, "gpt-5.5", "temporary outage", []byte(`{"error":{"message":"temporary outage"}}`)))
}

func TestShouldFailoverOpenAIUpstreamResponse_GLMContextWindow(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := []byte(`{"message":"This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens.","type":"Bad Request","code":400}`)
	msg := "This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens."
	glmAcc := glmMappedOpenAIAccount(21546, "nvidia-glm")
	nativeAcc := nativeOpenAIAccount(1, "native")

	require.True(t, svc.shouldFailoverOpenAIUpstreamResponse(http.StatusBadRequest, msg, body, glmAcc, "gpt-5.5"))
	require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(http.StatusBadRequest, msg, body, nativeAcc, "gpt-5.5"))
}

func TestNewOpenAIUpstreamFailoverError_GLMContextWindowReason(t *testing.T) {
	body := []byte(`{"message":"This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens.","type":"Bad Request","code":400}`)
	msg := "This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens."
	glmAcc := glmMappedOpenAIAccount(21546, "nvidia-glm")
	nativeAcc := nativeOpenAIAccount(1, "native")

	glmErr := newOpenAIUpstreamFailoverError(http.StatusBadRequest, http.Header{}, body, msg, true, glmAcc, "gpt-5.5")
	require.True(t, glmErr.IsOpenAIContextWindowGLMFailover())
	require.Equal(t, openAIContextWindowGLMFailoverReason, glmErr.Reason)
	require.Equal(t, GatewayFailureScopeAccount, glmErr.Scope)
	require.Equal(t, NextAccountRetry, glmErr.NextAccountAction)
	require.False(t, glmErr.RetryableOnSameAccount)

	// Non-GLM context-window should not take the GLM-specific path via helper
	// when shouldFailover is false; newOpenAIUpstreamFailoverError is only called
	// after shouldFailover returns true on non-context paths. Still verify the
	// helper does not tag native accounts.
	nativeErr := newOpenAIUpstreamFailoverError(http.StatusBadGateway, http.Header{}, []byte(`{"error":{"message":"temporary"}}`), "temporary", true, nativeAcc, "gpt-5.5")
	require.False(t, nativeErr.IsOpenAIContextWindowGLMFailover())
}

func TestShouldSkipGLMMappedAccountForContextWindow(t *testing.T) {
	glmAcc := glmMappedOpenAIAccount(21546, "nvidia-glm")
	nativeAcc := nativeOpenAIAccount(2, "native")
	ctx := WithSkipGLMMappedAccountsForContextWindow(context.Background())

	require.True(t, shouldSkipGLMMappedAccountForContextWindow(ctx, glmAcc, "gpt-5.5"))
	require.False(t, shouldSkipGLMMappedAccountForContextWindow(ctx, nativeAcc, "gpt-5.5"))
	require.False(t, shouldSkipGLMMappedAccountForContextWindow(context.Background(), glmAcc, "gpt-5.5"))
}

func TestForward_GLMContextWindowTriggersFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello oversized"}`)

	const upstreamBody = `{"message":"This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens. Please reduce the length of the messages.","type":"Bad Request","code":400}`
	body := &passthroughCloseTrackingReadCloser{Reader: bytes.NewReader([]byte(upstreamBody))}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		}},
	}
	account := glmMappedOpenAIAccount(21546, "nvidia-glm")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	result, err := svc.Forward(context.Background(), c, account, requestBody)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.IsOpenAIContextWindowGLMFailover())
	require.False(t, c.Writer.Written(), "GLM context-window failover must happen before client output")
	require.True(t, body.closed)
}

func TestForward_NativeContextWindowDoesNotFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello oversized"}`)

	const upstreamBody = `{"message":"This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens. Please reduce the length of the messages.","type":"Bad Request","code":400}`
	body := &passthroughCloseTrackingReadCloser{Reader: bytes.NewReader([]byte(upstreamBody))}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		}},
	}
	account := nativeOpenAIAccount(1, "native")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	result, err := svc.Forward(context.Background(), c, account, requestBody)
	require.Nil(t, result)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "native context-window stays fail-fast")
	require.True(t, c.Writer.Written())
	require.True(t, body.closed)
}

func TestShouldFailoverOpenAIPassthroughResponse_GLMContextWindow(t *testing.T) {
	body := []byte(`{"message":"This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens.","type":"Bad Request","code":400}`)
	glmAcc := glmMappedOpenAIAccount(21546, "nvidia-glm")
	nativeAcc := nativeOpenAIAccount(1, "native")

	require.True(t, shouldFailoverOpenAIPassthroughResponse(glmAcc, http.StatusBadRequest, body, "gpt-5.5"))
	require.False(t, shouldFailoverOpenAIPassthroughResponse(nativeAcc, http.StatusBadRequest, body, "gpt-5.5"))
}

func TestForwardPassthrough_GLMContextWindowTriggersFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello oversized"}`)
	const upstreamBody = `{"message":"This model's maximum context length is 202752 tokens. However, your messages resulted in 702051 tokens. Please reduce the length of the messages.","type":"Bad Request","code":400}`
	body := &passthroughCloseTrackingReadCloser{Reader: bytes.NewReader([]byte(upstreamBody))}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		}},
	}
	account := glmMappedOpenAIAccount(21546, "nvidia-glm-passthrough")
	account.Extra["openai_passthrough"] = true

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	result, err := svc.Forward(context.Background(), c, account, requestBody)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.IsOpenAIContextWindowGLMFailover())
	require.False(t, c.Writer.Written())
	require.True(t, body.closed)
}

