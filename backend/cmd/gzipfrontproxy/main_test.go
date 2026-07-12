package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProxyServerBuildUpstreamRequest_CompressesEligibleJSON(t *testing.T) {
	cfg := mustTestConfig(t)
	srv := &proxyServer{cfg: cfg}

	payload := strings.Repeat("long-context-", 1<<13)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?x=1", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")

	upstreamReq, stats, err := srv.buildUpstreamRequest(req)
	require.NoError(t, err)
	require.True(t, stats.Compressed)
	require.Less(t, stats.SentBytes, stats.OriginalBytes)
	require.Equal(t, "gzip", upstreamReq.Header.Get("Content-Encoding"))
	require.Equal(t, frontProxyID, upstreamReq.Header.Get(frontProxyHeader))
	require.Equal(t, "true", upstreamReq.Header.Get(frontProxyCompressed))
	require.Equal(t, strconv.Itoa(stats.OriginalBytes), upstreamReq.Header.Get(frontProxyOriginal))
	require.Equal(t, strconv.Itoa(stats.SentBytes), upstreamReq.Header.Get(frontProxySent))
	require.Equal(t, "Bearer test", upstreamReq.Header.Get("Authorization"))
	require.Equal(t, "/v1/responses", upstreamReq.URL.Path)
	require.Equal(t, "x=1", upstreamReq.URL.RawQuery)

	gr, err := gzip.NewReader(upstreamReq.Body)
	require.NoError(t, err)
	defer gr.Close()
	raw, err := io.ReadAll(gr)
	require.NoError(t, err)
	require.Equal(t, payload, string(raw))
}

func TestProxyServerBuildUpstreamRequest_SkipsSmallBody(t *testing.T) {
	cfg := mustTestConfig(t)
	srv := &proxyServer{cfg: cfg}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"ok":true}`))
	req.Header.Set("Content-Type", "application/json")

	upstreamReq, stats, err := srv.buildUpstreamRequest(req)
	require.NoError(t, err)
	require.False(t, stats.Compressed)
	require.Equal(t, frontProxyID, upstreamReq.Header.Get(frontProxyHeader))
	require.Equal(t, "false", upstreamReq.Header.Get(frontProxyCompressed))

	raw, err := io.ReadAll(upstreamReq.Body)
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(raw))
	require.Empty(t, upstreamReq.Header.Get("Content-Encoding"))
}

func TestProxyServerBuildUpstreamRequest_SkipsNonEligiblePath(t *testing.T) {
	cfg := mustTestConfig(t)
	srv := &proxyServer{cfg: cfg}

	req := httptest.NewRequest(http.MethodPost, "/health", strings.NewReader(strings.Repeat("x", 1<<20)))
	req.Header.Set("Content-Type", "application/json")

	_, stats, err := srv.buildUpstreamRequest(req)
	require.NoError(t, err)
	require.False(t, stats.Compressed)
}

func TestProxyServerBuildUpstreamRequest_RejectsOversizedBuffer(t *testing.T) {
	cfg := mustTestConfig(t)
	cfg.MaxBufferBytes = 8
	srv := &proxyServer{cfg: cfg}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(strings.Repeat("x", 32)))
	req.Header.Set("Content-Type", "application/json")

	_, _, err := srv.buildUpstreamRequest(req)
	require.Error(t, err)
	var maxErr *http.MaxBytesError
	require.ErrorAs(t, err, &maxErr)
	require.EqualValues(t, 8, maxErr.Limit)
}

func TestProxyServerHandle_ProxiesResponse(t *testing.T) {
	type capture struct {
		ContentEncoding string `json:"content_encoding"`
		Body            string `json:"body"`
	}
	captured := capture{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.ContentEncoding = r.Header.Get("Content-Encoding")
		var body []byte
		var err error
		if captured.ContentEncoding == "gzip" {
			gr, gzipErr := gzip.NewReader(r.Body)
			require.NoError(t, gzipErr)
			defer gr.Close()
			body, err = io.ReadAll(gr)
		} else {
			body, err = io.ReadAll(r.Body)
		}
		require.NoError(t, err)
		captured.Body = string(body)
		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("done"))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)

	srv := &proxyServer{
		cfg: config{
			UpstreamBaseURL:  upstreamURL,
			CompressMinBytes: 32,
			MaxBufferBytes:   1 << 20,
			CompressPaths:    defaultCompressPaths,
		},
		httpClient: upstream.Client(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(strings.Repeat("long-", 128)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handle(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "ok", rec.Header().Get("X-Upstream"))
	require.Equal(t, "done", rec.Body.String())
	require.Equal(t, "gzip", captured.ContentEncoding)
	require.True(t, strings.Contains(captured.Body, "long-long-"))
}

func TestProxyServerHandle_SSEHeartbeatInjectsKeepAlive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		flusher.Flush()

		time.Sleep(35 * time.Millisecond)
		_, err := io.WriteString(w, "data: hello\n\n")
		require.NoError(t, err)
		flusher.Flush()
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)

	srv := &proxyServer{
		cfg: config{
			UpstreamBaseURL:      upstreamURL,
			SSEHeartbeatInterval: 10 * time.Millisecond,
		},
		httpClient: upstream.Client(),
	}

	proxy := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer proxy.Close()

	resp, err := proxy.Client().Get(proxy.URL + "/v1/responses")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), ": keep-alive\n\n")
	require.Contains(t, string(body), "data: hello\n\n")
}

func TestProxyServerHandle_NonSSEResponseDoesNotInjectHeartbeat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err := io.WriteString(w, `{"ok":true}`)
		require.NoError(t, err)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)

	srv := &proxyServer{
		cfg: config{
			UpstreamBaseURL:      upstreamURL,
			SSEHeartbeatInterval: 10 * time.Millisecond,
		},
		httpClient: upstream.Client(),
	}

	proxy := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer proxy.Close()

	resp, err := proxy.Client().Get(proxy.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotContains(t, string(body), ": keep-alive\n\n")
	require.JSONEq(t, `{"ok":true}`, string(body))
}

func TestSplitCSV(t *testing.T) {
	out := splitCSV(" /v1/responses, /responses ,,")
	require.Equal(t, []string{"/v1/responses", "/responses"}, out)
}

func TestLoadConfig_DisableKeepAlivesFromEnv(t *testing.T) {
	t.Setenv("GZIP_FRONT_PROXY_UPSTREAM", "http://127.0.0.1:6780")
	t.Setenv("GZIP_FRONT_PROXY_DISABLE_KEEPALIVES", "true")

	cfg, err := loadConfig()
	require.NoError(t, err)
	require.True(t, cfg.DisableKeepAlives)
}

func mustTestConfig(t *testing.T) config {
	t.Helper()
	u, err := url.Parse("http://127.0.0.1:6780")
	require.NoError(t, err)
	return config{
		UpstreamBaseURL:      u,
		CompressMinBytes:     128,
		MaxBufferBytes:       1 << 20,
		CompressPaths:        defaultCompressPaths,
		SSEHeartbeatInterval: defaultSSEHeartbeat,
	}
}
