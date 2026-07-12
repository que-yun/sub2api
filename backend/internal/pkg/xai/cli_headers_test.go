package xai_test

import (
  "net/http"
  "testing"

  "github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

func TestIsCLIChatProxyBaseURL(t *testing.T) {
  if !xai.IsCLIChatProxyBaseURL("https://cli-chat-proxy.grok.com/v1") {
    t.Fatal("expected true")
  }
  if xai.IsCLIChatProxyBaseURL("https://api.x.ai/v1") {
    t.Fatal("expected false")
  }
}

func TestApplyCLIChatProxyHeaders(t *testing.T) {
  h := http.Header{}
  h.Set("User-Agent", "sub2api-grok/1.0")
  xai.MaybeApplyCLIChatProxyHeaders(h, "https://cli-chat-proxy.grok.com/v1")
  if h.Get("User-Agent") != xai.DefaultCLIUserAgent {
    t.Fatalf("ua=%s", h.Get("User-Agent"))
  }
  if h.Get("x-grok-client-version") == "" {
    t.Fatal("missing version")
  }
}
