package service

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	// Extra keys used by the background 403 recovery worker.
	grokErrorRecoveryLastProbeAtExtraKey     = "grok_error_recovery_last_probe_at"
	grokErrorRecoveryLastClassExtraKey       = "grok_error_recovery_last_class"
	grokErrorRecoveryLastStatusExtraKey      = "grok_error_recovery_last_status"
	grokErrorRecoveryLastMessageExtraKey     = "grok_error_recovery_last_message"
	grokErrorRecoveryLastRecoveredAtExtraKey = "grok_error_recovery_last_recovered_at"
	// A credential sync adds this marker on the VPS. It gives a fresh credential
	// one prompt, local-to-VPS readiness probe before it joins the normal retry
	// cadence for the much larger sticky-error pool.
	grokVPSProbeRequestedAtExtraKey = "grok_vps_probe_requested_at"
	grokVPSProbeCompletedAtExtraKey = "grok_vps_probe_completed_at"

	// Recovery class labels persisted into Extra for ops visibility.
	grokErrorRecoveryClassRecovered       = "recovered"
	grokErrorRecoveryClassForbidden       = "forbidden_403"
	grokErrorRecoveryClassPaymentRequired = "payment_402"
	grokErrorRecoveryClassRateLimited     = "rate_429"
	grokErrorRecoveryClassTokenError      = "token_error"
	grokErrorRecoveryClassTransportError  = "transport_error"
	grokErrorRecoveryClassOtherError      = "other_error"
	grokErrorRecoveryClassSkipped         = "skipped"

	// Default cycle timeout keeps a single pass from hanging forever.
	grokErrorRecoveryDefaultCycleTimeout = 30 * time.Minute
	// A single account must not hold a recovery worker forever. ProbeUsage has
	// its own upstream deadline, while this deadline also bounds persistence and
	// any provider-side work around the probe.
	grokErrorRecoveryProbeTimeout = 30 * time.Second
)

// GrokErrorRecoveryService periodically re-probes Grok OAuth accounts that are
// stuck in entitlement/403 error and restores them when the probe succeeds.
//
// It is intentionally separate from TokenRefreshService:
//   - token keep-alive covers active accounts
//   - this worker covers sticky entitlement errors that occasionally recover
//   - invalid_grant / missing RT accounts are excluded and stay in reauth queue
type GrokErrorRecoveryService struct {
	accountRepo AccountRepository
	quotaSvc    *GrokQuotaService
	cfg         *config.GrokErrorRecoveryConfig

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// nowFunc is overridable in tests.
	nowFunc func() time.Time
	// sleepFunc is overridable in tests (unused currently; reserved for pacing).
	runOnceHook func(ctx context.Context) GrokErrorRecoveryCycleResult
}

// GrokErrorRecoveryCycleResult summarizes one recovery pass for logs/tests.
type GrokErrorRecoveryCycleResult struct {
	Candidates int `json:"candidates"`
	Probed     int `json:"probed"`
	Recovered  int `json:"recovered"`
	Forbidden  int `json:"forbidden"`
	RateLimit  int `json:"rate_limited"`
	TokenError int `json:"token_error"`
	Transport  int `json:"transport_error"`
	Other      int `json:"other_error"`
	Skipped    int `json:"skipped"`
}

// NewGrokErrorRecoveryService creates the worker. Call Start to begin looping.
func NewGrokErrorRecoveryService(
	accountRepo AccountRepository,
	quotaSvc *GrokQuotaService,
	cfg *config.Config,
) *GrokErrorRecoveryService {
	var recoveryCfg *config.GrokErrorRecoveryConfig
	if cfg != nil {
		recoveryCfg = &cfg.GrokErrorRecovery
	}
	return &GrokErrorRecoveryService{
		accountRepo: accountRepo,
		quotaSvc:    quotaSvc,
		cfg:         recoveryCfg,
		stopCh:      make(chan struct{}),
		nowFunc:     time.Now,
	}
}

// Start begins the background recovery loop when enabled.
func (s *GrokErrorRecoveryService) Start() {
	if s == nil {
		return
	}
	if s.cfg == nil || !s.cfg.Enabled {
		slog.Info("grok_error_recovery.service_disabled")
		return
	}
	if s.accountRepo == nil || s.quotaSvc == nil {
		slog.Warn("grok_error_recovery.service_not_started", "reason", "missing dependencies")
		return
	}

	s.wg.Add(1)
	go s.loop()

	slog.Info("grok_error_recovery.service_started",
		"check_interval_hours", s.cfg.CheckIntervalHours,
		"min_reprobe_interval_hours", s.cfg.MinReprobeIntervalHours,
		"concurrency", s.cfg.Concurrency,
		"max_accounts_per_cycle", s.cfg.MaxAccountsPerCycle,
		"run_on_start", s.cfg.RunOnStart,
	)
}

// Stop stops the background loop. Safe to call multiple times.
func (s *GrokErrorRecoveryService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
	slog.Info("grok_error_recovery.service_stopped")
}

func (s *GrokErrorRecoveryService) loop() {
	defer s.wg.Done()

	interval := s.checkInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if s.cfg != nil && s.cfg.RunOnStart {
		s.processCycle()
	}

	for {
		select {
		case <-ticker.C:
			s.processCycle()
		case <-s.stopCh:
			return
		}
	}
}

func (s *GrokErrorRecoveryService) processCycle() {
	ctx, cancel := context.WithTimeout(context.Background(), grokErrorRecoveryDefaultCycleTimeout)
	defer cancel()

	var result GrokErrorRecoveryCycleResult
	if s.runOnceHook != nil {
		result = s.runOnceHook(ctx)
	} else {
		result = s.RunOnce(ctx)
	}

	slog.Info("grok_error_recovery.cycle_finished",
		"candidates", result.Candidates,
		"probed", result.Probed,
		"recovered", result.Recovered,
		"forbidden", result.Forbidden,
		"rate_limited", result.RateLimit,
		"token_error", result.TokenError,
		"transport_error", result.Transport,
		"other_error", result.Other,
		"skipped", result.Skipped,
	)
}

// RunOnce executes a single recovery pass. Exported for tests and manual ops hooks.
func (s *GrokErrorRecoveryService) RunOnce(ctx context.Context) GrokErrorRecoveryCycleResult {
	result := GrokErrorRecoveryCycleResult{}
	if s == nil || s.accountRepo == nil || s.quotaSvc == nil {
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}

	accounts, err := s.accountRepo.ListAllWithFilters(ctx, PlatformGrok, AccountTypeOAuth, StatusError, "", 0, "", "")
	if err != nil {
		slog.Error("grok_error_recovery.list_accounts_failed", "error", err)
		return result
	}

	now := s.now()
	minAge := s.minReprobeInterval()
	candidates := make([]Account, 0, len(accounts))
	for i := range accounts {
		acc := accounts[i]
		if !isGrokErrorRecoveryCandidate(&acc, now, minAge) {
			result.Skipped++
			continue
		}
		candidates = append(candidates, acc)
	}

	sortGrokErrorRecoveryCandidates(candidates)

	maxN := s.maxAccountsPerCycle()
	if maxN > 0 && len(candidates) > maxN {
		result.Skipped += len(candidates) - maxN
		candidates = candidates[:maxN]
	}
	result.Candidates = len(candidates)
	if len(candidates) == 0 {
		return result
	}

	concurrency := s.concurrency()
	sem := make(chan struct{}, concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := range candidates {
		select {
		case <-ctx.Done():
			return result
		case <-s.stopCh:
			return result
		default:
		}

		acc := candidates[i]
		sem <- struct{}{}
		wg.Add(1)
		go func(account Account) {
			defer wg.Done()
			defer func() { <-sem }()

			probeCtx, cancel := context.WithTimeout(ctx, grokErrorRecoveryProbeTimeout)
			defer cancel()
			class, statusCode, msg := s.probeOne(probeCtx, &account)

			mu.Lock()
			defer mu.Unlock()
			result.Probed++
			switch class {
			case grokErrorRecoveryClassRecovered:
				result.Recovered++
			case grokErrorRecoveryClassForbidden:
				result.Forbidden++
			case grokErrorRecoveryClassPaymentRequired:
				result.Other++
			case grokErrorRecoveryClassRateLimited:
				result.RateLimit++
			case grokErrorRecoveryClassTokenError:
				result.TokenError++
			case grokErrorRecoveryClassTransportError:
				result.Transport++
			default:
				result.Other++
			}
			if msg != "" {
				slog.Debug("grok_error_recovery.probe_result",
					"account_id", account.ID,
					"class", class,
					"status", statusCode,
					"message", truncate(msg, 180),
				)
			}
		}(acc)
	}
	wg.Wait()
	return result
}

func (s *GrokErrorRecoveryService) probeOne(ctx context.Context, account *Account) (class string, statusCode int, message string) {
	if account == nil {
		return grokErrorRecoveryClassOtherError, 0, "nil account"
	}

	// ProbeUsage already applies ApplyGrokProbeOrTestStatus side effects.
	// On success it clears error/hold via ClearGrokHoldAfterSuccess.
	result, err := s.quotaSvc.ProbeUsage(ctx, account.ID)
	now := s.now()
	class = grokErrorRecoveryClassOtherError
	statusCode = 0
	message = ""

	switch {
	case err == nil && result != nil && result.StatusCode == 200:
		class = grokErrorRecoveryClassRecovered
		statusCode = result.StatusCode
	case err == nil && result != nil && result.StatusCode == 429:
		// Free-usage/rate-limit is still a usable channel; side effects already applied.
		class = grokErrorRecoveryClassRateLimited
		statusCode = result.StatusCode
	case err != nil:
		message = err.Error()
		class, statusCode = classifyGrokErrorRecoveryFailure(message)
		if result != nil && result.StatusCode > 0 {
			statusCode = result.StatusCode
		}
	case result != nil:
		statusCode = result.StatusCode
		if statusCode == 403 {
			class = grokErrorRecoveryClassForbidden
		} else if statusCode >= 400 {
			class = grokErrorRecoveryClassOtherError
		}
	}

	extra := map[string]any{
		grokErrorRecoveryLastProbeAtExtraKey: now.UTC().Format(time.RFC3339),
		grokErrorRecoveryLastClassExtraKey:   class,
		grokErrorRecoveryLastStatusExtraKey:  statusCode,
		grokErrorRecoveryLastMessageExtraKey: truncate(message, 240),
	}
	if class == grokErrorRecoveryClassRecovered {
		extra[grokErrorRecoveryLastRecoveredAtExtraKey] = now.UTC().Format(time.RFC3339)
	}
	// A definitive upstream result acknowledges the credential-sync probe. Leave
	// transport failures unacknowledged so they can still receive their first
	// proper readiness probe, while 200/401/402/403/429 rejoin normal recovery
	// scheduling after this attempt.
	if isGrokVPSProbeTerminalStatus(statusCode) {
		extra[grokVPSProbeCompletedAtExtraKey] = now.UTC().Format(time.RFC3339)
	}
	if s.accountRepo != nil {
		_ = s.accountRepo.UpdateExtra(ctx, account.ID, extra)
	}
	return class, statusCode, message
}

func sortGrokErrorRecoveryCandidates(candidates []Account) {
	sort.SliceStable(candidates, func(i, j int) bool {
		iPending := grokVPSProbePendingAt(&candidates[i])
		jPending := grokVPSProbePendingAt(&candidates[j])
		if !iPending.IsZero() || !jPending.IsZero() {
			switch {
			case iPending.IsZero():
				return false
			case jPending.IsZero():
				return true
			case !iPending.Equal(jPending):
				return iPending.After(jPending)
			}
		}
		// For the normal recovery pool, prefer accounts that have never been
		// probed (or whose last probe is oldest).
		return grokErrorRecoveryLastProbeTime(&candidates[i]).Before(grokErrorRecoveryLastProbeTime(&candidates[j]))
	})
}

func grokVPSProbePendingAt(account *Account) time.Time {
	if account == nil || account.Extra == nil {
		return time.Time{}
	}
	requested := parseExtraTime(account.Extra[grokVPSProbeRequestedAtExtraKey])
	if requested.IsZero() {
		return time.Time{}
	}
	completed := parseExtraTime(account.Extra[grokVPSProbeCompletedAtExtraKey])
	if !completed.IsZero() && !completed.Before(requested) {
		return time.Time{}
	}
	return requested
}

func isGrokVPSProbeTerminalStatus(statusCode int) bool {
	switch statusCode {
	case 200, 401, 402, 403, 429:
		return true
	default:
		return false
	}
}

func (s *GrokErrorRecoveryService) now() time.Time {
	if s != nil && s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

func (s *GrokErrorRecoveryService) checkInterval() time.Duration {
	hours := 6.0
	if s != nil && s.cfg != nil && s.cfg.CheckIntervalHours > 0 {
		hours = s.cfg.CheckIntervalHours
	}
	d := time.Duration(hours * float64(time.Hour))
	if d < time.Minute {
		d = time.Minute
	}
	return d
}

func (s *GrokErrorRecoveryService) minReprobeInterval() time.Duration {
	hours := 6.0
	if s != nil && s.cfg != nil && s.cfg.MinReprobeIntervalHours > 0 {
		hours = s.cfg.MinReprobeIntervalHours
	}
	return time.Duration(hours * float64(time.Hour))
}

func (s *GrokErrorRecoveryService) concurrency() int {
	n := 4
	if s != nil && s.cfg != nil && s.cfg.Concurrency > 0 {
		n = s.cfg.Concurrency
	}
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	return n
}

func (s *GrokErrorRecoveryService) maxAccountsPerCycle() int {
	if s != nil && s.cfg != nil && s.cfg.MaxAccountsPerCycle > 0 {
		return s.cfg.MaxAccountsPerCycle
	}
	return 120
}

// isGrokErrorRecoveryCandidate decides whether an error account should enter the
// entitlement recovery queue. Pure helper for unit tests.
func isGrokErrorRecoveryCandidate(account *Account, now time.Time, minReprobeAge time.Duration) bool {
	if account == nil {
		return false
	}
	if account.Platform != PlatformGrok || account.Type != AccountTypeOAuth {
		return false
	}
	if account.Status != StatusError {
		return false
	}
	if strings.TrimSpace(account.GetGrokRefreshToken()) == "" {
		return false
	}

	// Exclude permanent token/reauth failures — those need reauth, not probing.
	if isGrokTokenReauthErrorMessage(account.ErrorMessage) {
		return false
	}

	// Prefer true entitlement/403 sticky failures; still allow unknown error text
	// that is not a token failure (historical recovery observed on sticky 403).
	if !looksLikeGrokEntitlementError(account) && !GrokAccountRequiresSuccessBeforeSchedule(account) {
		// Keep conservative: only probe entitlement-like or sticky-hold accounts.
		return false
	}

	// A locally synced credential stays in the VPS readiness queue until this
	// node observes a definitive upstream response. Do not make a transient
	// transport failure wait for the normal 6-hour entitlement re-probe window:
	// the short recovery cadence retries it without refreshing the credential.
	if !grokVPSProbePendingAt(account).IsZero() {
		return true
	}

	last := grokErrorRecoveryLastProbeTime(account)
	if !last.IsZero() && minReprobeAge > 0 && now.Sub(last) < minReprobeAge {
		return false
	}
	return true
}

func looksLikeGrokEntitlementError(account *Account) bool {
	if account == nil {
		return false
	}
	if GrokAccountRequiresSuccessBeforeSchedule(account) {
		return true
	}
	msg := strings.ToLower(strings.TrimSpace(account.ErrorMessage))
	if msg == "" {
		return false
	}
	needles := []string{
		"entitlement",
		"subscription tier",
		"permission-denied",
		"access to the chat endpoint is denied",
		"chat endpoint is denied",
		"403",
		"forbidden",
	}
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

func isGrokTokenReauthErrorMessage(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return false
	}
	needles := []string{
		"invalid_grant",
		"invalid_refresh_token",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"no refresh token",
		"refresh token is required",
		"token revoked",
		"reauth",
		"re-auth",
		"re-authorize",
		"unauthorized_client",
	}
	for _, n := range needles {
		if strings.Contains(m, n) {
			return true
		}
	}
	return false
}

func grokErrorRecoveryLastProbeTime(account *Account) time.Time {
	if account == nil || account.Extra == nil {
		return time.Time{}
	}
	// Prefer worker-owned marker; fall back to quota snapshot timestamps if present.
	keys := []string{
		grokErrorRecoveryLastProbeAtExtraKey,
		"last_probe_at",
	}
	for _, key := range keys {
		if t := parseExtraTime(account.Extra[key]); !t.IsZero() {
			return t
		}
	}
	switch snap := account.Extra[grokQuotaSnapshotExtraKey].(type) {
	case map[string]any:
		for _, key := range []string{"last_probe_at", "updated_at", "fetched_at"} {
			if t := parseExtraTime(snap[key]); !t.IsZero() {
				return t
			}
		}
	}
	return time.Time{}
}

func classifyGrokErrorRecoveryFailure(message string) (class string, statusCode int) {
	m := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(m, "returned 403") ||
		strings.Contains(m, "permission-denied") ||
		strings.Contains(m, "access to the chat endpoint is denied") ||
		strings.Contains(m, "entitlement"):
		return grokErrorRecoveryClassForbidden, 403
	case strings.Contains(m, "returned 402") ||
		strings.Contains(m, "payment required") ||
		strings.Contains(m, "spending-limit") ||
		strings.Contains(m, "personal-team-blocked"):
		// The quota endpoint intentionally maps most upstream 4xx responses to
		// 502 for admin callers. Preserve the embedded upstream 402 so a VPS
		// credential readiness probe is acknowledged as a definitive result.
		return grokErrorRecoveryClassPaymentRequired, 402
	case strings.Contains(m, "returned 429") ||
		strings.Contains(m, "free-usage") ||
		strings.Contains(m, "rate limit"):
		return grokErrorRecoveryClassRateLimited, 429
	case strings.Contains(m, "token") ||
		strings.Contains(m, "oauth") ||
		strings.Contains(m, "invalid_grant") ||
		strings.Contains(m, "unauthorized") ||
		strings.Contains(m, "401"):
		return grokErrorRecoveryClassTokenError, 401
	case strings.Contains(m, "timeout") ||
		strings.Contains(m, "eof") ||
		strings.Contains(m, "connection") ||
		strings.Contains(m, "proxy") ||
		strings.Contains(m, "transport") ||
		strings.Contains(m, "502") ||
		strings.Contains(m, "503") ||
		strings.Contains(m, "504"):
		return grokErrorRecoveryClassTransportError, 0
	default:
		return grokErrorRecoveryClassOtherError, 0
	}
}
