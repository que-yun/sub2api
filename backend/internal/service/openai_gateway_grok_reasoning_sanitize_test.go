package service

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeGrokVisibleReasoningJSON_ClearsSummaryKeepsMessage(t *testing.T) {
	body := []byte(`{
	  "model":"gpt-5.5",
	  "reasoning":{"effort":"high","summary":"detailed"},
	  "output":[
	    {"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"The user asked something long..."}]},
	    {"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"最终答案"}]}
	  ]
	}`)
	out, changed := sanitizeGrokVisibleReasoningJSON(body)
	if !changed {
		t.Fatal("expected changed")
	}
	if got := gjson.GetBytes(out, "reasoning.summary").String(); got != "auto" {
		t.Fatalf("reasoning.summary=%q, want auto", got)
	}
	if n := len(gjson.GetBytes(out, "output.0.summary").Array()); n != 0 {
		t.Fatalf("reasoning summary len=%d, want 0", n)
	}
	if got := gjson.GetBytes(out, "output.1.content.0.text").String(); got != "最终答案" {
		t.Fatalf("message text changed: %q", got)
	}
}

func TestSanitizeGrokVisibleReasoningSSELine_DropsSummaryDeltas(t *testing.T) {
	line := `data: {"type":"response.reasoning_summary_text.delta","delta":"The user"}`
	_, drop, _ := sanitizeGrokVisibleReasoningSSELine(line)
	if !drop {
		t.Fatal("expected drop summary delta")
	}

	eventLine := `event: response.reasoning_summary_text.delta`
	_, drop, _ = sanitizeGrokVisibleReasoningSSELine(eventLine)
	if !drop {
		t.Fatal("expected drop summary event line")
	}

	created := `data: {"type":"response.created","response":{"model":"gpt-5.5","reasoning":{"effort":"high","summary":"detailed"},"output":[]}}`
	out, drop, changed := sanitizeGrokVisibleReasoningSSELine(created)
	if drop {
		t.Fatal("should not drop response.created")
	}
	if !changed {
		t.Fatal("expected summary rewrite on response.created")
	}
	if !strings.Contains(out, `"summary":"auto"`) {
		t.Fatalf("expected summary auto, got %s", out)
	}
	if strings.Contains(out, `"summary":"detailed"`) {
		t.Fatalf("detailed still present: %s", out)
	}
}

func TestSanitizeGrokVisibleReasoningSSELine_ScrubsReasoningItemAdded(t *testing.T) {
	line := `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"thinking..."}]}}`
	out, drop, changed := sanitizeGrokVisibleReasoningSSELine(line)
	if drop {
		t.Fatal("should keep output_item.done")
	}
	if !changed {
		t.Fatal("expected item summary scrub")
	}
	if strings.Contains(out, "thinking...") {
		t.Fatalf("summary text still present: %s", out)
	}
	if !strings.Contains(out, `"summary":[]`) && !strings.Contains(out, `"summary": []`) {
		// sjson may emit compact []
		if gjson.Get(out[len("data: "):], "item.summary").String() != "[]" && len(gjson.Get(out[len("data: "):], "item.summary").Array()) != 0 {
			// parse properly
			data, _ := extractOpenAISSEDataLine(out)
			if n := len(gjson.Get(data, "item.summary").Array()); n != 0 {
				t.Fatalf("item.summary not empty: %s", out)
			}
		}
	}
}

func TestShouldSanitizeGrokVisibleReasoning(t *testing.T) {
	if shouldSanitizeGrokVisibleReasoning(nil) {
		t.Fatal("nil account")
	}
	if shouldSanitizeGrokVisibleReasoning(&Account{Platform: PlatformOpenAI}) {
		t.Fatal("openai should not sanitize")
	}
	if !shouldSanitizeGrokVisibleReasoning(&Account{Platform: PlatformGrok}) {
		t.Fatal("grok should sanitize")
	}
}

func TestSanitizeGrokResponsesReasoningSummary_RequestSide(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","reasoning":{"effort":"high","summary":"detailed"}}`)
	out, err := sanitizeGrokResponsesReasoningSummary(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "reasoning.summary").String(); got != "auto" {
		t.Fatalf("got %q", got)
	}
}
