package service

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// shouldSanitizeOpenAICompatibleResponsesModelInput limits Codex/OpenAI item
// normalization to third-party OpenAI-compatible API-key upstreams. Official
// OpenAI endpoints can receive their native Responses item types unchanged.
func shouldSanitizeOpenAICompatibleResponsesModelInput(account *Account) bool {
	if account == nil || !account.IsOpenAIApiKey() {
		return false
	}
	return !isOfficialOpenAIBaseURL(account.GetOpenAIBaseURL())
}

func isOfficialOpenAIBaseURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "api.openai.com"
}

func openAICompatibleResponsesBodyMayContainUnsupportedModelInput(body []byte) bool {
	for _, marker := range [][]byte{
		[]byte(`custom_tool_call`),
		[]byte(`custom_tool_call_output`),
		[]byte(`mcp_tool_call`),
		[]byte(`mcp_tool_call_output`),
		[]byte(`tool_search_call`),
		[]byte(`tool_search_output`),
		[]byte(`local_shell_call`),
		[]byte(`web_search_call`),
		[]byte(`file_search_call`),
		[]byte(`item_reference`),
		[]byte(`additional_tools`),
	} {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

func openAICompatibleResponsesBodyMayNeedCodexToolNormalization(body []byte) bool {
	for _, marker := range [][]byte{
		[]byte(`"local_shell"`),
		[]byte(`"shell"`),
		[]byte(`"additional_tools"`),
		[]byte(`"exec_command"`),
		[]byte(`"write_stdin"`),
		[]byte(`"tool_choice"`),
		[]byte(`"namespace"`),
	} {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

// sanitizeOpenAICompatibleResponsesModelInput normalizes Codex-only Responses
// history items for third-party OpenAI-compatible upstreams.
// Unlike the Grok path, function/custom tool call items keep namespace so the
// upstream 400 rejected-field retry can strip only the bad field.
func sanitizeOpenAICompatibleResponsesModelInput(body []byte) ([]byte, bool, error) {
	if !openAICompatibleResponsesBodyMayContainUnsupportedModelInput(body) {
		return body, false, nil
	}
	out, err := sanitizeOpenAICompatibleResponsesModelInputItems(body)
	if err != nil {
		return nil, false, err
	}
	return out, !bytes.Equal(out, body), nil
}

// prepareOpenAICompatibleResponsesBody applies the generic Codex/OpenAI
// compatibility transforms that third-party OpenAI-compatible upstreams need:
// 1) promote/normalize Codex shell tools into client-executable function tools
// 2) raise empty/auto tool_choice to required when client tools exist
// 3) sanitize unsupported Responses history item types
//
// Official api.openai.com is intentionally skipped.
//
// Image-generation tools are preserved across Codex tool filtering so billing
// resolution (model/size) still sees the original image_generation declaration.
// Codex-only input fields such as custom_tool_call.namespace are also kept for
// the rejected-field same-account retry path.
func prepareOpenAICompatibleResponsesBody(account *Account, body []byte) ([]byte, bool, error) {
	if !shouldSanitizeOpenAICompatibleResponsesModelInput(account) {
		return body, false, nil
	}

	original := body
	changed := false
	preservedImageTools := extractOpenAICompatibleImageGenerationTools(body)

	if openAICompatibleResponsesBodyMayNeedCodexToolNormalization(body) {
		// Reuse the Codex shell/tool normalization already proven on the Grok path.
		// For third-party OpenAI-compatible providers this is the generic convert
		// layer: local_shell/shell/namespace wrappers become function tools that
		// clients can actually execute.
		out, err := sanitizeGrokResponsesTools(body)
		if err != nil {
			return nil, false, err
		}
		if !bytes.Equal(out, body) {
			body = out
			changed = true
		}
		out, err = requireGrokResponsesFunctionToolChoice(body)
		if err != nil {
			return nil, false, err
		}
		if !bytes.Equal(out, body) {
			body = out
			changed = true
		}
	}

	out, inputChanged, err := sanitizeOpenAICompatibleResponsesModelInput(body)
	if err != nil {
		return nil, false, err
	}
	if inputChanged {
		body = out
		changed = true
	}

	if restored, restoreChanged, restoreErr := restoreOpenAICompatibleImageGenerationTools(body, preservedImageTools); restoreErr != nil {
		return nil, false, restoreErr
	} else if restoreChanged {
		body = restored
		changed = true
	}

	if !changed {
		return original, false, nil
	}
	return body, true, nil
}

// normalizeOpenAICompatibleCodexToolsOnly is a narrow helper for chat-fallback
// paths that already convert Responses->ChatCompletions. It still needs the
// shell/tool rewrite before bridge conversion so local_shell becomes an
// executable function tool.
func normalizeOpenAICompatibleCodexToolsOnly(body []byte) ([]byte, bool, error) {
	if !openAICompatibleResponsesBodyMayNeedCodexToolNormalization(body) {
		return body, false, nil
	}
	out, err := sanitizeGrokResponsesTools(body)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(out, body) {
		// still try tool_choice lift when tools already look like functions
		out2, err := requireGrokResponsesFunctionToolChoice(out)
		if err != nil {
			return nil, false, err
		}
		return out2, !bytes.Equal(out2, body), nil
	}
	out, err = requireGrokResponsesFunctionToolChoice(out)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}


// extractOpenAICompatibleImageGenerationTools captures native image_generation
// tool declarations before Codex/Grok tool filtering drops unsupported types.
func extractOpenAICompatibleImageGenerationTools(body []byte) [][]byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return nil
	}
	var preserved [][]byte
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) != "image_generation" {
			continue
		}
		preserved = append(preserved, []byte(tool.Raw))
	}
	return preserved
}

// restoreOpenAICompatibleImageGenerationTools re-attaches preserved image tools
// after Codex shell/tool filtering. Billing resolution reads model/size from
// these declarations; dropping them collapses billing_model to the text model.
func restoreOpenAICompatibleImageGenerationTools(body []byte, preserved [][]byte) ([]byte, bool, error) {
	if len(preserved) == 0 {
		return body, false, nil
	}
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
				return body, false, nil
			}
		}
	}

	merged := make([]json.RawMessage, 0, len(preserved)+8)
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			merged = append(merged, json.RawMessage(tool.Raw))
		}
	}
	for _, tool := range preserved {
		merged = append(merged, json.RawMessage(tool))
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, false, err
	}
	out, err := sjson.SetRawBytes(body, "tools", encoded)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// sanitizeOpenAICompatibleResponsesModelInputItems mirrors the Grok ModelInput
// cleanup but keeps namespace on function/custom tool calls so third-party
// providers can reject and retry only that field.
func sanitizeOpenAICompatibleResponsesModelInputItems(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}

	items := input.Array()
	kept := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, item := range items {
		raw, keep, itemChanged, err := normalizeOpenAICompatibleResponsesModelInputItem(item)
		if err != nil {
			return nil, err
		}
		if itemChanged {
			changed = true
		}
		if !keep {
			changed = true
			continue
		}
		kept = append(kept, raw)
	}
	if !changed {
		return body, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "input", encoded)
}

func normalizeOpenAICompatibleResponsesModelInputItem(item gjson.Result) (json.RawMessage, bool, bool, error) {
	if !item.IsObject() {
		return json.RawMessage(item.Raw), true, false, nil
	}

	// Preserve Codex namespace across type rewrites so third-party providers can
	// reject input[n].namespace and the same-account retry can strip only that
	// field without losing the tool call itself.
	namespace := ""
	if ns := item.Get("namespace"); ns.Exists() && ns.Type == gjson.String {
		namespace = strings.TrimSpace(ns.String())
	}

	raw, keep, changed, err := normalizeGrokResponsesModelInputItem(item)
	if err != nil || !keep || namespace == "" {
		return raw, keep, changed, err
	}

	// Only re-attach namespace on tool-call shaped items that the rejected-field
	// retry path knows how to edit.
	itemType := strings.TrimSpace(gjson.GetBytes(raw, "type").String())
	switch itemType {
	case "function_call", "tool_call", "custom_tool_call", "mcp_tool_call":
	default:
		return raw, keep, changed, nil
	}
	if gjson.GetBytes(raw, "namespace").Exists() {
		return raw, keep, changed, nil
	}
	out, setErr := sjson.SetBytes(raw, "namespace", namespace)
	if setErr != nil {
		return raw, keep, changed, setErr
	}
	return out, keep, true, nil
}
