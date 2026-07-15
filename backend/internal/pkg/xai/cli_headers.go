package xai

import (
	"net/http"
	"net/url"
	"strings"
)

// Free Grok Build (cli-chat-proxy) requires Grok CLI client headers.
// Without them the upstream returns HTTP 426 "Grok CLI version (none) is outdated".
const (
	DefaultCLIClientVersion    = "0.2.93"
	DefaultCLIClientIdentifier = "grok-pager"
	DefaultCLIUserAgent        = "grok-pager/" + DefaultCLIClientVersion + " grok-shell/" + DefaultCLIClientVersion + " (linux; x86_64)"
	DefaultCLITokenAuth        = "xai-grok-cli"
	DefaultCLIAuthenticateResp = "authenticate-response"
)

// IsCLIChatProxyBaseURL reports whether baseURL targets the free Grok Build proxy.
func IsCLIChatProxyBaseURL(baseURL string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return strings.Contains(strings.ToLower(raw), "cli-chat-proxy.grok.com")
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	return host == "cli-chat-proxy.grok.com"
}

// ApplyGrokBuildHeaders sets the Grok Build client headers expected by xAI.
// Existing non-empty headers are preserved.
func ApplyGrokBuildHeaders(h http.Header) {
	if h == nil {
		return
	}
	if strings.TrimSpace(h.Get("User-Agent")) == "" || strings.HasPrefix(h.Get("User-Agent"), "sub2api-grok") {
		h.Set("User-Agent", DefaultCLIUserAgent)
	}
	if strings.TrimSpace(h.Get("x-grok-client-version")) == "" {
		h.Set("x-grok-client-version", DefaultCLIClientVersion)
	}
	if strings.TrimSpace(h.Get("x-grok-client-identifier")) == "" {
		h.Set("x-grok-client-identifier", DefaultCLIClientIdentifier)
	}
	if strings.TrimSpace(h.Get("x-xai-token-auth")) == "" {
		h.Set("x-xai-token-auth", DefaultCLITokenAuth)
	}
	if strings.TrimSpace(h.Get("x-authenticateresponse")) == "" {
		h.Set("x-authenticateresponse", DefaultCLIAuthenticateResp)
	}
}

// ApplyCLIChatProxyHeaders sets the minimum Grok CLI headers required by cli-chat-proxy.
// Existing non-empty headers are preserved.
func ApplyCLIChatProxyHeaders(h http.Header) {
	ApplyGrokBuildHeaders(h)
}

// MaybeApplyCLIChatProxyHeaders applies CLI headers only when baseURL is cli-chat-proxy.
func MaybeApplyCLIChatProxyHeaders(h http.Header, baseURL string) {
	if IsCLIChatProxyBaseURL(baseURL) {
		ApplyCLIChatProxyHeaders(h)
	}
}
