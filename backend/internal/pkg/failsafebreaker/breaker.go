// Package failsafebreaker:组内「健康分级选路 + 失败转移」,failsafe-go 做 transient 断路器内核。
//
// 三档:Healthy(优先,preferred 粘住)/ Degraded(间断,降级备用)/ Broken(不可用,跳过)。
// Broken 三种来源 + 关键语义:冷却 = 「降到最后才用」,不是「完全不能用」。
//
//	· 永久失效 401/403 无效key → disabled(成功才恢复)
//	· 配额/限流 429/quota       → 冷却到 reset(到点自动恢复,期间不主动探测)
//	· 持续 transient/5xx        → 断路器 OPEN,半开被动恢复
//
// 全 Broken 时 Route 会「panic 兜底」:**绕过冷却**强制试一发,按最可能成功排序
//
//	(软冷却 transient > 配额 > 死key),做到「一定要找到一个」,冷却不致让账号彻底不可用。
//
// 保证:① Route 同请求内排除已失败号,绝不重复打同一报错渠道;② 持续失败号熔断,后续请求跳过;
//
//	③ 冷却中的号在「没有更好选择」时仍可被强制试(forceExecute),不会因冷却而完全用不了。
//
// 传输/VPN 错不罚账号;恢复全程被动(真实流量),无合成探测。
package failsafebreaker

import (
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/failclass"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
)

type Tier int

const (
	TierHealthy Tier = iota
	TierDegraded
	TierBroken
)

func (t Tier) String() string {
	switch t {
	case TierHealthy:
		return "healthy"
	case TierDegraded:
		return "degraded"
	default:
		return "broken"
	}
}

// brokenReason 决定全 Broken 时谁先被强制试(越小越可能成功,优先试)。
type brokenReason int

const (
	reasonSoftCooled brokenReason = iota // 持续 5xx/transient 冷却:可能已恢复 → 最先强制试
	reasonQuota                          // 配额到 reset:reset 前多半还不行,但仍比死 key 强
	reasonDisabled                       // 401/403 死 key:基本不会成,最后试
)

type Config struct {
	FailureThreshold     uint
	OpenDelay            time.Duration
	SuccessThreshold     uint
	DegradeWindow        time.Duration
	DefaultQuotaCooldown time.Duration
	OnStateChange        func(accountID int64, from, to string)
}

func DefaultConfig() Config {
	return Config{FailureThreshold: 3, OpenDelay: 30 * time.Second, SuccessThreshold: 1, DegradeWindow: 90 * time.Second, DefaultQuotaCooldown: 5 * time.Minute}
}

type Registry struct {
	mu         sync.Mutex
	cfg        Config
	breakers   map[int64]circuitbreaker.CircuitBreaker[any]
	lastFail   map[int64]time.Time
	quotaUntil map[int64]time.Time
	disabled   map[int64]bool
}

func NewRegistry(cfg Config) *Registry {
	if cfg.FailureThreshold == 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.OpenDelay == 0 {
		cfg.OpenDelay = 30 * time.Second
	}
	if cfg.SuccessThreshold == 0 {
		cfg.SuccessThreshold = 1
	}
	if cfg.DegradeWindow == 0 {
		cfg.DegradeWindow = 90 * time.Second
	}
	if cfg.DefaultQuotaCooldown == 0 {
		cfg.DefaultQuotaCooldown = 5 * time.Minute
	}
	return &Registry{cfg: cfg, breakers: map[int64]circuitbreaker.CircuitBreaker[any]{}, lastFail: map[int64]time.Time{}, quotaUntil: map[int64]time.Time{}, disabled: map[int64]bool{}}
}

func (r *Registry) breakerFor(accountID int64) circuitbreaker.CircuitBreaker[any] {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cb, ok := r.breakers[accountID]; ok {
		return cb
	}
	b := circuitbreaker.NewBuilder[any]().
		WithFailureThreshold(r.cfg.FailureThreshold).WithDelay(r.cfg.OpenDelay).WithSuccessThreshold(r.cfg.SuccessThreshold)
	if r.cfg.OnStateChange != nil {
		id := accountID
		b = b.OnStateChanged(func(e circuitbreaker.StateChangedEvent) {
			r.cfg.OnStateChange(id, stateName(e.OldState), stateName(e.NewState))
		})
	}
	cb := b.Build()
	r.breakers[accountID] = cb
	return cb
}

// Execute:permit 放行才调 fn(冷却中拒)。
func (r *Registry) Execute(accountID int64, fn func() (int, []byte, error)) (res failclass.Result, allowed bool) {
	cb := r.breakerFor(accountID)
	if !cb.TryAcquirePermit() {
		return failclass.Result{Category: failclass.Unknown, Reason: "cooling/open"}, false
	}
	status, body, err := fn()
	res = failclass.Classify(status, err, body, 0)
	r.record(cb, accountID, res)
	return res, true
}

// forceExecute:**绕过冷却**强制调 fn(panic 兜底用)。成功则立即恢复该号。
func (r *Registry) forceExecute(accountID int64, fn func() (int, []byte, error)) failclass.Result {
	status, body, err := fn()
	res := failclass.Classify(status, err, body, 0)
	cb := r.breakerFor(accountID)
	if res.Category == failclass.Success {
		cb.Close()
		r.mu.Lock()
		delete(r.disabled, accountID)
		delete(r.quotaUntil, accountID)
		delete(r.lastFail, accountID)
		r.mu.Unlock()
		return res
	}
	r.record(cb, accountID, res) // 失败:照常记账(不额外加重)
	return res
}

func (r *Registry) Record(accountID int64, res failclass.Result) {
	r.record(r.breakerFor(accountID), accountID, res)
}

func (r *Registry) record(cb circuitbreaker.CircuitBreaker[any], accountID int64, res failclass.Result) {
	switch res.Category {
	case failclass.AccountTransient:
		cb.RecordFailure()
		r.mu.Lock()
		r.lastFail[accountID] = time.Now()
		r.mu.Unlock()
	case failclass.AccountPermanent:
		cb.RecordSuccess()
		r.mu.Lock()
		r.disabled[accountID] = true
		r.mu.Unlock()
	case failclass.AccountQuota:
		cb.RecordSuccess()
		cd := res.CooldownHint
		if cd <= 0 {
			cd = r.cfg.DefaultQuotaCooldown
		}
		r.mu.Lock()
		r.quotaUntil[accountID] = time.Now().Add(cd)
		r.mu.Unlock()
	case failclass.Success:
		cb.RecordSuccess()
		r.mu.Lock()
		delete(r.lastFail, accountID)
		delete(r.quotaUntil, accountID)
		delete(r.disabled, accountID)
		r.mu.Unlock()
	default:
		cb.RecordSuccess()
	}
}

func (r *Registry) Tier(accountID int64) Tier {
	r.mu.Lock()
	if r.disabled[accountID] {
		r.mu.Unlock()
		return TierBroken
	}
	if until, ok := r.quotaUntil[accountID]; ok && time.Now().Before(until) {
		r.mu.Unlock()
		return TierBroken
	}
	lastFail, hadFail := r.lastFail[accountID]
	r.mu.Unlock()
	cb := r.breakerFor(accountID)
	if cb.IsOpen() {
		return TierBroken
	}
	if cb.IsHalfOpen() || (hadFail && time.Since(lastFail) < r.cfg.DegradeWindow) {
		return TierDegraded
	}
	return TierHealthy
}

func (r *Registry) brokenReason(accountID int64) brokenReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled[accountID] {
		return reasonDisabled
	}
	if until, ok := r.quotaUntil[accountID]; ok && time.Now().Before(until) {
		return reasonQuota
	}
	return reasonSoftCooled
}

type Pick struct {
	AccountID int64
	Tier      Tier
	Panic     bool
}

func (r *Registry) PickBest(ids []int64, preferred int64) (Pick, bool) {
	if len(ids) == 0 {
		return Pick{}, false
	}
	var healthy, degraded, broken []int64
	for _, id := range ids {
		switch r.Tier(id) {
		case TierHealthy:
			healthy = append(healthy, id)
		case TierDegraded:
			degraded = append(degraded, id)
		default:
			broken = append(broken, id)
		}
	}
	if preferred != 0 {
		for _, id := range healthy {
			if id == preferred {
				return Pick{AccountID: id, Tier: TierHealthy}, true
			}
		}
	}
	if len(healthy) > 0 {
		return Pick{AccountID: healthy[0], Tier: TierHealthy}, true
	}
	if len(degraded) > 0 {
		r.mu.Lock()
		sort.Slice(degraded, func(i, j int) bool { return r.lastFail[degraded[i]].Before(r.lastFail[degraded[j]]) })
		r.mu.Unlock()
		return Pick{AccountID: degraded[0], Tier: TierDegraded}, true
	}
	// 全 Broken:按「最可能成功」排 —— 软冷却 > 配额 > 死key;同因再按剩余恢复时间
	sort.Slice(broken, func(i, j int) bool {
		ri, rj := r.brokenReason(broken[i]), r.brokenReason(broken[j])
		if ri != rj {
			return ri < rj
		}
		return r.breakerFor(broken[i]).RemainingDelay() < r.breakerFor(broken[j]).RemainingDelay()
	})
	return Pick{AccountID: broken[0], Tier: TierBroken, Panic: true}, true
}

// Route:组内完整失败转移(见包注释三条保证)。panic 兜底绕过冷却强制试。
func (r *Registry) Route(ids []int64, preferred int64, call func(accountID int64) (status int, body []byte, err error)) (accountID int64, res failclass.Result, ok bool) {
	excluded := make(map[int64]bool)
	var lastID int64
	var lastRes failclass.Result
	for {
		avail := make([]int64, 0, len(ids))
		for _, id := range ids {
			if !excluded[id] {
				avail = append(avail, id)
			}
		}
		pick, found := r.PickBest(avail, preferred)
		if !found {
			return lastID, lastRes, false
		}
		wrapped := func() (int, []byte, error) { return call(pick.AccountID) }
		if pick.Panic {
			res = r.forceExecute(pick.AccountID, wrapped) // 全坏:绕过冷却硬试一发
		} else {
			var allowed bool
			res, allowed = r.Execute(pick.AccountID, wrapped)
			if !allowed { // 选后到执行间被冷却(竞态):排除重选
				excluded[pick.AccountID] = true
				continue
			}
		}
		if res.Category == failclass.Success {
			return pick.AccountID, res, true
		}
		excluded[pick.AccountID] = true
		preferred = 0
		lastID, lastRes = pick.AccountID, res
	}
}

func (r *Registry) State(accountID int64) string { return stateName(r.breakerFor(accountID).State()) }

func (r *Registry) Snapshot() map[int64]Tier {
	r.mu.Lock()
	ids := make([]int64, 0, len(r.breakers))
	for id := range r.breakers {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	out := make(map[int64]Tier, len(ids))
	for _, id := range ids {
		out[id] = r.Tier(id)
	}
	return out
}

func stateName(s circuitbreaker.State) string {
	switch s {
	case circuitbreaker.ClosedState:
		return "closed"
	case circuitbreaker.OpenState:
		return "open"
	case circuitbreaker.HalfOpenState:
		return "half_open"
	default:
		return "unknown"
	}
}
