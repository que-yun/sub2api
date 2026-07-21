package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// sessionWindowSyncRepo 记录 syncActiveToPassive 触发的所有写操作。
type sessionWindowSyncRepo struct {
	AccountRepository

	mu                sync.Mutex
	extraUpdates      []map[string]any
	sessionWindowEnds []sessionWindowEndCall
}

type sessionWindowEndCall struct {
	AccountID int64
	End       time.Time
}

func (r *sessionWindowSyncRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[string]any, len(updates))
	for k, v := range updates {
		copied[k] = v
	}
	r.extraUpdates = append(r.extraUpdates, copied)
	return nil
}

func (r *sessionWindowSyncRepo) UpdateSessionWindowEnd(_ context.Context, id int64, end time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionWindowEnds = append(r.sessionWindowEnds, sessionWindowEndCall{AccountID: id, End: end})
	return nil
}

func TestEstimateSetupTokenUsage_ExpiredWindowZeroes(t *testing.T) {
	t.Parallel()

	past := time.Now().Add(-2 * time.Hour)
	svc := &AccountUsageService{}
	info := svc.estimateSetupTokenUsage(&Account{
		SessionWindowEnd: &past,
		Extra: map[string]any{
			"session_window_utilization": 0.53,
		},
	})

	if info.FiveHour == nil {
		t.Fatal("expected non-nil FiveHour info")
	}
	if info.FiveHour.Utilization != 0 {
		t.Fatalf("expected Utilization=0 for expired window, got %v", info.FiveHour.Utilization)
	}
	if info.FiveHour.ResetsAt != nil {
		t.Fatalf("expected ResetsAt=nil for expired window, got %v", info.FiveHour.ResetsAt)
	}
	if info.FiveHour.RemainingSeconds != 0 {
		t.Fatalf("expected RemainingSeconds=0 for expired window, got %v", info.FiveHour.RemainingSeconds)
	}
}

func TestEstimateSetupTokenUsage_ActiveWindowPreservesUtilization(t *testing.T) {
	t.Parallel()

	future := time.Now().Add(3 * time.Hour)
	svc := &AccountUsageService{}
	info := svc.estimateSetupTokenUsage(&Account{
		SessionWindowEnd: &future,
		Extra: map[string]any{
			"session_window_utilization": 0.53,
		},
	})

	if info.FiveHour == nil {
		t.Fatal("expected non-nil FiveHour info")
	}
	if info.FiveHour.Utilization != 53 {
		t.Fatalf("expected Utilization=53, got %v", info.FiveHour.Utilization)
	}
	if info.FiveHour.ResetsAt == nil || !info.FiveHour.ResetsAt.Equal(future) {
		t.Fatalf("expected ResetsAt=%v, got %v", future, info.FiveHour.ResetsAt)
	}
	if info.FiveHour.RemainingSeconds <= 0 {
		t.Fatalf("expected positive RemainingSeconds, got %v", info.FiveHour.RemainingSeconds)
	}
}

func TestSyncActiveToPassive_WritesFiveHourSessionWindowEnd(t *testing.T) {
	t.Parallel()

	repo := &sessionWindowSyncRepo{}
	svc := &AccountUsageService{accountRepo: repo}
	resetsAt := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	svc.syncActiveToPassive(context.Background(), 42, &UsageInfo{
		FiveHour: &UsageProgress{
			Utilization: 53,
			ResetsAt:    &resetsAt,
		},
	})

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.sessionWindowEnds) != 1 {
		t.Fatalf("expected 1 UpdateSessionWindowEnd call, got %d", len(repo.sessionWindowEnds))
	}
	call := repo.sessionWindowEnds[0]
	if call.AccountID != 42 {
		t.Fatalf("expected AccountID=42, got %d", call.AccountID)
	}
	if !call.End.Equal(resetsAt) {
		t.Fatalf("expected End=%v, got %v", resetsAt, call.End)
	}
}

func TestSyncActiveToPassive_SkipsSessionWindowEndWhenResetMissing(t *testing.T) {
	t.Parallel()

	repo := &sessionWindowSyncRepo{}
	svc := &AccountUsageService{accountRepo: repo}
	svc.syncActiveToPassive(context.Background(), 99, &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 10},
	})

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.sessionWindowEnds) != 0 {
		t.Fatalf("expected no UpdateSessionWindowEnd calls when ResetsAt is nil, got %d", len(repo.sessionWindowEnds))
	}
}

// windowStatsRepoStub 记录 GetAccountWindowStats 的起止时间与返回值，供 addWindowStats 单测使用。
type windowStatsRepoStub struct {
	UsageLogRepository
	mu         sync.Mutex
	starts     []time.Time
	byOrder    []*usagestats.AccountStats
	byStartFn  func(start time.Time) *usagestats.AccountStats
	err        error
	callCount  int
}

func (r *windowStatsRepoStub) GetAccountWindowStats(_ context.Context, _ int64, start time.Time) (*usagestats.AccountStats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.callCount++
	r.starts = append(r.starts, start)
	if r.err != nil {
		return nil, r.err
	}
	if r.byStartFn != nil {
		return r.byStartFn(start), nil
	}
	idx := len(r.starts) - 1
	if idx < len(r.byOrder) {
		return r.byOrder[idx], nil
	}
	return &usagestats.AccountStats{}, nil
}

func TestAddWindowStats_AttachesFiveHourAndSevenDayLocalStats(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessionStart := now.Add(-2 * time.Hour)
	sessionEnd := now.Add(3 * time.Hour)
	sevenReset := now.Add(48 * time.Hour)

	repo := &windowStatsRepoStub{
		byStartFn: func(start time.Time) *usagestats.AccountStats {
			// 5h 窗起点较近；7d 窗起点更早
			if start.After(now.Add(-24 * time.Hour)) {
				return &usagestats.AccountStats{Requests: 5, Tokens: 500, Cost: 1.5, StandardCost: 1.5, UserCost: 0.8}
			}
			return &usagestats.AccountStats{Requests: 90, Tokens: 9000, Cost: 42, StandardCost: 42, UserCost: 30}
		},
	}
	svc := &AccountUsageService{
		usageLogRepo: repo,
		cache:        NewUsageCache(),
	}
	account := &Account{
		ID:                 21330,
		SessionWindowStart: &sessionStart,
		SessionWindowEnd:   &sessionEnd,
	}
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 16},
		SevenDay: &UsageProgress{Utilization: 56, ResetsAt: &sevenReset},
	}

	svc.addWindowStats(context.Background(), account, usage)

	if usage.FiveHour.WindowStats == nil || usage.FiveHour.WindowStats.Requests != 5 {
		t.Fatalf("expected 5h local stats requests=5, got %+v", usage.FiveHour.WindowStats)
	}
	if usage.SevenDay.WindowStats == nil || usage.SevenDay.WindowStats.Requests != 90 {
		t.Fatalf("expected 7d local stats requests=90, got %+v", usage.SevenDay.WindowStats)
	}
	if repo.callCount != 2 {
		t.Fatalf("expected 2 window queries, got %d", repo.callCount)
	}

	// 缓存命中：再次调用不应再查库
	usage2 := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 16},
		SevenDay: &UsageProgress{Utilization: 56, ResetsAt: &sevenReset},
	}
	svc.addWindowStats(context.Background(), account, usage2)
	if repo.callCount != 2 {
		t.Fatalf("expected cache hit (still 2 queries), got %d", repo.callCount)
	}
	if usage2.SevenDay.WindowStats == nil || usage2.SevenDay.WindowStats.Tokens != 9000 {
		t.Fatalf("expected cached 7d tokens=9000, got %+v", usage2.SevenDay.WindowStats)
	}
}
