//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPatchGrokResponsesBodySetsMappedModelAndDropsUnsupportedFields(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "grok",
		"input": "hello",
		"prompt_cache_retention": "24h",
		"safety_identifier": "user-1",
		"reasoning": {"effort": "high"}
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.3")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.Equal(t, "grok-4.3", gjson.GetBytes(patched, "model").String())
	require.False(t, gjson.GetBytes(patched, "prompt_cache_retention").Exists())
	require.False(t, gjson.GetBytes(patched, "safety_identifier").Exists())
	require.Equal(t, "high", gjson.GetBytes(patched, "reasoning.effort").String())
}

func TestExtractGrokResponsesReasoningEffortSupportsOpenAICompatibleField(t *testing.T) {
	t.Parallel()

	effort := extractOpenAIReasoningEffortFromBody(
		[]byte(`{"model":"grok-4.3","reasoning_effort":"high"}`),
		"grok-4.3",
	)
	require.NotNil(t, effort)
	require.Equal(t, "high", *effort)
}

func TestPatchGrokResponsesBodyDropsGrok45ReasoningUnsupportedFields(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "grok-latest",
		"input": "hello",
		"presence_penalty": 0.1,
		"presencePenalty": 0.2,
		"frequency_penalty": 0.3,
		"frequencyPenalty": 0.4,
		"stop": ["done"]
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.Equal(t, "grok-4.5", gjson.GetBytes(patched, "model").String())
	require.False(t, gjson.GetBytes(patched, "presence_penalty").Exists())
	require.False(t, gjson.GetBytes(patched, "presencePenalty").Exists())
	require.False(t, gjson.GetBytes(patched, "frequency_penalty").Exists())
	require.False(t, gjson.GetBytes(patched, "frequencyPenalty").Exists())
	require.False(t, gjson.GetBytes(patched, "stop").Exists())
}

func TestPatchGrokResponsesBodyDropsReasoningForComposerModels(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "grok",
		"input": "hello",
		"reasoning": {"effort": "high"},
		"reasoning_effort": "high",
		"reasoningEffort": "high"
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-composer-2.5-fast")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.False(t, gjson.GetBytes(patched, "reasoning").Exists())
	require.False(t, gjson.GetBytes(patched, "reasoning_effort").Exists())
	require.False(t, gjson.GetBytes(patched, "reasoningEffort").Exists())
}

func TestPatchGrokResponsesBodyKeepsPenaltyAndStopFieldsForNon45Models(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "grok-4.3",
		"input": "hello",
		"presence_penalty": 0.1,
		"frequency_penalty": 0.2,
		"stop": ["done"]
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.3")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.Equal(t, "grok-4.3", gjson.GetBytes(patched, "model").String())
	require.Equal(t, 0.1, gjson.GetBytes(patched, "presence_penalty").Float())
	require.Equal(t, 0.2, gjson.GetBytes(patched, "frequency_penalty").Float())
	require.Len(t, gjson.GetBytes(patched, "stop").Array(), 1)
}

func TestPatchGrokResponsesBodyDropsReasoningNullContent(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "gpt-5.6-sol",
		"input": [
			{"type": "reasoning", "id": "rs_1", "content": null, "summary": [{"type": "summary_text", "text": "kept"}]},
			{"type": "message", "role": "user", "content": "hello"}
		]
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.False(t, strings.Contains(string(patched), `"content":null`))
	require.Equal(t, "message", gjson.GetBytes(patched, "input.0.type").String())
	require.Equal(t, "assistant", gjson.GetBytes(patched, "input.0.role").String())
	require.Equal(t, "message", gjson.GetBytes(patched, "input.1.type").String())
}

func TestPatchGrokResponsesBodyDropsNestedUnsupportedFields(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "grok",
		"input": "hello",
		"external_web_access": true,
		"tools": [
			{"type": "function", "name": "kept_fn", "external_web_access": true, "parameters": {"type": "object", "properties": {"q": {"type": "string", "external_web_access": true}}}}
		],
		"metadata": {"external_web_access": false}
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.3")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.False(t, strings.Contains(string(patched), "external_web_access"))
	require.Equal(t, "kept_fn", gjson.GetBytes(patched, "tools.0.name").String())
}

func TestPatchGrokResponsesBodyFlattensNamespaceAndDropsInvalidShellTools(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "grok",
		"input": "hello",
		"tools": [
			{"type": "namespace", "namespace": "functions", "tools": [{"type": "function", "name": "inner"}]},
			{"type": "function", "name": "kept_fn", "parameters": {"type": "object"}},
			{"type": "shell", "name": "kept_shell"}
		],
		"tool_choice": {"type": "function", "name": "kept_fn"}
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.3")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.Equal(t, "grok-4.3", gjson.GetBytes(patched, "model").String())
	require.Len(t, gjson.GetBytes(patched, "tools").Array(), 2)
	require.False(t, gjson.GetBytes(patched, `tools.#(type=="namespace")`).Exists())
	require.Equal(t, "functions__inner", gjson.GetBytes(patched, `tools.#(name=="functions__inner").name`).String())
	require.Equal(t, "kept_fn", gjson.GetBytes(patched, `tools.#(name=="kept_fn").name`).String())
	require.False(t, gjson.GetBytes(patched, `tools.#(type=="shell")`).Exists())
	require.Equal(t, "kept_fn", gjson.GetBytes(patched, "tool_choice.name").String())
}

func TestPatchGrokResponsesBodyDropsToolChoiceWhenNoSupportedToolsRemain(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "grok",
		"input": "hello",
		"tools": [
			{"type": "namespace", "namespace": "functions"},
			{"type": "image_generation", "model": "gpt-image-2"}
		],
		"tool_choice": {"type": "namespace", "namespace": "functions"}
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.3")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.False(t, gjson.GetBytes(patched, "tools").Exists())
	require.False(t, gjson.GetBytes(patched, "tool_choice").Exists())
}

func TestPatchGrokResponsesBodyDropsToolChoiceWithoutTools(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "grok",
		"input": "hello",
		"tool_choice": "auto"
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.3")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.False(t, gjson.GetBytes(patched, "tools").Exists())
	require.False(t, gjson.GetBytes(patched, "tool_choice").Exists())
}

func TestPatchGrokResponsesBodyDropsShellToolWithoutEnvironment(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "grok",
		"input": "hello",
		"tools": [
			{"type": "shell", "name": "bad_shell"},
			{"type": "shell", "name": "good_shell", "environment": {"type": "sandbox"}}
		],
		"tool_choice": "auto"
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.Equal(t, 1, int(gjson.GetBytes(patched, "tools.#").Int()))
	require.Equal(t, "good_shell", gjson.GetBytes(patched, "tools.0.name").String())
	require.True(t, gjson.GetBytes(patched, "tool_choice").Exists())
}

func TestPatchGrokResponsesBodySanitizesUnsupportedModelInputItems(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "gpt-5.6-sol",
		"previous_response_id": "resp_grok_prev",
		"input": [
			{"type": "item_reference", "id": "call_ref_1"},
			{"type": "additional_tools", "role": "developer", "tools": [{"type": "local_shell"}]},
			{"type": "input_text", "text": "continue with this"},
			{"type": "file_search_call", "id": "fs_1", "status": "completed", "queries": ["claude code session manager"]},
			{"type": "mcp_tool_call_output", "call_id": "call_mcp_1", "output": "{\"ok\":true}"},
			{"type": "custom_tool_call", "call_id": "call_custom_1", "name": "lookup", "arguments": "{\"q\":\"x\"}"},
			{"type": "message", "role": "user", "content": "keep raw message"}
		]
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.Equal(t, "resp_grok_prev", gjson.GetBytes(patched, "previous_response_id").String())
	require.Equal(t, "web_search", gjson.GetBytes(patched, "tools.0.type").String())
	require.False(t, gjson.GetBytes(patched, `input.#(type=="item_reference")`).Exists())
	require.False(t, gjson.GetBytes(patched, `input.#(type=="additional_tools")`).Exists())
	require.Equal(t, "message", gjson.GetBytes(patched, "input.0.type").String())
	require.Equal(t, "user", gjson.GetBytes(patched, "input.0.role").String())
	require.Equal(t, "continue with this", gjson.GetBytes(patched, "input.0.content").String())
	require.Equal(t, "message", gjson.GetBytes(patched, "input.1.type").String())
	require.Equal(t, "assistant", gjson.GetBytes(patched, "input.1.role").String())
	require.Equal(t, "claude code session manager", gjson.GetBytes(patched, "input.1.content").String())
	require.Equal(t, "function_call_output", gjson.GetBytes(patched, "input.2.type").String())
	require.Equal(t, "call_mcp_1", gjson.GetBytes(patched, "input.2.call_id").String())
	require.Equal(t, `{"ok":true}`, gjson.GetBytes(patched, "input.2.output").String())
	require.Equal(t, "function_call", gjson.GetBytes(patched, "input.3.type").String())
	require.Equal(t, "call_custom_1", gjson.GetBytes(patched, "input.3.call_id").String())
	require.Equal(t, "lookup", gjson.GetBytes(patched, "input.3.name").String())
	require.Equal(t, "message", gjson.GetBytes(patched, "input.4.type").String())
	require.Equal(t, "keep raw message", gjson.GetBytes(patched, "input.4.content").String())
}

func TestPatchGrokResponsesBodyBridgesAdditionalCustomAndToolSearchTools(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "gpt-5.6-sol",
		"input": [
			{"type": "additional_tools", "role": "developer", "tools": [
				{"type": "custom", "name": "exec", "description": "Run a local command", "format": {"type": "text"}},
				{"type": "tool_search"},
				{"type": "namespace", "name": "functions", "tools": [
					{"type": "function", "name": "read_file", "description": "Read a file", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}
				]},
				{"type": "local_shell", "name": "shell"}
			]},
			{"type": "message", "role": "user", "content": "install brew package"}
		]
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.False(t, gjson.GetBytes(patched, `input.#(type=="additional_tools")`).Exists())
	require.Equal(t, "function", gjson.GetBytes(patched, `tools.#(name=="exec").type`).String())
	require.Equal(t, "Run a local command", gjson.GetBytes(patched, `tools.#(name=="exec").description`).String())
	require.Equal(t, "input", gjson.GetBytes(patched, `tools.#(name=="exec").parameters.required.0`).String())
	require.Equal(t, "function", gjson.GetBytes(patched, `tools.#(name=="tool_search").type`).String())
	require.Equal(t, "function", gjson.GetBytes(patched, `tools.#(name=="functions__read_file").type`).String())
	require.False(t, gjson.GetBytes(patched, `tools.#(name=="shell")`).Exists())
	require.True(t, gjson.GetBytes(patched, `tools.#(type=="web_search")`).Exists())
}

func TestGrokResponsesToolBridgeTransformsCustomToolCalls(t *testing.T) {
	t.Parallel()

	requestBody := []byte(`{
		"model": "gpt-5.6-sol",
		"input": [
			{"type": "additional_tools", "tools": [
				{"type": "custom", "name": "exec", "description": "Run a local command"}
			]},
			{"type": "message", "role": "user", "content": "run echo ok"}
		]
	}`)
	bridge := extractGrokResponsesToolBridge(requestBody)
	require.True(t, bridge.CustomTools["exec"])
	ctx := context.WithValue(context.Background(), grokResponsesToolBridgeContextKey{}, &grokResponsesToolBridgeSession{Bridge: bridge, customItemID: map[string]struct{}{}})

	body := []byte(`{
		"id": "resp_1",
		"object": "response",
		"model": "grok-4.5",
		"status": "completed",
		"output": [
			{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "exec", "arguments": "{\"input\":\"echo ok\"}", "status": "completed"}
		],
		"usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}
	}`)

	transformed := transformGrokResponsesToolBridgeBody(ctx, body)
	require.Equal(t, "custom_tool_call", gjson.GetBytes(transformed, "output.0.type").String())
	require.Equal(t, "exec", gjson.GetBytes(transformed, "output.0.name").String())
	require.Equal(t, "echo ok", gjson.GetBytes(transformed, "output.0.input").String())
	require.False(t, gjson.GetBytes(transformed, "output.0.arguments").Exists())
}

func TestGrokResponsesToolBridgeTransformsToolSearchAndNamespaceCalls(t *testing.T) {
	t.Parallel()

	requestBody := []byte(`{
		"model": "gpt-5.6-sol",
		"tools": [{"type": "tool_search"}],
		"input": [
			{"type": "additional_tools", "tools": [
				{"type": "namespace", "name": "functions", "tools": [
					{"type": "function", "name": "read_file", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}
				]}
			]},
			{"type": "message", "role": "user", "content": "load/read"}
		]
	}`)
	bridge := extractGrokResponsesToolBridge(requestBody)
	ctx := context.WithValue(context.Background(), grokResponsesToolBridgeContextKey{}, &grokResponsesToolBridgeSession{Bridge: bridge, customItemID: map[string]struct{}{}})

	body := []byte(`{
		"output": [
			{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "tool_search", "arguments": "{\"query\":\"shell\"}", "status": "completed"},
			{"type": "function_call", "id": "fc_2", "call_id": "call_2", "name": "functions__read_file", "arguments": "{\"path\":\"README.md\"}", "status": "completed"}
		]
	}`)

	transformed := transformGrokResponsesToolBridgeBody(ctx, body)
	require.Equal(t, "tool_search_call", gjson.GetBytes(transformed, "output.0.type").String())
	require.Equal(t, "client", gjson.GetBytes(transformed, "output.0.execution").String())
	require.Equal(t, "shell", gjson.GetBytes(transformed, "output.0.arguments.query").String())
	require.Equal(t, "function_call", gjson.GetBytes(transformed, "output.1.type").String())
	require.Equal(t, "functions", gjson.GetBytes(transformed, "output.1.namespace").String())
	require.Equal(t, "read_file", gjson.GetBytes(transformed, "output.1.name").String())
}

func TestGrokResponsesToolBridgeTransformsStreamingCustomToolEvents(t *testing.T) {
	t.Parallel()

	bridge := grokResponsesToolBridge{CustomTools: map[string]bool{"exec": true}}
	ctx := context.WithValue(context.Background(), grokResponsesToolBridgeContextKey{}, &grokResponsesToolBridgeSession{Bridge: bridge, customItemID: map[string]struct{}{}})

	added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec","status":"in_progress"}}`)
	transformedAdded, changed, drop := transformGrokResponsesToolBridgeSSEData(ctx, added)
	require.True(t, changed)
	require.False(t, drop)
	require.Equal(t, "custom_tool_call", gjson.GetBytes(transformedAdded, "item.type").String())

	delta := []byte(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"input\":\"brew install cchv\"}"}`)
	_, changed, drop = transformGrokResponsesToolBridgeSSEData(ctx, delta)
	require.True(t, changed)
	require.True(t, drop)

	done := []byte(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"input\":\"brew install cchv\"}"}`)
	transformedDone, changed, drop := transformGrokResponsesToolBridgeSSEData(ctx, done)
	require.True(t, changed)
	require.False(t, drop)
	require.Equal(t, "response.custom_tool_call_input.done", gjson.GetBytes(transformedDone, "type").String())
	require.Equal(t, "brew install cchv", gjson.GetBytes(transformedDone, "input").String())
	require.False(t, gjson.GetBytes(transformedDone, "arguments").Exists())
}

func TestBuildGrokResponsesRequestUsesAccountBaseURLAndBearerToken(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")

	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"base_url": "https://xai.test/v1/",
		},
	}

	req, err := buildGrokResponsesRequest(context.Background(), nil, account, []byte(`{"model":"grok-4.3"}`), "access-token")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, "https://xai.test/v1/responses", req.URL.String())
	require.Equal(t, "Bearer access-token", req.Header.Get("Authorization"))
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Contains(t, req.Header.Get("Accept"), "text/event-stream")
	require.Equal(t, xai.DefaultCLIUserAgent, req.Header.Get("User-Agent"))
	require.Equal(t, xai.DefaultCLITokenAuth, req.Header.Get("X-XAI-Token-Auth"))
	require.Equal(t, xai.DefaultCLIClientIdentifier, req.Header.Get("x-grok-client-identifier"))
	require.Equal(t, xai.DefaultCLIClientVersion, req.Header.Get("x-grok-client-version"))

	data, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, `{"model":"grok-4.3"}`, strings.TrimSpace(string(data)))
}

func TestBuildGrokResponsesRequestRejectsUnsafeAccountBaseURL(t *testing.T) {
	t.Parallel()

	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"base_url": "https://xai.test/v1",
		},
	}

	_, err := buildGrokResponsesRequest(context.Background(), nil, account, []byte(`{"model":"grok-4.3"}`), "access-token")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid base url")
}

func TestGrokMediaGenerationGateCoversImagesAndVideo(t *testing.T) {
	tests := []struct {
		name     string
		endpoint GrokMediaEndpoint
		want     bool
	}{
		{name: "image generation", endpoint: GrokMediaEndpointImagesGenerations, want: true},
		{name: "image edit", endpoint: GrokMediaEndpointImagesEdits, want: true},
		{name: "video generation", endpoint: GrokMediaEndpointVideosGenerations, want: true},
		{name: "video status", endpoint: GrokMediaEndpointVideoStatus, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.endpoint.IsGenerationRequest())
		})
	}
}

func TestExtractGrokMediaModelSupportsJSONAndMultipart(t *testing.T) {
	require.Equal(t, "grok-imagine", ExtractGrokMediaModel("application/json", []byte(`{"model":"grok-imagine"}`)))

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("prompt", "draw a cat"))
	require.NoError(t, writer.WriteField("model", "grok-imagine-edit"))
	require.NoError(t, writer.Close())

	require.Equal(t, "grok-imagine-edit", ExtractGrokMediaModel(writer.FormDataContentType(), buf.Bytes()))
}

func TestParseGrokMediaRequestBuildsMultipartModerationBody(t *testing.T) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("prompt", "edit this private image"))
	require.NoError(t, writer.WriteField("model", "grok-imagine-edit"))
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", `form-data; name="image"; filename="input.png"`)
	partHeader.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(partHeader)
	require.NoError(t, err)
	_, err = part.Write([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a})
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	info := ParseGrokMediaRequest(writer.FormDataContentType(), buf.Bytes())
	require.Equal(t, "grok-imagine-edit", info.Model)
	require.Equal(t, "edit this private image", info.Prompt)

	moderationBody := info.ModerationBody()
	require.NotEmpty(t, moderationBody)
	require.Equal(t, "edit this private image", gjson.GetBytes(moderationBody, "prompt").String())
	require.True(t, strings.HasPrefix(gjson.GetBytes(moderationBody, "images.0.image_url").String(), "data:image/"))
}

func TestParseGrokMediaVideoRequestResolution(t *testing.T) {
	info := ParseGrokMediaRequest("application/json", []byte(`{"model":"grok-imagine-video","prompt":"waves","resolution":"720p"}`))

	require.Equal(t, "grok-imagine-video", info.Model)
	require.Equal(t, "720p", info.Resolution)
}

func TestNormalizeGrokMediaModelForEndpoint(t *testing.T) {
	tests := []struct {
		name          string
		endpoint      GrokMediaEndpoint
		model         string
		hasInputImage bool
		want          string
	}{
		{name: "image generation alias", endpoint: GrokMediaEndpointImagesGenerations, model: "grok-imagine", want: "grok-imagine-image-quality"},
		{name: "image edit alias", endpoint: GrokMediaEndpointImagesEdits, model: "grok-imagine", want: "grok-imagine-image-quality"},
		{name: "image quality passthrough", endpoint: GrokMediaEndpointImagesGenerations, model: "grok-imagine-image-quality", want: "grok-imagine-image-quality"},
		{name: "image fast passthrough", endpoint: GrokMediaEndpointImagesGenerations, model: "grok-imagine-image", want: "grok-imagine-image"},
		{name: "video passthrough", endpoint: GrokMediaEndpointVideosGenerations, model: "grok-imagine-video", want: "grok-imagine-video"},
		{name: "video 1.5 text-only fallback", endpoint: GrokMediaEndpointVideosGenerations, model: "grok-imagine-video-1.5", want: "grok-imagine-video"},
		{name: "video 1.5 image-to-video passthrough", endpoint: GrokMediaEndpointVideosGenerations, model: "grok-imagine-video-1.5", hasInputImage: true, want: "grok-imagine-video-1.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, normalizeGrokMediaModelForEndpoint(tt.endpoint, tt.model, tt.hasInputImage))
		})
	}
}

func TestForwardGrokMediaImagesGenerationNormalizesImagineAlias(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok-imagine","prompt":"draw a cat"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := &Account{
		ID:          61,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "api-key",
			"base_url": "https://xai.test/v1",
		},
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":   []string{"application/json"},
			"Xai-Request-Id": []string{"xai-image-req"},
		},
		Body: io.NopCloser(strings.NewReader(`{"data":[]}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	result, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointImagesGenerations, "", body, "application/json")
	require.NoError(t, err)
	require.Equal(t, "https://xai.test/v1/images/generations", upstream.lastReq.URL.String())
	require.Equal(t, http.MethodPost, upstream.lastReq.Method)
	require.Equal(t, "Bearer api-key", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "application/json", upstream.lastReq.Header.Get("Content-Type"))
	require.JSONEq(t, `{"model":"grok-imagine-image-quality","prompt":"draw a cat"}`, string(upstream.lastBody))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"data":[]}`, recorder.Body.String())
	require.Equal(t, "xai-image-req", result.RequestID)
	require.Equal(t, "grok-imagine-image-quality", result.Model)
	require.Equal(t, "grok-imagine-image-quality", result.BillingModel)
	require.Equal(t, 1, result.ImageCount)
	require.Equal(t, ImageBillingSize2K, result.ImageSize)
}

func TestForwardGrokMediaImagesGenerationStripsUnsupportedSize(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok-imagine-image","prompt":"draw a cat","size":"1024x1024"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := &Account{
		ID:          65,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "api-key",
			"base_url": "https://xai.test/v1",
		},
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{"data":[]}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	result, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointImagesGenerations, "", body, "application/json")
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"grok-imagine-image","prompt":"draw a cat"}`, string(upstream.lastBody))
	require.Equal(t, ImageBillingSize1K, result.ImageSize)
	require.Equal(t, "1024x1024", result.ImageInputSize)
}

func TestForwardGrokMediaImagesEditMultipartConvertsToJSON(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("model", "grok-imagine-edit"))
	require.NoError(t, writer.WriteField("prompt", "edit this private image"))
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", `form-data; name="image"; filename="input.png"`)
	partHeader.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(partHeader)
	require.NoError(t, err)
	_, err = part.Write([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a})
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(buf.Bytes()))
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())

	account := &Account{
		ID:          62,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "api-key",
			"base_url": "https://xai.test/v1",
		},
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{"data":[]}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	_, err = svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointImagesEdits, "", buf.Bytes(), writer.FormDataContentType())
	require.NoError(t, err)
	require.Equal(t, "https://xai.test/v1/images/edits", upstream.lastReq.URL.String())
	require.Equal(t, "application/json", upstream.lastReq.Header.Get("Content-Type"))
	require.True(t, json.Valid(upstream.lastBody))
	require.Equal(t, "grok-imagine-edit", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "edit this private image", gjson.GetBytes(upstream.lastBody, "prompt").String())
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.lastBody, "image.image_url").String(), "data:image/png;base64,"))
}

func TestForwardGrokMediaVideoGenerationReturnsUsageAndResponseID(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok-imagine-video-1.5","prompt":"waves","resolution":"720p","duration":10}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := &Account{
		ID:          63,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "api-key",
			"base_url": "https://xai.test/v1",
		},
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":   []string{"application/json"},
			"Xai-Request-Id": []string{"xai-video-generate-req"},
		},
		Body: io.NopCloser(strings.NewReader(`{"request_id":"video-request-123","usage":{"prompt_tokens":3,"completion_tokens":4}}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	result, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointVideosGenerations, "", body, "application/json")
	require.NoError(t, err)
	require.Equal(t, "https://xai.test/v1/videos/generations", upstream.lastReq.URL.String())
	require.JSONEq(t, `{"model":"grok-imagine-video","prompt":"waves","resolution":"720p","duration":10}`, string(upstream.lastBody))
	require.Equal(t, "video-request-123", result.ResponseID)
	require.Equal(t, "grok-imagine-video", result.BillingModel)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Equal(t, 1, result.ImageCount)
	require.Empty(t, result.ImageSize)
	require.Equal(t, 1, result.VideoCount)
	require.Equal(t, VideoBillingResolution720P, result.VideoResolution)
	require.Equal(t, 10, result.VideoDurationSeconds)
}

func TestForwardGrokMediaVideoGenerationPreservesImageToVideoModel(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok-imagine-video-1.5","prompt":"animate","image":{"image_url":"data:image/png;base64,aW1n"}}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := &Account{
		ID:          63,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "api-key",
			"base_url": "https://xai.test/v1",
		},
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{"request_id":"video-request-456"}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	result, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointVideosGenerations, "", body, "application/json")
	require.NoError(t, err)
	require.Equal(t, "https://xai.test/v1/videos/generations", upstream.lastReq.URL.String())
	require.JSONEq(t, `{"model":"grok-imagine-video-1.5","prompt":"animate","image":{"image_url":"data:image/png;base64,aW1n"}}`, string(upstream.lastBody))
	require.Equal(t, "video-request-456", result.ResponseID)
	require.Equal(t, "grok-imagine-video-1.5", result.BillingModel)
	// 未指定 duration 时按上游默认 8 秒计费。
	require.Equal(t, VideoBillingDefaultDurationSeconds, result.VideoDurationSeconds)
}

func TestForwardGrokMediaVideoStatusUsesGETWithoutBody(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/request-123", nil)

	account := &Account{
		ID:          62,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "api-key",
			"base_url": "https://xai.test/v1",
		},
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":   []string{"application/json"},
			"Xai-Request-Id": []string{"xai-video-req"},
		},
		Body: io.NopCloser(strings.NewReader(`{"id":"request-123","status":"completed"}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	result, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointVideoStatus, "request-123", nil, "")
	require.NoError(t, err)
	require.Equal(t, "https://xai.test/v1/videos/request-123", upstream.lastReq.URL.String())
	require.Equal(t, http.MethodGet, upstream.lastReq.Method)
	require.Equal(t, "Bearer api-key", upstream.lastReq.Header.Get("Authorization"))
	require.Empty(t, upstream.lastReq.Header.Get("Content-Type"))
	require.Empty(t, upstream.lastBody)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"id":"request-123","status":"completed"}`, recorder.Body.String())
	require.Equal(t, "xai-video-req", result.RequestID)
}

func TestBindGrokMediaVideoRequestAccountUsesRequestIDStickyHash(t *testing.T) {
	ctx := context.Background()
	groupID := int64(7)
	cache := &stubGatewayCache{}
	svc := &OpenAIGatewayService{cache: cache}

	hash := GrokMediaVideoRequestSessionHash("video-request-123")
	require.NotEmpty(t, hash)
	require.NoError(t, svc.BindGrokMediaVideoRequestAccount(ctx, &groupID, "video-request-123", 63))

	accountID, err := svc.getStickySessionAccountID(ctx, &groupID, hash)
	require.NoError(t, err)
	require.Equal(t, int64(63), accountID)
}

func TestForwardGrokMediaErrorHonorsCustomErrorCodes(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok-imagine","prompt":"draw a cat"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := &Account{
		ID:          64,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":                    "api-key",
			"base_url":                   "https://xai.test/v1",
			"custom_error_codes_enabled": true,
			"custom_error_codes":         []any{float64(http.StatusTooManyRequests)},
		},
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header: http.Header{
			"Content-Type":   []string{"application/json"},
			"Xai-Request-Id": []string{"xai-error-req"},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"do not expose this upstream detail"}}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	result, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointImagesGenerations, "", body, "application/json")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Upstream gateway error")
	require.NotContains(t, recorder.Body.String(), "do not expose")
}

func TestForwardAsChatCompletionsForGrokUsesXAIChatCompletionsAndSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	account := &Account{
		ID:          51,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"base_url":     xai.DefaultCLIBaseURL,
		},
	}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{51: account},
		},
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":                   []string{"application/json"},
			"Xai-Request-Id":                 []string{"xai-req"},
			"X-Ratelimit-Limit-Requests":     []string{"10"},
			"X-Ratelimit-Remaining-Requests": []string{"9"},
			"X-Ratelimit-Limit-Tokens":       []string{"1000"},
			"X-Ratelimit-Remaining-Tokens":   []string{"990"},
		},
		Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl","object":"chat.completion","model":"grok-4.3","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream:      upstream,
		grokTokenProvider: NewGrokTokenProvider(repo, nil),
		accountRepo:       repo,
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, xai.DefaultCLIBaseURL+"/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer access-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, xai.DefaultCLIUserAgent, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, xai.DefaultCLITokenAuth, upstream.lastReq.Header.Get("X-XAI-Token-Auth"))
	require.Equal(t, xai.DefaultCLIClientIdentifier, upstream.lastReq.Header.Get("x-grok-client-identifier"))
	require.Equal(t, xai.DefaultCLIClientVersion, upstream.lastReq.Header.Get("x-grok-client-version"))
	require.Equal(t, "grok-4.5", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "grok", result.Model)
	require.Equal(t, "grok-4.5", result.UpstreamModel)
	require.Equal(t, 1, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.NotNil(t, repo.updates[51][grokQuotaSnapshotExtraKey])
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestForwardGrokResponsesStreamingUsesXAIResponsesAndSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok","input":"hi","stream":true,"reasoning_effort":"high"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("OpenAI-Beta", "responses=experimental")

	account := &Account{
		ID:          52,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"base_url":     xai.DefaultCLIBaseURL,
		},
	}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{52: account},
		},
	}
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.output_text.delta","sequence_number":0,"delta":"ok"}`,
		"",
		`data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_grok","model":"grok-4.3","usage":{"input_tokens":5,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`,
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":                   []string{"text/event-stream"},
			"Xai-Request-Id":                 []string{"xai-stream-req"},
			"X-Ratelimit-Limit-Requests":     []string{"10"},
			"X-Ratelimit-Remaining-Requests": []string{"8"},
			"X-Ratelimit-Limit-Tokens":       []string{"1000"},
			"X-Ratelimit-Remaining-Tokens":   []string{"990"},
		},
		Body: io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream:      upstream,
		grokTokenProvider: NewGrokTokenProvider(repo, nil),
		accountRepo:       repo,
	}

	result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok", true, time.Now())
	require.NoError(t, err)
	require.Equal(t, xai.DefaultCLIBaseURL+"/responses", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer access-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, xai.DefaultCLIUserAgent, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, xai.DefaultCLITokenAuth, upstream.lastReq.Header.Get("X-XAI-Token-Auth"))
	require.Equal(t, xai.DefaultCLIClientIdentifier, upstream.lastReq.Header.Get("x-grok-client-identifier"))
	require.Equal(t, xai.DefaultCLIClientVersion, upstream.lastReq.Header.Get("x-grok-client-version"))
	require.Equal(t, "responses=experimental", upstream.lastReq.Header.Get("OpenAI-Beta"))
	require.Equal(t, "grok-4.5", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "high", gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
	require.True(t, result.Stream)
	require.Equal(t, "resp_grok", result.ResponseID)
	require.Equal(t, "xai-stream-req", result.RequestID)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Equal(t, 2, result.Usage.CacheReadInputTokens)
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "high", *result.ReasoningEffort)
	require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
	require.Contains(t, recorder.Body.String(), "response.output_text.delta")
	require.NotNil(t, repo.updates[52][grokQuotaSnapshotExtraKey])
}

func TestForwardAsChatCompletionsForGrokStreamingUsesRawXAIChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := &Account{
		ID:          53,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"base_url":     xai.DefaultCLIBaseURL,
		},
	}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{53: account},
		},
	}
	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_grok","object":"chat.completion.chunk","model":"grok-4.3","choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		"",
		`data: {"id":"chatcmpl_grok","object":"chat.completion.chunk","model":"grok-4.3","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":4,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":1}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":                   []string{"text/event-stream"},
			"X-Request-Id":                   []string{"chat-stream-req"},
			"X-Ratelimit-Limit-Requests":     []string{"10"},
			"X-Ratelimit-Remaining-Requests": []string{"7"},
		},
		Body: io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:               rawChatCompletionsTestConfig(),
		httpUpstream:      upstream,
		grokTokenProvider: NewGrokTokenProvider(repo, nil),
		accountRepo:       repo,
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, xai.DefaultCLIBaseURL+"/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer access-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "text/event-stream", upstream.lastReq.Header.Get("Accept"))
	require.Equal(t, xai.DefaultCLIUserAgent, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, xai.DefaultCLITokenAuth, upstream.lastReq.Header.Get("X-XAI-Token-Auth"))
	require.Equal(t, xai.DefaultCLIClientIdentifier, upstream.lastReq.Header.Get("x-grok-client-identifier"))
	require.Equal(t, xai.DefaultCLIClientVersion, upstream.lastReq.Header.Get("x-grok-client-version"))
	require.Equal(t, "grok-4.5", gjson.GetBytes(upstream.lastBody, "model").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool())
	require.True(t, result.Stream)
	require.Equal(t, 6, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Equal(t, 1, result.Usage.CacheReadInputTokens)
	require.Contains(t, recorder.Body.String(), "data: [DONE]")
	require.NotNil(t, repo.updates[53][grokQuotaSnapshotExtraKey])
}

func TestForwardAsChatCompletionsForGrokComposerBridgesImageInput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok-composer-2.5-fast","messages":[{"role":"system","content":"You are concise."},{"role":"user","content":[{"type":"text","text":"What is shown?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := &Account{
		ID:          55,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"base_url":     xai.DefaultCLIBaseURL,
		},
	}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{55: account},
		},
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "xai-request-id": []string{"vision-req"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_vision","object":"response","model":"grok-build-0.1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A small diagram with ABC letters."}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`)),
		},
		{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":                   []string{"application/json"},
				"X-Request-Id":                   []string{"composer-req"},
				"X-Ratelimit-Limit-Requests":     []string{"10"},
				"X-Ratelimit-Remaining-Requests": []string{"9"},
				"X-Ratelimit-Limit-Tokens":       []string{"1000"},
				"X-Ratelimit-Remaining-Tokens":   []string{"980"},
			},
			Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_composer","object":"chat.completion","model":"grok-composer-2.5-fast","choices":[{"index":0,"message":{"role":"assistant","content":"It shows ABC."},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)),
		},
	}}
	svc := &OpenAIGatewayService{
		cfg:               rawChatCompletionsTestConfig(),
		httpUpstream:      upstream,
		grokTokenProvider: NewGrokTokenProvider(repo, nil),
		accountRepo:       repo,
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, xai.DefaultCLIBaseURL+"/responses", upstream.requests[0].URL.String())
	require.Equal(t, "grok-build-0.1", gjson.GetBytes(upstream.bodies[0], "model").String())
	require.Equal(t, "input_image", gjson.GetBytes(upstream.bodies[0], "input.0.content.1.type").String())
	require.Equal(t, xai.DefaultCLIBaseURL+"/chat/completions", upstream.requests[1].URL.String())
	require.Equal(t, "grok-composer-2.5-fast", gjson.GetBytes(upstream.bodies[1], "model").String())
	require.False(t, strings.Contains(string(upstream.bodies[1]), "image_url"))
	require.Contains(t, gjson.GetBytes(upstream.bodies[1], "messages.1.content").String(), "Image 1 description")
	require.Contains(t, gjson.GetBytes(upstream.bodies[1], "messages.1.content").String(), "A small diagram with ABC letters.")
	require.Equal(t, 14, result.Usage.InputTokens)
	require.Equal(t, 12, result.Usage.OutputTokens)
	require.Equal(t, "It shows ABC.", gjson.Get(recorder.Body.String(), "choices.0.message.content").String())
	require.NotNil(t, repo.updates[55][grokQuotaSnapshotExtraKey])
}

func TestForwardAsAnthropicForGrokUsesXAIResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	account := &Account{
		ID:          54,
		Name:        "grok",
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"base_url":     xai.DefaultCLIBaseURL,
		},
	}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{54: account},
		},
	}
	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_grok_messages", "grok-4.3")}
	svc := &OpenAIGatewayService{
		httpUpstream:      upstream,
		grokTokenProvider: NewGrokTokenProvider(repo, nil),
		accountRepo:       repo,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, xai.DefaultCLIBaseURL+"/responses", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer access-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, xai.DefaultCLIUserAgent, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, xai.DefaultCLITokenAuth, upstream.lastReq.Header.Get("X-XAI-Token-Auth"))
	require.Equal(t, xai.DefaultCLIClientIdentifier, upstream.lastReq.Header.Get("x-grok-client-identifier"))
	require.Equal(t, xai.DefaultCLIClientVersion, upstream.lastReq.Header.Get("x-grok-client-version"))
	require.Equal(t, "grok-4.5", gjson.GetBytes(upstream.lastBody, "model").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
	require.NotContains(t, string(upstream.lastBody), "chatgpt.com")
	require.Equal(t, "grok", result.Model)
	require.Equal(t, "grok-4.5", result.UpstreamModel)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Contains(t, recorder.Body.String(), `"type":"message"`)
	require.Contains(t, recorder.Body.String(), "ok")
}

func TestHandleGrokAccountUpstreamErrorTempUnschedulesReadinessStates(t *testing.T) {
	tests := []struct {
		name            string
		status          int
		headers         http.Header
		body            []byte
		wantReason      string
		wantMinCooldown time.Duration
		wantMaxCooldown time.Duration
	}{
		{
			name:            "unauthorized reauth",
			status:          http.StatusUnauthorized,
			wantReason:      "grok oauth token unauthorized",
			wantMinCooldown: 10*time.Minute - time.Second,
			wantMaxCooldown: 10*time.Minute + time.Second,
		},
		{
			name:            "forbidden entitlement",
			status:          http.StatusForbidden,
			wantReason:      "grok entitlement or subscription tier denied",
			wantMinCooldown: 24*time.Hour - time.Second,
			wantMaxCooldown: 24*time.Hour + time.Second,
		},
		{
			name:            "rate limited retry after",
			status:          http.StatusTooManyRequests,
			headers:         http.Header{"Retry-After": []string{"45"}},
			wantReason:      "grok rate limited",
			wantMinCooldown: 44 * time.Second,
			wantMaxCooldown: 46 * time.Second,
		},
		{
			name:            "free usage exhausted rolling 24h",
			status:          http.StatusTooManyRequests,
			body:            []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1033696/1000000."}`),
			wantReason:      "grok free usage exhausted (rolling window)",
			wantMinCooldown: 24*time.Hour - time.Second,
			wantMaxCooldown: 24*time.Hour + time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{ID: 61, Platform: PlatformGrok, Type: AccountTypeOAuth}
			repo := &grokQuotaAccountRepo{}
			svc := &OpenAIGatewayService{accountRepo: repo}
			before := time.Now()

			svc.handleGrokAccountUpstreamError(context.Background(), account, tt.status, tt.headers, tt.body)

			require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
			wantRateLimit := strings.Contains(strings.ToLower(tt.wantReason), "rate limited") || strings.Contains(strings.ToLower(tt.wantReason), "free usage exhausted")
			if wantRateLimit {
				// 限流原因只写 rate_limit_reset_at(对齐 OpenAI 429)，不再写 temp_unschedulable_until，
				// 否则会被 admin 的"限流中"筛选排除、与状态徽标自相矛盾。
				require.Equal(t, 0, repo.tempUnschedCalls, "限流原因不应写 temp_unschedulable")
				require.Equal(t, 1, repo.rateLimitCalls)
				require.Equal(t, account.ID, repo.lastRateLimitID)
				require.True(t, repo.lastRateLimitUntil.After(before.Add(tt.wantMinCooldown)))
				require.True(t, repo.lastRateLimitUntil.Before(before.Add(tt.wantMaxCooldown)))
			} else {
				// 鉴权/传输类原因走 temp_unschedulable_until 一条道。
				require.Equal(t, 1, repo.tempUnschedCalls)
				require.Equal(t, account.ID, repo.lastTempUnschedID)
				require.Equal(t, tt.wantReason, repo.lastTempUnschedReason)
				require.True(t, repo.lastTempUnschedUntil.After(before.Add(tt.wantMinCooldown)))
				require.True(t, repo.lastTempUnschedUntil.Before(before.Add(tt.wantMaxCooldown)))
				require.Equal(t, 0, repo.rateLimitCalls)
			}
			if tt.status == http.StatusUnauthorized || tt.status == http.StatusForbidden {
				snapshot, ok := repo.updates[account.ID][grokQuotaSnapshotExtraKey].(*xai.QuotaSnapshot)
				require.True(t, ok)
				require.Equal(t, tt.status, snapshot.StatusCode)
				require.Equal(t, "upstream_error", snapshot.ObservationSource)
			}
		})
	}
}

func TestHandleGrokAccountUpstreamErrorDoesNotShortenExistingPause(t *testing.T) {
	existingUntil := time.Now().Add(15 * time.Minute)
	account := &Account{
		ID:                      62,
		Platform:                PlatformGrok,
		Type:                    AccountTypeOAuth,
		TempUnschedulableUntil:  &existingUntil,
		TempUnschedulableReason: "existing pause",
	}
	repo := &grokQuotaAccountRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}

	svc.handleGrokAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{"Retry-After": []string{"45"}}, nil)

	// 429 是限流：只写 rate_limit_reset_at，且不缩短已有的更长暂停(existingUntil)。
	require.Equal(t, 0, repo.tempUnschedCalls, "限流不应写 temp_unschedulable")
	require.Equal(t, 1, repo.rateLimitCalls)
	require.WithinDuration(t, existingUntil, repo.lastRateLimitUntil, time.Second)
	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	runtimeUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.WithinDuration(t, existingUntil, runtimeUntil, time.Second)
}

// TestHandleGrokAccountUpstreamErrorForbiddenEscalation 覆盖对齐 GPT 的 403 处理：
// 1) 真·permission-denied 连续达阈值 → 永久 error(SetError)；
// 2) free-usage-exhausted 即便以 403 返回，也走可恢复限流冷却，绝不永久禁用。
func TestHandleGrokAccountUpstreamErrorForbiddenEscalation(t *testing.T) {
	t.Run("permission_denied_escalates_at_threshold", func(t *testing.T) {
		gwRepo := &grokQuotaAccountRepo{}
		rlRepo := &rateLimitAccountRepoStub{}
		rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
		rl.SetOpenAI403CounterCache(&openAI403CounterCacheStub{counts: []int64{openAI403DisableThreshold}})
		svc := &OpenAIGatewayService{accountRepo: gwRepo, rateLimitService: rl}
		account := &Account{ID: 81, Platform: PlatformGrok, Type: AccountTypeOAuth,
			Credentials: map[string]any{"refresh_token": "rt-81"}}

		svc.handleGrokAccountUpstreamError(context.Background(), account,
			http.StatusForbidden, http.Header{}, []byte(`{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`))

		require.Equal(t, 1, rlRepo.setErrorCalls, "连续达阈值的 permission-denied 应永久禁用")
		require.Equal(t, 0, gwRepo.rateLimitCalls, "permission-denied 不是限流，不应写 rate_limit")
	})

	t.Run("free_usage_403_never_escalates", func(t *testing.T) {
		gwRepo := &grokQuotaAccountRepo{}
		rlRepo := &rateLimitAccountRepoStub{}
		rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
		// 计数器即便配置为"到阈值即禁用"，free-usage 分支也必须提前 return，不触碰升级逻辑。
		rl.SetOpenAI403CounterCache(&openAI403CounterCacheStub{counts: []int64{openAI403DisableThreshold}})
		svc := &OpenAIGatewayService{accountRepo: gwRepo, rateLimitService: rl}
		account := &Account{ID: 82, Platform: PlatformGrok, Type: AccountTypeOAuth,
			Credentials: map[string]any{"refresh_token": "rt-82"}}

		svc.handleGrokAccountUpstreamError(context.Background(), account,
			http.StatusForbidden, http.Header{},
			[]byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1033696/1000000."}`))

		require.Equal(t, 0, rlRepo.setErrorCalls, "free-usage-exhausted 是可恢复限流，绝不能永久禁用")
		require.Equal(t, 1, gwRepo.rateLimitCalls, "free-usage 应走限流冷却")
		require.Contains(t, gwRepo.updates[account.ID], grokQuotaSnapshotExtraKey)
	})

	t.Run("unauthorized_without_refresh_token_disables", func(t *testing.T) {
		gwRepo := &grokQuotaAccountRepo{}
		rlRepo := &rateLimitAccountRepoStub{}
		rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
		svc := &OpenAIGatewayService{accountRepo: gwRepo, rateLimitService: rl}
		account := &Account{ID: 83, Platform: PlatformGrok, Type: AccountTypeOAuth} // 无 refresh_token

		svc.handleGrokAccountUpstreamError(context.Background(), account,
			http.StatusUnauthorized, http.Header{}, []byte(`{"error":"unauthorized"}`))

		require.Equal(t, 1, rlRepo.setErrorCalls, "无 refresh_token 的 401 无法自愈，应永久禁用")
		require.Contains(t, rlRepo.lastErrorMsg, "refresh_token missing")
	})
}
