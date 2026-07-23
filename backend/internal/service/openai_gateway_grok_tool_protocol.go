package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const grokResponsesClientToolMappingContextKey = "grok_responses_client_tool_mapping"

// grokDefaultToolNamespace is the Codex reserved namespace for ordinary client
// function tools. Unlike a real MCP/plugin namespace (e.g. "collaboration"),
// its children are plain functions, so the Grok fork keeps them unqualified
// instead of letting apicompat rewrite them to "functions__<name>".
const grokDefaultToolNamespace = "functions"

func adaptGrokResponsesClientTools(body []byte) ([]byte, apicompat.ResponsesClientToolMapping, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var requestBody map[string]any
	if err := decoder.Decode(&requestBody); err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, fmt.Errorf("decode Grok Responses client tools: %w", err)
	}

	// Lower the reserved "functions" default namespace to bare-named function
	// tools before apicompat runs. apicompat would otherwise prefix them as
	// "functions__<name>"; real namespaces (collaboration, image_gen, ...) are
	// left for apicompat so their qualified identity and restoration mapping
	// stay intact.
	preChanged := flattenGrokDefaultNamespaceTools(requestBody)

	mapping, changed, err := apicompat.AdaptResponsesClientTools(requestBody)
	if err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, err
	}
	if !changed && !preChanged {
		return body, mapping, nil
	}
	rebuilt, err := marshalOpenAIUpstreamJSON(requestBody)
	if err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, fmt.Errorf("encode Grok Responses client tools: %w", err)
	}
	return rebuilt, mapping, nil
}

// flattenGrokDefaultNamespaceTools lifts children of the reserved "functions"
// namespace into bare-named top-level function tools. A tool_choice qualified
// with that namespace can no longer bind to a specific child, so it falls back
// to the first plain top-level function tool (or is dropped when none exists).
// Real namespaces are left untouched. Returns true when the request changed.
func flattenGrokDefaultNamespaceTools(req map[string]any) bool {
	tools, ok := req["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}
	flattenedDefault := false
	firstFunctionName := ""
	flattened := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			flattened = append(flattened, raw)
			continue
		}
		typ := strings.TrimSpace(grokToolStringField(tool["type"]))
		if typ == "namespace" && grokToolNamespaceName(tool) == grokDefaultToolNamespace {
			flattenedDefault = true
			for _, rawChild := range grokNamespaceToolChildren(tool) {
				child, ok := rawChild.(map[string]any)
				if !ok || strings.TrimSpace(grokToolStringField(child["type"])) != "function" {
					continue
				}
				flattened = append(flattened, rawChild)
			}
			continue
		}
		if typ == "function" && firstFunctionName == "" {
			if name := strings.TrimSpace(grokToolStringField(tool["name"])); name != "" {
				firstFunctionName = name
			}
		}
		flattened = append(flattened, raw)
	}
	if !flattenedDefault {
		return false
	}
	req["tools"] = flattened

	choice, ok := req["tool_choice"].(map[string]any)
	if !ok {
		return true
	}
	choiceNamespace := strings.TrimSpace(grokToolStringField(choice["namespace"]))
	if strings.TrimSpace(grokToolStringField(choice["type"])) == "namespace" {
		choiceNamespace = grokToolNamespaceName(choice)
	}
	if choiceNamespace != grokDefaultToolNamespace {
		return true
	}
	if firstFunctionName != "" {
		req["tool_choice"] = map[string]any{"type": "function", "name": firstFunctionName}
	} else {
		delete(req, "tool_choice")
	}
	return true
}

func grokToolStringField(value any) string {
	text, _ := value.(string)
	return text
}

func grokToolNamespaceName(tool map[string]any) string {
	if name := strings.TrimSpace(grokToolStringField(tool["name"])); name != "" {
		return name
	}
	return strings.TrimSpace(grokToolStringField(tool["namespace"]))
}

func grokNamespaceToolChildren(tool map[string]any) []any {
	if children, ok := tool["tools"].([]any); ok && len(children) > 0 {
		return children
	}
	children, _ := tool["children"].([]any)
	return children
}

func hasGrokResponsesClientToolMapping(mapping apicompat.ResponsesClientToolMapping) bool {
	return len(mapping.CustomTools) > 0 || mapping.ToolSearch || len(mapping.NamespaceTools) > 0
}

func setGrokResponsesClientToolMapping(c *gin.Context, mapping apicompat.ResponsesClientToolMapping) {
	if c == nil {
		return
	}
	if !hasGrokResponsesClientToolMapping(mapping) {
		clearGrokResponsesClientToolMapping(c)
		return
	}
	c.Set(grokResponsesClientToolMappingContextKey, mapping)
}

func clearGrokResponsesClientToolMapping(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get(grokResponsesClientToolMappingContextKey); !exists {
		return
	}
	c.Set(grokResponsesClientToolMappingContextKey, apicompat.ResponsesClientToolMapping{})
}

func grokResponsesClientToolMapping(c *gin.Context) (apicompat.ResponsesClientToolMapping, bool) {
	if c == nil {
		return apicompat.ResponsesClientToolMapping{}, false
	}
	value, ok := c.Get(grokResponsesClientToolMappingContextKey)
	if !ok {
		return apicompat.ResponsesClientToolMapping{}, false
	}
	mapping, ok := value.(apicompat.ResponsesClientToolMapping)
	return mapping, ok && hasGrokResponsesClientToolMapping(mapping)
}

func restoreGrokResponsesClientToolPayload(c *gin.Context, payload []byte) ([]byte, error) {
	mapping, ok := grokResponsesClientToolMapping(c)
	if !ok || !bytes.Contains(payload, []byte(`"function_call"`)) || !json.Valid(payload) {
		return payload, nil
	}
	restored, _, err := apicompat.RestoreResponsesClientToolPayload(payload, mapping)
	return restored, err
}

type grokResponsesClientToolStreamBody struct {
	*io.PipeReader
	source io.Closer
}

func (b *grokResponsesClientToolStreamBody) Close() error {
	readerErr := b.PipeReader.Close()
	sourceErr := b.source.Close()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

func newGrokResponsesClientToolStreamBody(
	source io.ReadCloser,
	mapping apicompat.ResponsesClientToolMapping,
	maxLineSize int,
) io.ReadCloser {
	reader, writer := io.Pipe()
	body := &grokResponsesClientToolStreamBody{PipeReader: reader, source: source}
	go transformGrokResponsesClientToolStream(source, writer, mapping, maxLineSize)
	return body
}

func transformGrokResponsesClientToolStream(
	source io.ReadCloser,
	destination *io.PipeWriter,
	mapping apicompat.ResponsesClientToolMapping,
	maxLineSize int,
) {
	defer func() { _ = source.Close() }()
	if maxLineSize <= 0 {
		maxLineSize = defaultMaxLineSize
	}

	scanner := bufio.NewScanner(source)
	scanBuf := getSSEScannerBuf64K()
	defer putSSEScannerBuf64K(scanBuf)
	scanner.Buffer(scanBuf[:0], maxLineSize)
	documents := newOpenAISSEJSONDocumentScanner(scanner)
	restorer := apicompat.NewResponsesClientToolStreamRestorer(mapping)
	buffered := bufio.NewWriterSize(destination, 4*1024)
	pendingFields := make([]string, 0, 2)
	frameHadEventField := false
	frameEmitted := false

	writeLine := func(line string) error {
		if _, err := buffered.WriteString(line); err != nil {
			return err
		}
		return buffered.WriteByte('\n')
	}
	writePendingFields := func(payload []byte, includeNonEvent bool) error {
		eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
		for _, field := range pendingFields {
			if _, isEvent := extractOpenAISSEEventLine(field); isEvent {
				if eventType != "" {
					if err := writeLine("event: " + eventType); err != nil {
						return err
					}
				} else if err := writeLine(field); err != nil {
					return err
				}
				continue
			}
			if includeNonEvent {
				if err := writeLine(field); err != nil {
					return err
				}
			}
		}
		return nil
	}
	writePayloads := func(payloads [][]byte) error {
		for index, payload := range payloads {
			if index == 0 {
				if err := writePendingFields(payload, true); err != nil {
					return err
				}
			} else if frameHadEventField {
				eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
				if eventType != "" {
					if err := writeLine("event: " + eventType); err != nil {
						return err
					}
				}
			}
			if err := writeLine("data: " + string(payload)); err != nil {
				return err
			}
			if err := writeLine(""); err != nil {
				return err
			}
		}
		return buffered.Flush()
	}

	for documents.Scan() {
		line := documents.Text()
		data, isData := extractOpenAISSEDataLine(line)
		if isData {
			payload := []byte(data)
			payloads := [][]byte{payload}
			if json.Valid(payload) {
				var err error
				payloads, _, err = restorer.RestoreEvent(payload)
				if err != nil {
					_ = buffered.Flush()
					_ = destination.CloseWithError(fmt.Errorf("restore Grok Responses client tool event: %w", err))
					return
				}
			}
			if err := writePayloads(payloads); err != nil {
				_ = destination.CloseWithError(err)
				return
			}
			pendingFields = pendingFields[:0]
			frameHadEventField = false
			frameEmitted = true
			continue
		}

		if line == "" {
			if !frameEmitted {
				for _, field := range pendingFields {
					if err := writeLine(field); err != nil {
						_ = destination.CloseWithError(err)
						return
					}
				}
				if len(pendingFields) > 0 {
					if err := writeLine(""); err != nil {
						_ = destination.CloseWithError(err)
						return
					}
					if err := buffered.Flush(); err != nil {
						_ = destination.CloseWithError(err)
						return
					}
				}
			}
			pendingFields = pendingFields[:0]
			frameHadEventField = false
			frameEmitted = false
			continue
		}

		if _, isEvent := extractOpenAISSEEventLine(line); isEvent {
			frameHadEventField = true
		}
		pendingFields = append(pendingFields, line)
	}

	for _, field := range pendingFields {
		if err := writeLine(field); err != nil {
			_ = destination.CloseWithError(err)
			return
		}
	}
	if err := buffered.Flush(); err != nil {
		_ = destination.CloseWithError(err)
		return
	}
	if err := documents.Err(); err != nil {
		_ = destination.CloseWithError(err)
		return
	}
	_ = destination.Close()
}
