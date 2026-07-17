package service

import (
	"bytes"
	"net/url"
	"strings"
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
func sanitizeOpenAICompatibleResponsesModelInput(body []byte) ([]byte, bool, error) {
	if !openAICompatibleResponsesBodyMayContainUnsupportedModelInput(body) {
		return body, false, nil
	}
	out, err := sanitizeGrokResponsesModelInput(body)
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
func prepareOpenAICompatibleResponsesBody(account *Account, body []byte) ([]byte, bool, error) {
	if !shouldSanitizeOpenAICompatibleResponsesModelInput(account) {
		return body, false, nil
	}

	original := body
	changed := false

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
