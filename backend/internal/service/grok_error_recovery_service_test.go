//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestIsGrokErrorRecoveryCandidate_EntitlementError(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	acc := &Account{
		ID:           1,
		Platform:     PlatformGrok,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		ErrorMessage: grokHoldUntilSuccessReason,
		Credentials: map[string]any{
			"refresh_token": "rt-1",
		},
		Extra: map[string]any{
			grokHoldUntilSuccessExtraKey: true,
		},
	}
	require.True(t, isGrokErrorRecoveryCandidate(acc, now, 6*time.Hour))
}

func TestIsGrokErrorRecoveryCandidate_SkipsTokenReauth(t *testing.T) {
	now := time.Now()
	acc := &Account{
		ID:           2,
		Platform:     PlatformGrok,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		ErrorMessage: "invalid_grant: token revoked",
		Credentials: map[string]any{
			"refresh_token": "rt-2",
		},
	}
	require.False(t, isGrokErrorRecoveryCandidate(acc, now, 0))
}

func TestIsGrokErrorRecoveryCandidate_SkipsMissingRefreshToken(t *testing.T) {
	now := time.Now()
	acc := &Account{
		ID:           3,
		Platform:     PlatformGrok,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		ErrorMessage: grokHoldUntilSuccessReason,
		Credentials:  map[string]any{},
		Extra: map[string]any{
			grokHoldUntilSuccessExtraKey: true,
		},
	}
	require.False(t, isGrokErrorRecoveryCandidate(acc, now, 0))
}

func TestIsGrokErrorRecoveryCandidate_RespectsMinReprobeInterval(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	acc := &Account{
		ID:           4,
		Platform:     PlatformGrok,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		ErrorMessage: "permission-denied",
		Credentials: map[string]any{
			"refresh_token": "rt-4",
		},
		Extra: map[string]any{
			grokHoldUntilSuccessExtraKey:         true,
			grokErrorRecoveryLastProbeAtExtraKey: now.Add(-1 * time.Hour).Format(time.RFC3339),
		},
	}
	require.False(t, isGrokErrorRecoveryCandidate(acc, now, 6*time.Hour))
	require.True(t, isGrokErrorRecoveryCandidate(acc, now, time.Hour))
}

func TestClassifyGrokErrorRecoveryFailure(t *testing.T) {
	class, code := classifyGrokErrorRecoveryFailure(`upstream returned 403 for probe model "grok-4.5": permission-denied`)
	require.Equal(t, grokErrorRecoveryClassForbidden, class)
	require.Equal(t, 403, code)

	class, code = classifyGrokErrorRecoveryFailure("failed to acquire access token: invalid_grant")
	require.Equal(t, grokErrorRecoveryClassTokenError, class)
	require.Equal(t, 401, code)

	class, code = classifyGrokErrorRecoveryFailure("upstream probe failed: EOF")
	require.Equal(t, grokErrorRecoveryClassTransportError, class)
	require.Equal(t, 0, code)
}

type fakeGrokQuotaForRecovery struct {
	mu      sync.Mutex
	results map[int64]*GrokQuotaProbeResult
	errors  map[int64]error
	calls   []int64
}

func (f *fakeGrokQuotaForRecovery) ProbeUsage(_ context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, accountID)
	if err, ok := f.errors[accountID]; ok {
		return f.results[accountID], err
	}
	if res, ok := f.results[accountID]; ok {
		return res, nil
	}
	return nil, errors.New("unexpected account")
}

func newRecoveryProbeStub(repo AccountRepository, fake *fakeGrokQuotaForRecovery) *GrokErrorRecoveryService {
	// Build a real service with a dummy quota service object; override probeOne via
	// wrapping RunOnce is hard without interface. Instead test helper path by using
	// a custom quota service with hijacked ProbeUsage through a local type below.
	cfg := &config.Config{GrokErrorRecovery: config.GrokErrorRecoveryConfig{
		Enabled:                 true,
		CheckIntervalHours:      6,
		MinReprobeIntervalHours: 0,
		Concurrency:             2,
		MaxAccountsPerCycle:     10,
	}}
	// Use a real GrokQuotaService pointer but replace the probe via a custom wrapper type.
	// Easiest path: implement RunOnce against a local interface-like adapter by temporarily
	// swapping service methods using a test-only subtype defined below.
	_ = cfg
	svc := &GrokErrorRecoveryService{
		accountRepo: repo,
		// quotaSvc left nil intentionally; we override RunOnce body via specialized test helper.
		cfg:     &cfg.GrokErrorRecovery,
		stopCh:  make(chan struct{}),
		nowFunc: time.Now,
	}
	// Attach fake through a small adapter that reimplements probeOne externally.
	svc.runOnceHook = func(ctx context.Context) GrokErrorRecoveryCycleResult {
		return runGrokErrorRecoveryOnceWithProbe(ctx, svc, repo, func(ctx context.Context, id int64) (*GrokQuotaProbeResult, error) {
			return fake.ProbeUsage(ctx, id)
		})
	}
	return svc
}

func runGrokErrorRecoveryOnceWithProbe(
	ctx context.Context,
	s *GrokErrorRecoveryService,
	repo AccountRepository,
	probeFn func(context.Context, int64) (*GrokQuotaProbeResult, error),
) GrokErrorRecoveryCycleResult {
	// Clone of RunOnce with injectable probe for unit tests.
	result := GrokErrorRecoveryCycleResult{}
	accounts, err := repo.ListAllWithFilters(ctx, PlatformGrok, AccountTypeOAuth, StatusError, "", 0, "", "")
	if err != nil {
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
	maxN := s.maxAccountsPerCycle()
	if maxN > 0 && len(candidates) > maxN {
		result.Skipped += len(candidates) - maxN
		candidates = candidates[:maxN]
	}
	result.Candidates = len(candidates)

	for i := range candidates {
		acc := &candidates[i]
		res, err := probeFn(ctx, acc.ID)
		class := grokErrorRecoveryClassOtherError
		statusCode := 0
		message := ""
		switch {
		case err == nil && res != nil && res.StatusCode == 200:
			class = grokErrorRecoveryClassRecovered
			statusCode = 200
			// Mimic ProbeUsage success side effect.
			ClearGrokHoldAfterSuccess(ctx, repo, nil, acc)
		case err == nil && res != nil && res.StatusCode == 429:
			class = grokErrorRecoveryClassRateLimited
			statusCode = 429
		case err != nil:
			message = err.Error()
			class, statusCode = classifyGrokErrorRecoveryFailure(message)
			if res != nil && res.StatusCode > 0 {
				statusCode = res.StatusCode
			}
		}
		_ = repo.UpdateExtra(ctx, acc.ID, map[string]any{
			grokErrorRecoveryLastProbeAtExtraKey: now.UTC().Format(time.RFC3339),
			grokErrorRecoveryLastClassExtraKey:   class,
			grokErrorRecoveryLastStatusExtraKey:  statusCode,
			grokErrorRecoveryLastMessageExtraKey: truncate(message, 240),
		})
		result.Probed++
		switch class {
		case grokErrorRecoveryClassRecovered:
			result.Recovered++
		case grokErrorRecoveryClassForbidden:
			result.Forbidden++
		case grokErrorRecoveryClassRateLimited:
			result.RateLimit++
		case grokErrorRecoveryClassTokenError:
			result.TokenError++
		case grokErrorRecoveryClassTransportError:
			result.Transport++
		default:
			result.Other++
		}
	}
	return result
}

type recoveryListRepo struct {
	*grokQuotaAccountRepo
	accounts []Account
}

func (r *recoveryListRepo) ListAllWithFilters(context.Context, string, string, string, string, int64, string, string) ([]Account, error) {
	out := make([]Account, len(r.accounts))
	copy(out, r.accounts)
	return out, nil
}

func TestGrokErrorRecoveryRunOnce_RecoversAndKeepsForbidden(t *testing.T) {
	baseRepo := &grokQuotaAccountRepo{updates: map[int64]map[string]any{}}
	repo := &recoveryListRepo{
		grokQuotaAccountRepo: baseRepo,
		accounts: []Account{
			{
				ID:           11,
				Platform:     PlatformGrok,
				Type:         AccountTypeOAuth,
				Status:       StatusError,
				Schedulable:  false,
				ErrorMessage: grokHoldUntilSuccessReason,
				Credentials:  map[string]any{"refresh_token": "rt-ok"},
				Extra:        map[string]any{grokHoldUntilSuccessExtraKey: true},
			},
			{
				ID:           12,
				Platform:     PlatformGrok,
				Type:         AccountTypeOAuth,
				Status:       StatusError,
				Schedulable:  false,
				ErrorMessage: grokHoldUntilSuccessReason,
				Credentials:  map[string]any{"refresh_token": "rt-403"},
				Extra:        map[string]any{grokHoldUntilSuccessExtraKey: true},
			},
			{
				ID:           13,
				Platform:     PlatformGrok,
				Type:         AccountTypeOAuth,
				Status:       StatusError,
				Schedulable:  false,
				ErrorMessage: "invalid_grant",
				Credentials:  map[string]any{"refresh_token": "rt-bad"},
			},
		},
	}
	fake := &fakeGrokQuotaForRecovery{
		results: map[int64]*GrokQuotaProbeResult{
			11: {StatusCode: 200},
			12: {StatusCode: 403},
		},
		errors: map[int64]error{
			12: errors.New(`upstream returned 403 for probe model "grok-4.5": permission-denied`),
		},
	}
	svc := newRecoveryProbeStub(repo, fake)
	result := svc.runOnceHook(context.Background())

	require.Equal(t, 2, result.Candidates)
	require.Equal(t, 2, result.Probed)
	require.Equal(t, 1, result.Recovered)
	require.Equal(t, 1, result.Forbidden)
	require.Equal(t, 1, result.Skipped) // invalid_grant skipped

	// recovered account side effects (ClearGrokHoldAfterSuccess on the candidate copy)
	require.Equal(t, 1, baseRepo.clearErrorCalls)
	require.Equal(t, int64(11), baseRepo.lastClearErrorID)
	require.Equal(t, 1, baseRepo.setSchedulableCalls)
	require.Equal(t, int64(11), baseRepo.lastSetSchedulableID)
	require.True(t, baseRepo.lastSetSchedulable)

	// probe metadata written
	require.Equal(t, grokErrorRecoveryClassRecovered, baseRepo.updates[11][grokErrorRecoveryLastClassExtraKey])
	require.Equal(t, grokErrorRecoveryClassForbidden, baseRepo.updates[12][grokErrorRecoveryLastClassExtraKey])
	require.NotEmpty(t, baseRepo.updates[11][grokErrorRecoveryLastProbeAtExtraKey])
	require.NotEmpty(t, baseRepo.updates[12][grokErrorRecoveryLastProbeAtExtraKey])
}

func TestGrokErrorRecoveryServiceStartStopDisabled(t *testing.T) {
	cfg := &config.Config{GrokErrorRecovery: config.GrokErrorRecoveryConfig{Enabled: false}}
	svc := NewGrokErrorRecoveryService(nil, nil, cfg)
	svc.Start()
	svc.Stop()
}
