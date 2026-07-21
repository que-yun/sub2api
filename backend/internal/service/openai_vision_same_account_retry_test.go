package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShouldRetryOpenAISameAccountVisionModel(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-*":                              "z-ai/glm-5.2",
				"meta/llama-3.2-11b-vision-instruct": "meta/llama-3.2-11b-vision-instruct",
				"nvidia/nemotron-nano-12b-v2-vl":     "nvidia/nemotron-nano-12b-v2-vl",
			},
			"vision_model_mapping": map[string]any{
				"gpt-*": "meta/llama-3.2-11b-vision-instruct",
			},
			"openai_capabilities": map[string]any{"chat_completions": true, "vision": true},
		},
	}

	tried := map[string]struct{}{
		"meta/llama-3.2-11b-vision-instruct": {},
		"meta/llama-3.2-90b-vision-instruct": {},
	}
	next, ok := shouldRetryOpenAISameAccountVisionModel(
		account,
		http.StatusInternalServerError,
		"broken data stream when reading image file",
		[]byte(`{"error":{"message":"broken data stream when reading image file"}}`),
		"meta/llama-3.2-11b-vision-instruct",
		tried,
	)
	require.True(t, ok)
	require.Equal(t, "nvidia/nemotron-nano-12b-v2-vl", next)
}


func TestIsOpenAICompatibleModelPayloadIncompatError(t *testing.T) {
	require.True(t, isOpenAICompatibleModelPayloadIncompatError(
		"invalid tool call arguments",
		[]byte(`{"error":{"message":"invalid tool call arguments","type":"invalid_request_error"}}`),
	))
	require.True(t, isOpenAICompatibleModelPayloadIncompatError(
		"Extra data: line 1 column 61 (char 60)",
		[]byte(`{"error":{"message":"Extra data: line 1 column 61 (char 60)"}}`),
	))
	require.True(t, isOpenAICompatibleModelPayloadIncompatError(
		"tools are not supported for this model",
		nil,
	))
	require.False(t, isOpenAICompatibleModelPayloadIncompatError(
		"invalid api key",
		[]byte(`{"error":{"message":"invalid api key"}}`),
	))
}

func TestShouldFailoverOpenAIUpstreamResponse_MultiModelToolIncompat(t *testing.T) {
	svc := &OpenAIGatewayService{}
	ollamaAcc := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://ollama.com/v1",
			"model_mapping": map[string]any{
				"gpt-*":      "glm-5.2",
				"gemma4:31b": "gemma4:31b",
			},
			"vision_model_mapping": map[string]any{
				"gpt-*": "gemma4:31b",
			},
		},
	}
	nativeAcc := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "tok",
		},
	}
	msg := "invalid tool call arguments"
	body := []byte(`{"error":{"message":"invalid tool call arguments","type":"invalid_request_error"}}`)
	require.True(t, svc.shouldFailoverOpenAIUpstreamResponse(http.StatusBadRequest, msg, body, ollamaAcc, "gpt-5.5"))
	require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(http.StatusBadRequest, msg, body, nativeAcc, "gpt-5.5"))
}
