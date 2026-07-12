package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultListenAddr     = "127.0.0.1:6791"
	defaultUpstreamBase   = "http://127.0.0.1:6780"
	defaultCompressMin    = 64 << 10
	defaultBufferLimit    = 8 << 20
	defaultSSEHeartbeat   = 10 * time.Second
	defaultReadTimeout    = 30 * time.Second
	defaultWriteTimeout   = 0 * time.Second
	defaultIdleTimeout    = 75 * time.Second
	defaultShutdownTimout = 10 * time.Second
	frontProxyID          = "gzipfrontproxy"
	frontProxyHeader      = "X-Sub2api-Front-Proxy"
	frontProxyReadMs      = "X-Sub2api-Front-Proxy-Read-Ms"
	frontProxyOriginal    = "X-Sub2api-Front-Proxy-Original-Bytes"
	frontProxySent        = "X-Sub2api-Front-Proxy-Sent-Bytes"
	frontProxyCompressed  = "X-Sub2api-Front-Proxy-Compressed"
)

var defaultCompressPaths = []string{
	"/v1/responses",
	"/responses",
	"/v1/chat/completions",
	"/chat/completions",
}

type config struct {
	ListenAddr           string
	UpstreamBaseURL      *url.URL
	CompressMinBytes     int64
	MaxBufferBytes       int64
	CompressPaths        []string
	DisableKeepAlives    bool
	SSEHeartbeatInterval time.Duration
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	IdleTimeout          time.Duration
}

type proxyServer struct {
	cfg        config
	httpClient *http.Client
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	srv := &proxyServer{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: buildUpstreamTransport(cfg),
		},
	}

	server := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      http.HandlerFunc(srv.handle),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	log.Printf("gzip-front-proxy listen=%s upstream=%s min_bytes=%d max_buffer=%d disable_keepalives=%v sse_heartbeat=%s paths=%s",
		cfg.ListenAddr, cfg.UpstreamBaseURL.String(), cfg.CompressMinBytes, cfg.MaxBufferBytes, cfg.DisableKeepAlives, cfg.SSEHeartbeatInterval, strings.Join(cfg.CompressPaths, ","))

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("received signal=%s, shutting down", sig)
	case err := <-errCh:
		log.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func loadConfig() (config, error) {
	upstreamBase := getenvDefault("GZIP_FRONT_PROXY_UPSTREAM", defaultUpstreamBase)
	upstreamURL, err := url.Parse(upstreamBase)
	if err != nil {
		return config{}, fmt.Errorf("parse GZIP_FRONT_PROXY_UPSTREAM: %w", err)
	}
	if upstreamURL.Scheme == "" || upstreamURL.Host == "" {
		return config{}, fmt.Errorf("invalid GZIP_FRONT_PROXY_UPSTREAM: %s", upstreamBase)
	}

	compressMinBytes, err := getenvInt64("GZIP_FRONT_PROXY_MIN_BYTES", defaultCompressMin)
	if err != nil {
		return config{}, err
	}
	maxBufferBytes, err := getenvInt64("GZIP_FRONT_PROXY_MAX_BUFFER_BYTES", defaultBufferLimit)
	if err != nil {
		return config{}, err
	}

	return config{
		ListenAddr:           getenvDefault("GZIP_FRONT_PROXY_LISTEN", defaultListenAddr),
		UpstreamBaseURL:      upstreamURL,
		CompressMinBytes:     compressMinBytes,
		MaxBufferBytes:       maxBufferBytes,
		CompressPaths:        splitCSV(getenvDefault("GZIP_FRONT_PROXY_PATHS", strings.Join(defaultCompressPaths, ","))),
		DisableKeepAlives:    getenvBool("GZIP_FRONT_PROXY_DISABLE_KEEPALIVES", false),
		SSEHeartbeatInterval: getenvDuration("GZIP_FRONT_PROXY_SSE_HEARTBEAT_INTERVAL", defaultSSEHeartbeat),
		ReadTimeout:          getenvDuration("GZIP_FRONT_PROXY_READ_TIMEOUT", defaultReadTimeout),
		WriteTimeout:         getenvDuration("GZIP_FRONT_PROXY_WRITE_TIMEOUT", defaultWriteTimeout),
		IdleTimeout:          getenvDuration("GZIP_FRONT_PROXY_IDLE_TIMEOUT", defaultIdleTimeout),
	}, nil
}

func buildUpstreamTransport(cfg config) http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	transport := base.Clone()
	transport.DisableKeepAlives = cfg.DisableKeepAlives
	return transport
}

func (p *proxyServer) handle(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()

	upstreamReq, stats, err := p.buildUpstreamRequest(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.As(err, new(*http.MaxBytesError)) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}

	resp, err := p.httpClient.Do(upstreamReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("forward request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	useHeartbeat := p.shouldProxySSEWithHeartbeat(w, resp)
	if useHeartbeat {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)
	if useHeartbeat {
		if err := p.copySSEWithHeartbeat(r.Context(), w, resp.Body); err != nil {
			log.Printf("copy upstream sse failed path=%s err=%v", r.URL.Path, err)
			return
		}
	} else if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("copy upstream response failed path=%s err=%v", r.URL.Path, err)
		return
	}

	log.Printf("gzip-front-proxy request method=%s path=%s compressed=%v original_bytes=%d sent_bytes=%d read_ms=%d total_ms=%d ua=%q",
		r.Method, r.URL.Path, stats.Compressed, stats.OriginalBytes, stats.SentBytes, stats.ReadElapsed.Milliseconds(), time.Since(startedAt).Milliseconds(), r.UserAgent())
}

func (p *proxyServer) shouldProxySSEWithHeartbeat(w http.ResponseWriter, resp *http.Response) bool {
	if p.cfg.SSEHeartbeatInterval <= 0 || resp == nil {
		return false
	}
	if _, ok := w.(http.Flusher); !ok {
		return false
	}
	return isSSEResponse(resp.Header)
}

func (p *proxyServer) copySSEWithHeartbeat(ctx context.Context, w http.ResponseWriter, body io.Reader) error {
	flusher := w.(http.Flusher)
	flusher.Flush()

	type readResult struct {
		data []byte
		err  error
	}

	chunks := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				select {
				case chunks <- readResult{data: data, err: err}:
				case <-ctx.Done():
				}
			}
			if err != nil {
				if n == 0 {
					select {
					case chunks <- readResult{err: err}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()

	timer := time.NewTimer(p.cfg.SSEHeartbeatInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-chunks:
			if len(result.data) > 0 {
				if _, err := w.Write(result.data); err != nil {
					return err
				}
				flusher.Flush()
			}
			if errors.Is(result.err, io.EOF) {
				return nil
			}
			if result.err != nil {
				return result.err
			}
			resetTimer(timer, p.cfg.SSEHeartbeatInterval)
		case <-timer.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return err
			}
			flusher.Flush()
			resetTimer(timer, p.cfg.SSEHeartbeatInterval)
		}
	}
}

type requestBuildStats struct {
	Compressed    bool
	OriginalBytes int
	SentBytes     int
	ReadElapsed   time.Duration
}

func (p *proxyServer) buildUpstreamRequest(r *http.Request) (*http.Request, requestBuildStats, error) {
	targetURL := *p.cfg.UpstreamBaseURL
	targetURL.Path = singleJoiningSlash(p.cfg.UpstreamBaseURL.Path, r.URL.Path)
	targetURL.RawQuery = r.URL.RawQuery

	bodyReader := r.Body
	stats := requestBuildStats{}

	if p.shouldCompress(r) {
		readStarted := time.Now()
		raw, err := readLimitedBody(r.Body, p.cfg.MaxBufferBytes)
		stats.ReadElapsed = time.Since(readStarted)
		if err != nil {
			return nil, stats, err
		}
		stats.OriginalBytes = len(raw)
		if len(raw) >= int(p.cfg.CompressMinBytes) {
			gzipped, err := gzipBytes(raw)
			if err != nil {
				return nil, stats, fmt.Errorf("gzip request body: %w", err)
			}
			if len(gzipped) < len(raw) {
				bodyReader = io.NopCloser(bytes.NewReader(gzipped))
				stats.Compressed = true
				stats.SentBytes = len(gzipped)
			} else {
				bodyReader = io.NopCloser(bytes.NewReader(raw))
				stats.SentBytes = len(raw)
			}
		} else {
			bodyReader = io.NopCloser(bytes.NewReader(raw))
			stats.SentBytes = len(raw)
		}
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), bodyReader)
	if err != nil {
		return nil, stats, fmt.Errorf("new upstream request: %w", err)
	}
	upstreamReq.Header = cloneHeaders(r.Header)
	upstreamReq.Host = ""
	upstreamReq.Header.Set(frontProxyHeader, frontProxyID)
	upstreamReq.Header.Set(frontProxyReadMs, strconv.FormatInt(stats.ReadElapsed.Milliseconds(), 10))
	upstreamReq.Header.Set(frontProxyOriginal, strconv.Itoa(stats.OriginalBytes))
	upstreamReq.Header.Set(frontProxySent, strconv.Itoa(stats.SentBytes))
	upstreamReq.Header.Set(frontProxyCompressed, strconv.FormatBool(stats.Compressed))
	if stats.Compressed {
		upstreamReq.Header.Set("Content-Encoding", "gzip")
		upstreamReq.ContentLength = int64(stats.SentBytes)
		upstreamReq.Header.Set("Content-Length", strconv.Itoa(stats.SentBytes))
	} else if stats.SentBytes > 0 {
		upstreamReq.ContentLength = int64(stats.SentBytes)
		upstreamReq.Header.Set("Content-Length", strconv.Itoa(stats.SentBytes))
	}
	removeHopByHopHeaders(upstreamReq.Header)
	return upstreamReq, stats, nil
}

func (p *proxyServer) shouldCompress(r *http.Request) bool {
	if r == nil || r.Body == nil {
		return false
	}
	if isWebsocketUpgrade(r.Header) {
		return false
	}
	if r.Method != http.MethodPost {
		return false
	}
	if strings.TrimSpace(r.Header.Get("Content-Encoding")) != "" {
		return false
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return false
	}
	for _, path := range p.cfg.CompressPaths {
		if r.URL.Path == path {
			return true
		}
	}
	return false
}

func readLimitedBody(body io.ReadCloser, maxBytes int64) ([]byte, error) {
	defer body.Close()
	if maxBytes <= 0 {
		return io.ReadAll(body)
	}
	limited := &io.LimitedReader{R: body, N: maxBytes + 1}
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if limited.N <= 0 {
		return nil, &http.MaxBytesError{Limit: maxBytes}
	}
	return raw, nil
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func cloneHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		cloned := make([]string, len(values))
		copy(cloned, values)
		dst[key] = cloned
	}
	return dst
}

func removeHopByHopHeaders(header http.Header) {
	for _, key := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		header.Del(key)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	removeHopByHopHeaders(src)
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isSSEResponse(header http.Header) bool {
	if header == nil {
		return false
	}
	return strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/event-stream")
}

func isWebsocketUpgrade(header http.Header) bool {
	if header == nil {
		return false
	}
	return strings.Contains(strings.ToLower(header.Get("Connection")), "upgrade") &&
		strings.EqualFold(strings.TrimSpace(header.Get("Upgrade")), "websocket")
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func getenvDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getenvInt64(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return value, nil
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("invalid duration %s=%q, fallback=%s", key, raw, fallback)
		return fallback
	}
	return value
}

func getenvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("invalid bool %s=%q, fallback=%v", key, raw, fallback)
		return fallback
	}
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}
