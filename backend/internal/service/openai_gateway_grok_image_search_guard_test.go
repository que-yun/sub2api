package service

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func imageResponsesBody() []byte {
	return []byte(`{"model":"grok-4.5","input":[` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"existing dev"}]},` +
		`{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"这是什么"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,AAAA"}` +
		`]}]}`)
}

func TestAppendGrokImageSearchGuard_InjectsAfterLeadingDeveloper(t *testing.T) {
	out := appendGrokImageSearchGuard(imageResponsesBody())

	input := gjson.GetBytes(out, "input")
	if got := int(input.Get("#").Int()); got != 3 {
		t.Fatalf("expected 3 input items after injection, got %d: %s", got, out)
	}
	// 前导 developer 原样保留在 index 0。
	if input.Get("0.content.0.text").String() != "existing dev" {
		t.Fatalf("leading developer message not preserved: %s", input.Get("0").Raw)
	}
	// guard 插在 index 1（前导 developer 之后）。
	if input.Get("1.role").String() != "developer" {
		t.Fatalf("guard should be a developer message, got role=%q", input.Get("1.role").String())
	}
	if !strings.Contains(input.Get("1").Raw, grokImageSearchGuardMarker) {
		t.Fatalf("guard marker missing at index 1: %s", input.Get("1").Raw)
	}
	// 原用户消息（含图片）后移到 index 2，未被破坏。
	if input.Get("2.role").String() != "user" {
		t.Fatalf("user message should move to index 2, got role=%q", input.Get("2.role").String())
	}
	if input.Get("2.content.1.type").String() != "input_image" {
		t.Fatalf("user image part lost after injection: %s", input.Get("2").Raw)
	}
}

func TestAppendGrokImageSearchGuard_NoImageIsNoop(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"北京今天天气"}]}]}`)
	out := appendGrokImageSearchGuard(body)
	if strings.Contains(string(out), grokImageSearchGuardMarker) {
		t.Fatalf("guard should not be injected without an image: %s", out)
	}
	if string(out) != string(body) {
		t.Fatalf("no-image body should be unchanged\nwant: %s\ngot:  %s", body, out)
	}
}

func TestAppendGrokImageSearchGuard_Idempotent(t *testing.T) {
	once := appendGrokImageSearchGuard(imageResponsesBody())
	twice := appendGrokImageSearchGuard(once)
	if got := strings.Count(string(twice), grokImageSearchGuardMarker); got != 1 {
		t.Fatalf("marker should appear exactly once after double injection, got %d", got)
	}
	if int(gjson.GetBytes(twice, "input.#").Int()) != 3 {
		t.Fatalf("second injection must not add another item: %s", twice)
	}
}

func TestAppendGrokImageSearchGuard_StringInputIsNoop(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":"describe the image"}`)
	out := appendGrokImageSearchGuard(body)
	if string(out) != string(body) {
		t.Fatalf("string input should be unchanged: %s", out)
	}
}
