//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCoerceWholeNumberFloatsInJSONArguments(t *testing.T) {
	got, ok := coerceWholeNumberFloatsInJSONArguments(`{"yield_time_ms":1000.0,"session_id":12.0,"keep":1.5,"nested":{"max_output_tokens":200000.0}}`)
	require.True(t, ok)
	require.Equal(t, int64(1000), gjson.Get(got, "yield_time_ms").Int())
	require.Equal(t, int64(12), gjson.Get(got, "session_id").Int())
	require.Equal(t, 1.5, gjson.Get(got, "keep").Float())
	require.Equal(t, int64(200000), gjson.Get(got, "nested.max_output_tokens").Int())
	// already integer-only
	_, ok = coerceWholeNumberFloatsInJSONArguments(`{"yield_time_ms":1000}`)
	require.False(t, ok)
}

func TestNormalizeOpenAIResponsesFunctionCallArgumentsCoercesFloats(t *testing.T) {
	payload := []byte(`{"type":"response.function_call_arguments.done","call_id":"call_a","name":"wait","arguments":"{\"cell_id\":\"x\",\"yield_time_ms\":1000.0}"}`)
	out, ok := normalizeOpenAIResponsesFunctionCallArguments(payload)
	require.True(t, ok)
	args := gjson.GetBytes(out, "arguments").String()
	require.Equal(t, int64(1000), gjson.Get(args, "yield_time_ms").Int())
	require.NotContains(t, args, "1000.0")

	item := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_b","name":"write_stdin","arguments":"{\"session_id\":71904.0,\"chars\":\"x\"}"}}`)
	out, ok = normalizeOpenAIResponsesFunctionCallArguments(item)
	require.True(t, ok)
	args = gjson.GetBytes(out, "item.arguments").String()
	require.Equal(t, int64(71904), gjson.Get(args, "session_id").Int())
}

func TestMarshalGrokCodexCanonicalFunctionToolUsesIntegerTypes(t *testing.T) {
	raw, ok := marshalGrokCodexCanonicalFunctionTool("exec_command", gjson.Parse(`{"type":"function","name":"exec_command"}`))
	require.True(t, ok)
	require.Equal(t, "integer", gjson.GetBytes(raw, "parameters.properties.yield_time_ms.type").String())
	require.Equal(t, "integer", gjson.GetBytes(raw, "parameters.properties.max_output_tokens.type").String())

	raw, ok = marshalGrokCodexCanonicalFunctionTool("write_stdin", gjson.Parse(`{"type":"function","name":"write_stdin"}`))
	require.True(t, ok)
	require.Equal(t, "integer", gjson.GetBytes(raw, "parameters.properties.session_id.type").String())
	require.Equal(t, "integer", gjson.GetBytes(raw, "parameters.properties.yield_time_ms.type").String())
}
