package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGrokCodexLocalShellToolsConvertedToFunctions(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.5",
		"input": "hello",
		"tools": [
			{"type": "shell", "name": "local_shell"},
			{"type": "local_shell"},
			{"type": "namespace", "namespace": "functions", "tools": [
				{"type": "function", "name": "wait", "parameters": {"type": "object"}}
			]}
		],
		"tool_choice": "auto"
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	names := make([]string, 0)
	for _, tool := range gjson.GetBytes(patched, "tools").Array() {
		names = append(names, tool.Get("type").String()+":"+tool.Get("name").String())
	}
	require.ElementsMatch(t, []string{"function:exec_command", "function:wait"}, names)
	require.Equal(t, "required", gjson.GetBytes(patched, "tool_choice").String())
}

func TestGrokAdditionalToolsPromotedToTopLevel(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.5",
		"input": [
			{
				"type": "additional_tools",
				"role": "developer",
				"tools": [
					{"type": "local_shell"},
					{"type": "function", "name": "write_stdin", "parameters": {"type": "object", "properties": {"chars": {"type": "string"}}}}
				]
			},
			{"type": "message", "role": "user", "content": "run ls"}
		],
		"tool_choice": "auto"
	}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.True(t, json.Valid(patched))
	require.False(t, gjson.GetBytes(patched, `input.#(type=="additional_tools")`).Exists())
	names := make([]string, 0)
	for _, tool := range gjson.GetBytes(patched, "tools").Array() {
		names = append(names, tool.Get("type").String()+":"+tool.Get("name").String())
	}
	require.ElementsMatch(t, []string{"function:exec_command", "function:write_stdin"}, names)
	require.Equal(t, "required", gjson.GetBytes(patched, "tool_choice").String())
}
