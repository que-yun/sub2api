package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

const (
	grokQuotaUpstreamTimeout = 20 * time.Second
	grokQuotaProbeInput      = "hi"
	grokQuotaDefaultModel    = grokDefaultResponsesModel
	grokBillingExtraKey      = "grok_billing_snapshot"
	grokBillingMaxAttempts   = 2
	grokBillingRetryDelay    = 100 * time.Millisecond
)

type GrokQuotaProbeResult struct {
	Source            string              `json:"source"`
	Model             string              `json:"model,omitempty"`
	Billing           *xai.BillingSummary `json:"billing,omitempty"`
	Snapshot          *xai.QuotaSnapshot  `json:"snapshot,omitempty"`
	LocalUsage24h     *WindowStats        `json:"local_usage_24h,omitempty"`
	LocalUsage7d      *WindowStats        `json:"local_usage_7d,omitempty"`
	LocalUsageMonthly *WindowStats        `json:"local_usage_monthly,omitempty"`
	StatusCode        int                 `json:"status_code,omitempty"`
	HeadersObserved   bool                `json:"headers_observed"`
	ResetSupported    bool                `json:"reset_supported"`
	FetchedAt         int64               `json:"fetched_at"`
	Persisted         bool                `json:"persisted"`
	ProbeError        string              `json:"probe_error,omitempty"`
}

type GrokQuotaResetResult struct {
	Supported bool   `json:"supported"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type GrokQuotaService struct {
	accountRepo   AccountRepository
	proxyRepo     ProxyRepository
	tokenProvider *GrokTokenProvider
	httpUpstream  HTTPUpstream
	usageLogRepo  UsageLogRepository
	cfg           *config.Config
	probeFlight   singleflight.Group
}

func NewGrokQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	tokenProvider *GrokTokenProvider,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	usageLogRepos ...UsageLogRepository,
) *GrokQuotaService {
	var usageLogRepo UsageLogRepository
	if len(usageLogRepos) > 0 {
		usageLogRepo = usageLogRepos[0]
	}
	return &GrokQuotaService{
		accountRepo:   accountRepo,
		proxyRepo:     proxyRepo,
		tokenProvider: tokenProvider,
		httpUpstream:  httpUpstream,
		usageLogRepo:  usageLogRepo,
		cfg:           cfg,
	}
}

// QueryQuota combines xAI billing data with an active quota-header probe for
// Free accounts, whose billing response does not include usage_percent.
//
// forceActiveProbe=true always runs the chat/readiness probe. Recovery tooling
// must use that path; paid billing snapshots alone can look healthy while chat
// entitlement is still 403.
func (s *GrokQuotaService) QueryQuota(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	return s.queryQuota(ctx, accountID, false)
}

// QueryQuotaActiveProbe forces the chat readiness probe used by recovery scans.
func (s *GrokQuotaService) QueryQuotaActiveProbe(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	return s.queryQuota(ctx, accountID, true)
}

func (s *GrokQuotaService) queryQuota(ctx context.Context, accountID int64, forceActiveProbe bool) (*GrokQuotaProbeResult, error) {
	billingResult, billingErr := s.ProbeBilling(ctx, accountID)
	if !forceActiveProbe && billingErr == nil && billingResult != nil && grokBillingHasAuthoritativeQuota(billingResult.Billing) {
		return billingResult, nil
	}

	probeResult, probeErr := s.ProbeUsage(ctx, accountID)
	if probeErr != nil {
		// Hard readiness failures (401/403/token) must not be masked by billing.
		// Soft failures may still return billing with probe_error for diagnostics
		// when the caller is not doing an explicit readiness/recovery probe.
		if forceActiveProbe || isHardGrokActiveProbeFailure(probeErr) || billingResult == nil || billingResult.Billing == nil {
			return nil, probeErr
		}
		billingResult.ProbeError = probeErr.Error()
		if code := infraerrors.Code(probeErr); code > 0 {
			// Keep admin clients from treating billing-only 200 as chat-usable.
			billingResult.StatusCode = code
		}
		return billingResult, nil
	}
	if probeResult == nil {
		if billingErr != nil {
			return nil, billingErr
		}
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_PROBE_EMPTY", "Grok quota probe returned no result")
	}
	if billingResult != nil {
		probeResult.Source = "hybrid_probe"
		probeResult.Billing = billingResult.Billing
		probeResult.LocalUsage24h = billingResult.LocalUsage24h
		probeResult.LocalUsage7d = billingResult.LocalUsage7d
		probeResult.LocalUsageMonthly = billingResult.LocalUsageMonthly
		probeResult.Persisted = probeResult.Persisted || billingResult.Persisted
	}
	return probeResult, nil
}

// isHardGrokActiveProbeFailure reports readiness failures that mean the account
// is not currently usable for chat, even if billing endpoints still answer.
func isHardGrokActiveProbeFailure(err error) bool {
	if err == nil {
		return false
	}
	reason := infraerrors.Reason(err)
	code := infraerrors.Code(err)
	switch reason {
	case "GROK_QUOTA_TOKEN_UNAVAILABLE":
		return true
	case "GROK_QUOTA_PROBE_UPSTREAM_ERROR":
		return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound
	default:
		return false
	}
}

func grokBillingHasAuthoritativeQuota(billing *xai.BillingSummary) bool {
	if billing == nil {
		return false
	}
	return billing.UsagePercent != nil ||
		billing.UsedPercent != nil ||
		(billing.MonthlyLimitCents != nil && *billing.MonthlyLimitCents > 0) ||
		strings.TrimSpace(billing.Plan) != ""
}

func (s *GrokQuotaService) ProbeUsage(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	return s.runProbeFlight(ctx, "active:"+strconv.FormatInt(accountID, 10), func(sharedCtx context.Context) (*GrokQuotaProbeResult, error) {
		return s.probeUsage(sharedCtx, accountID)
	})
}

func (s *GrokQuotaService) probeUsage(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	account, token, proxyURL, err := s.prepareProbe(ctx, accountID)
	if err != nil {
		return nil, err
	}

	// 探测必须测"账号真正服务的模型"：与真实转发一致，按账号 model_mapping 解析上游模型
	// (resolveOpenAIForwardModelForContext 同 forwardGrokResponses)。原来硬用固定 grok-4.5，
	// 若某账号把 grok-4.5 映射成别的模型，探测会测错模型 → 假 403 → 误 hold，把 chat 其实可用的号冻死。
	probeModel := resolveOpenAIForwardModelForContext(ctx, account, grokQuotaProbeModel(), "")
	if strings.TrimSpace(probeModel) == "" {
		probeModel = grokQuotaProbeModel()
	}
	body, err := buildGrokQuotaProbeBody(probeModel)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "GROK_QUOTA_PROBE_BODY_ERROR", "failed to build probe body: %v", err)
	}
	targetURL, err := buildGrokResponsesURL(account, s.cfg)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "GROK_QUOTA_BASE_URL_INVALID", "invalid Grok base_url: %v", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, grokQuotaUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "GROK_QUOTA_PROBE_REQUEST_BUILD_FAILED", "failed to build upstream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	applyGrokOAuthUpstreamHeaders(account, req.Header)
	// 探测请求与真实转发保持同一套账号级请求头覆写，避免探测通过但转发失败。
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "GROK_QUOTA_PROBE_REQUEST_FAILED", "upstream probe failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	snapshot := xai.ObserveQuotaHeaders(resp.Header, resp.StatusCode, "active_probe")
	resetAt, limited := grokRateLimitResetAtForAccount(account, snapshot, time.Now())
	if limited {
		normalizeGrokExhaustedWindowResets(snapshot, resetAt, time.Now())
	}
	persistErr := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		grokQuotaSnapshotExtraKey: snapshot,
	})
	if limited && resp.StatusCode < http.StatusBadRequest {
		// Error statuses (429/403) are marked by ApplyGrokProbeOrTestStatus below;
		// only persist here for successful-but-exhausted windows to avoid double
		// rate-limit writes.
		persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
	} else if isSuccessfulGrokRateLimitRecovery(account, snapshot) {
		clearGrokRateLimitAfterRecovery(ctx, s.accountRepo, account)
	}

	result := &GrokQuotaProbeResult{
		Source:          "active_probe",
		Model:           probeModel,
		Snapshot:        snapshot,
		StatusCode:      resp.StatusCode,
		HeadersObserved: snapshot.HeadersObserved,
		ResetSupported:  false,
		FetchedAt:       time.Now().Unix(),
		Persisted:       persistErr == nil,
	}
	// Read a small body prefix for readiness classification (403 vs free-usage etc).
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// Operator/admin probes must apply the same readiness side effects as production:
	// HTTP 200 clears sticky hold/error; true entitlement 403 marks error.
	ApplyGrokProbeOrTestStatus(ctx, s.accountRepo, nil, account, resp.StatusCode, resp.Header, bodyBytes, "active_probe")
	if resp.StatusCode == http.StatusTooManyRequests {
		return result, nil
	}
	if resp.StatusCode >= 400 {
		const reason = "GROK_QUOTA_PROBE_UPSTREAM_ERROR"
		// Never echo the raw upstream body: free-text error/detail fields can carry
		// credentials. Surface only the structured classification code (e.g.
		// permission-denied / personal-team-blocked:spending-limit), which is safe
		// and lets callers distinguish a hard entitlement 403 from billing. bodyBytes
		// is still consumed above by ApplyGrokProbeOrTestStatus for readiness.
		upstreamCode := strings.TrimSpace(gjson.GetBytes(bodyBytes, "code").String())
		if upstreamCode == "" {
			upstreamCode = strings.TrimSpace(gjson.GetBytes(bodyBytes, "error.code").String())
		}
		slog.Warn(
			"grok_quota_probe_failed",
			"account_id", account.ID,
			"model", probeModel,
			"status", resp.StatusCode,
			"reason", reason,
			"upstream_code", upstreamCode,
		)
		if upstreamCode != "" {
			return nil, infraerrors.Newf(
				mapUpstreamStatus(resp.StatusCode),
				reason,
				"upstream returned %d for probe model %q: %s",
				resp.StatusCode,
				probeModel,
				upstreamCode,
			)
		}
		return nil, infraerrors.Newf(
			mapUpstreamStatus(resp.StatusCode),
			reason,
			"upstream returned %d for probe model %q",
			resp.StatusCode,
			probeModel,
		)
	}
	return result, nil
}

// ProbeBilling only calls the xAI billing endpoints. Account usage refreshes
// use this method so opening the account list never consumes model quota.
func (s *GrokQuotaService) ProbeBilling(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	return s.runProbeFlight(ctx, "billing:"+strconv.FormatInt(accountID, 10), func(sharedCtx context.Context) (*GrokQuotaProbeResult, error) {
		return s.probeBilling(sharedCtx, accountID)
	})
}

// ProbeMediaEligibility refreshes billing state and evaluates the persisted
// account snapshot used by media scheduling. Probe failures remain fail-closed;
// deterministic persisted states such as forbidden or Free are returned as
// normal ineligibility decisions rather than transport errors.
func (s *GrokQuotaService) ProbeMediaEligibility(ctx context.Context, accountID int64) (bool, string, error) {
	_, probeErr := s.ProbeBilling(ctx, accountID)
	account, err := s.loadGrokOAuthAccount(ctx, accountID)
	if err != nil {
		return false, "billing_probe_failed", err
	}
	eligible, reason := account.GrokMediaGenerationEligibility()
	if reason == "billing_unobserved" && probeErr != nil {
		return false, reason, probeErr
	}
	return eligible, reason, nil
}

func (s *GrokQuotaService) probeBilling(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	account, token, proxyURL, err := s.prepareProbe(ctx, accountID)
	if err != nil {
		return nil, err
	}

	probeCtx, cancel := context.WithTimeout(ctx, grokQuotaUpstreamTimeout)
	defer cancel()
	type billingResult struct {
		summary *xai.BillingSummary
		status  int
		err     error
	}
	var weekly, monthly billingResult
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		weekly.summary, weekly.status, weekly.err = s.fetchBilling(probeCtx, account, token, proxyURL, true)
	}()
	go func() {
		defer wg.Done()
		monthly.summary, monthly.status, monthly.err = s.fetchBilling(probeCtx, account, token, proxyURL, false)
	}()
	wg.Wait()

	weeklyOK := weekly.summary != nil
	monthlyOK := monthly.summary != nil
	previous, _ := grokBillingSnapshotFromExtra(account.Extra)
	if !weeklyOK && !monthlyOK {
		probeErr := mergeGrokBillingProbeErrors(weekly.status, monthly.status, weekly.err, monthly.err)
		billing := xai.MergeBillingProbeResult(previous, nil, nil, false, false)
		if billing == nil {
			billing = &xai.BillingSummary{Partial: true, FailedWindows: []string{"weekly", "monthly"}}
		}
		billing.WeeklyStatusCode = weekly.status
		billing.MonthlyStatusCode = monthly.status
		billing = xai.StampBillingSummary(billing, preferBillingObservationStatus(weekly.status, monthly.status), "billing_probe")
		if persistErr := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{grokBillingExtraKey: billing}); persistErr != nil {
			slog.Warn("grok_billing_failure_persist_failed", "account_id", account.ID, "error", persistErr)
		}
		return nil, probeErr
	}
	statusCode := preferSuccessfulBillingStatus(weekly.status, monthly.status, weeklyOK, monthlyOK)
	billing := xai.MergeBillingProbeResult(previous, weekly.summary, monthly.summary, weeklyOK, monthlyOK)
	billing.WeeklyStatusCode = weekly.status
	billing.MonthlyStatusCode = monthly.status
	billing = xai.StampBillingSummary(billing, statusCode, "billing_probe")
	persistErr := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		grokBillingExtraKey: billing,
	})
	if persistErr != nil {
		slog.Warn("grok_billing_persist_failed", "account_id", account.ID, "error", persistErr)
	}
	now := time.Now().UTC()
	localUsage24h, localUsage7d, localUsageMonthly := grokLocalUsageForQuota(ctx, s.usageLogRepo, account.ID, billing, now)
	return &GrokQuotaProbeResult{
		Source:            "billing_probe",
		Billing:           billing,
		LocalUsage24h:     localUsage24h,
		LocalUsage7d:      localUsage7d,
		LocalUsageMonthly: localUsageMonthly,
		StatusCode:        statusCode,
		FetchedAt:         now.Unix(),
		Persisted:         persistErr == nil,
	}, nil
}

func preferBillingObservationStatus(weeklyStatus, monthlyStatus int) int {
	if weeklyStatus == http.StatusForbidden || monthlyStatus == http.StatusForbidden {
		return http.StatusForbidden
	}
	if weeklyStatus != 0 {
		return weeklyStatus
	}
	return monthlyStatus
}

func (s *GrokQuotaService) runProbeFlight(
	ctx context.Context,
	key string,
	probe func(context.Context) (*GrokQuotaProbeResult, error),
) (*GrokQuotaProbeResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_NOT_CONFIGURED", "grok quota service is not configured")
	}
	resultCh := s.probeFlight.DoChan(key, func() (any, error) {
		sharedCtx, cancel := context.WithTimeout(context.Background(), grokQuotaUpstreamTimeout+5*time.Second)
		defer cancel()
		return probe(sharedCtx)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return nil, flightResult.Err
		}
		result, ok := flightResult.Val.(*GrokQuotaProbeResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_PROBE_RESULT_INVALID", "invalid Grok quota probe result")
		}
		cloned := *result
		return &cloned, nil
	}
}

func (s *GrokQuotaService) fetchBilling(
	ctx context.Context,
	account *Account,
	token string,
	proxyURL string,
	weekly bool,
) (*xai.BillingSummary, int, error) {
	billingURL, err := buildGrokBillingURL(account, s.cfg, weekly)
	if err != nil {
		return nil, 0, infraerrors.Newf(http.StatusBadRequest, "GROK_QUOTA_BASE_URL_INVALID", "invalid Grok base_url: %v", err)
	}
	for attempt := 0; attempt < grokBillingMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, billingURL, nil)
		if err != nil {
			return nil, 0, infraerrors.Newf(http.StatusInternalServerError, "GROK_QUOTA_PROBE_REQUEST_BUILD_FAILED", "failed to build billing request: %v", err)
		}
		xai.ApplyCLIBillingHeaders(req, token)
		// billing 探测与真实转发保持同一套账号级请求头覆写。
		account.ApplyHeaderOverrides(req.Header)
		resp, requestErr := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 2))

		statusCode := 0
		var bodyBytes []byte
		if requestErr == nil {
			statusCode = resp.StatusCode
			bodyBytes, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
		}

		shouldRetry := requestErr != nil || isRetryableGrokBillingStatus(statusCode)
		if shouldRetry && attempt+1 < grokBillingMaxAttempts {
			timer := time.NewTimer(grokBillingRetryDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, statusCode, infraerrors.Newf(http.StatusBadGateway, "GROK_QUOTA_PROBE_REQUEST_FAILED", "billing request failed: %v", ctx.Err())
			}
			continue
		}

		if requestErr != nil {
			return nil, 0, infraerrors.Newf(http.StatusBadGateway, "GROK_QUOTA_PROBE_REQUEST_FAILED", "billing request failed: %v", requestErr)
		}
		if statusCode == http.StatusTooManyRequests {
			return nil, statusCode, nil
		}
		if statusCode >= 400 {
			bodyText := truncate(strings.TrimSpace(string(bodyBytes)), 240)
			slog.Warn("grok_quota_billing_failed", "account_id", account.ID, "weekly", weekly, "status", statusCode, "body", bodyText)
			return nil, statusCode, infraerrors.Newf(mapUpstreamStatus(statusCode), "GROK_QUOTA_PROBE_UPSTREAM_ERROR", "billing returned %d: %s", statusCode, bodyText)
		}
		payload, err := xai.ParseBillingPayload(bodyBytes)
		if err != nil {
			return nil, statusCode, infraerrors.Newf(http.StatusBadGateway, "GROK_QUOTA_BILLING_PARSE_ERROR", "failed to parse billing body: %v", err)
		}
		return xai.BuildBillingSummary(payload.Config), statusCode, nil
	}
	return nil, 0, infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_PROBE_REQUEST_FAILED", "billing request failed")
}

func isRetryableGrokBillingStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func mergeGrokBillingProbeErrors(weeklyStatus, monthlyStatus int, weeklyErr, monthlyErr error) error {
	weeklyKey := grokBillingProbeErrorKey(weeklyStatus, weeklyErr)
	monthlyKey := grokBillingProbeErrorKey(monthlyStatus, monthlyErr)
	if weeklyKey == monthlyKey {
		switch {
		case weeklyErr != nil:
			return weeklyErr
		case monthlyErr != nil:
			return monthlyErr
		case weeklyStatus == http.StatusTooManyRequests:
			return infraerrors.New(http.StatusTooManyRequests, "GROK_QUOTA_PROBE_UPSTREAM_ERROR", "billing rate limited")
		case weeklyStatus != 0 && weeklyStatus != http.StatusOK:
			return infraerrors.New(mapUpstreamStatus(weeklyStatus), "GROK_QUOTA_PROBE_UPSTREAM_ERROR", "xAI billing endpoints returned the same upstream error")
		default:
			return infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_BILLING_EMPTY", "xAI billing endpoints returned no quota data")
		}
	}
	slog.Warn("grok_quota_probe_parts_failed", "weekly_status", weeklyStatus, "weekly_error", weeklyErr, "monthly_status", monthlyStatus, "monthly_error", monthlyErr)
	return infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_PROBE_PARTS_FAILED", "weekly and monthly billing probes failed differently").WithMetadata(map[string]string{
		"weekly_status": strconv.Itoa(weeklyStatus), "monthly_status": strconv.Itoa(monthlyStatus),
	})
}

func grokBillingProbeErrorKey(status int, err error) string {
	if err != nil {
		return strconv.Itoa(status) + ":" + strconv.Itoa(infraerrors.Code(err)) + ":" + infraerrors.Reason(err)
	}
	return strconv.Itoa(status) + ":empty"
}

func preferSuccessfulBillingStatus(weeklyStatus, monthlyStatus int, weeklyOK, monthlyOK bool) int {
	if weeklyOK && weeklyStatus >= 200 && weeklyStatus < 300 {
		return weeklyStatus
	}
	if monthlyOK && monthlyStatus >= 200 && monthlyStatus < 300 {
		return monthlyStatus
	}
	if weeklyStatus != 0 {
		return weeklyStatus
	}
	return monthlyStatus
}

func (s *GrokQuotaService) ResetQuota(ctx context.Context, accountID int64) (*GrokQuotaResetResult, error) {
	if _, err := s.loadGrokOAuthAccount(ctx, accountID); err != nil {
		return nil, err
	}
	return nil, infraerrors.New(http.StatusNotImplemented, "GROK_QUOTA_RESET_UNSUPPORTED", "xAI does not expose a Grok subscription quota reset endpoint for OAuth accounts")
}

func (s *GrokQuotaService) prepareProbe(ctx context.Context, accountID int64) (*Account, string, string, error) {
	if s == nil || s.tokenProvider == nil || s.httpUpstream == nil {
		return nil, "", "", infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_NOT_CONFIGURED", "grok quota service is not configured")
	}
	account, err := s.loadGrokOAuthAccount(ctx, accountID)
	if err != nil {
		return nil, "", "", err
	}
	proxyURL := s.resolveProxyURL(ctx, account)

	// Admin/recovery probes intentionally target sticky error accounts.
	// Use operator probe token path so inactive Grok OAuth can still refresh.
	token, err := s.tokenProvider.GetAccessTokenForOperatorProbe(ctx, account)
	if err != nil {
		return nil, "", "", infraerrors.Newf(http.StatusBadGateway, "GROK_QUOTA_TOKEN_UNAVAILABLE", "failed to acquire access token: %v", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, "", "", infraerrors.New(http.StatusBadGateway, "GROK_QUOTA_TOKEN_UNAVAILABLE", "access token is empty")
	}

	return account, token, proxyURL, nil
}

func (s *GrokQuotaService) resolveProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil {
		return ""
	}
	switch {
	case account.Proxy != nil:
		return account.Proxy.URL()
	case s != nil && s.proxyRepo != nil:
		if proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			account.Proxy = proxy
			return proxy.URL()
		}
	}
	return ""
}

func (s *GrokQuotaService) loadGrokOAuthAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "GROK_QUOTA_NOT_CONFIGURED", "grok quota service is not configured")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "GROK_QUOTA_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if account == nil {
		return nil, infraerrors.New(http.StatusNotFound, "GROK_QUOTA_ACCOUNT_NOT_FOUND", "account not found")
	}
	if account.Platform != PlatformGrok {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_QUOTA_INVALID_PLATFORM", "account is not a Grok account")
	}
	if account.Type != AccountTypeOAuth {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_QUOTA_INVALID_TYPE", "account is not an OAuth account")
	}
	return account, nil
}

func grokQuotaProbeModel() string {
	return grokQuotaDefaultModel
}

func buildGrokQuotaProbeBody(model string) ([]byte, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = grokQuotaDefaultModel
	}
	return json.Marshal(map[string]any{
		"model":  model,
		"input":  grokQuotaProbeInput,
		"stream": true,
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
