package failsafebreaker

import (
	"context"
	"testing"
	"time"
)

func fastCfg() Config {
	return Config{FailureThreshold: 3, OpenDelay: 150 * time.Millisecond, SuccessThreshold: 1, DegradeWindow: 2 * time.Second, DefaultQuotaCooldown: 2 * time.Second}
}
func panicCfg() Config {
	return Config{FailureThreshold: 3, OpenDelay: 5 * time.Second, SuccessThreshold: 1, DegradeWindow: 5 * time.Second, DefaultQuotaCooldown: 5 * time.Second}
}

func c503() (int, []byte, error) { return 503, []byte("service unavailable"), nil }
func c401() (int, []byte, error) { return 401, []byte("invalid API key"), nil }
func c429() (int, []byte, error) { return 429, []byte("Too Many Requests"), nil }
func cOK() (int, []byte, error)  { return 200, []byte("{}"), nil }
func cNet() (int, []byte, error) { return 0, nil, context.DeadlineExceeded }

// ========== 你的要求①:绝不一直打报错渠道,用户重试经可用号成功 ==========
func TestGuarantee1_NeverStuckViaWorkingChannel(t *testing.T) {
	scenarios := []struct {
		name string
		bad  func() (int, []byte, error)
	}{
		{"bad_5xx", c503},
		{"bad_401_deadkey", c401},
		{"bad_quota", c429},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			r := NewRegistry(panicCfg())
			const bad, good = int64(1), int64(2)
			call := func(id int64) (int, []byte, error) {
				if id == bad {
					return sc.bad()
				}
				return cOK()
			}
			// 连发 5 次(模拟用户重试):每次都必须经 good 成功,绝不卡在 bad
			for i := 0; i < 5; i++ {
				gotID, _, ok := r.Route([]int64{bad, good}, 0, call)
				if !ok || gotID != good {
					t.Fatalf("第%d次请求应经 good(%d) 成功,得 id=%d ok=%v", i+1, good, gotID, ok)
				}
			}
			if r.Tier(bad) == TierHealthy {
				t.Errorf("坏渠道应被降级/熔断,不应仍 healthy,得 %s", r.Tier(bad))
			}
		})
	}
}

// 全坏也不空手:Route 会 panic 兜底挑一个试;只有真没有可用号才返回失败(且已逐个试过,不死循环)
func TestGuarantee1_AllBadTriesEachOnceNoLoop(t *testing.T) {
	r := NewRegistry(panicCfg())
	tries := map[int64]int{}
	call := func(id int64) (int, []byte, error) { tries[id]++; return c503() }
	_, _, ok := r.Route([]int64{1, 2, 3}, 0, call)
	if ok {
		t.Fatalf("全坏应失败")
	}
	for id, n := range tries {
		if n != 1 {
			t.Errorf("每个号本次请求应只试 1 次(不重复打),账号 %d 试了 %d 次", id, n)
		}
	}
	if len(tries) != 3 {
		t.Errorf("应把 3 个号都试过,只试了 %d 个", len(tries))
	}
}

// ========== 你的要求②:degrade vs break 按真实错误类型分清 ==========
func TestGuarantee2_DegradeVsBreak(t *testing.T) {
	type tc struct {
		name string
		run  func(r *Registry, id int64)
		want Tier
	}
	cases := []tc{
		{"401无效key=熔断", func(r *Registry, id int64) { r.Execute(id, c401) }, TierBroken},
		{"429配额=熔断到reset", func(r *Registry, id int64) { r.Execute(id, c429) }, TierBroken},
		{"持续5xx_x3=熔断", func(r *Registry, id int64) {
			for i := 0; i < 3; i++ {
				r.Execute(id, c503)
			}
		}, TierBroken},
		{"间断5xx_x1=降级", func(r *Registry, id int64) { r.Execute(id, c503) }, TierDegraded},
		{"传输VPN错=不罚保持健康", func(r *Registry, id int64) {
			for i := 0; i < 5; i++ {
				r.Execute(id, cNet)
			}
		}, TierHealthy},
		{"成功=健康", func(r *Registry, id int64) { r.Execute(id, cOK) }, TierHealthy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewRegistry(fastCfg())
			c.run(r, 1)
			if got := r.Tier(1); got != c.want {
				t.Errorf("%s:应 %s,得 %s", c.name, c.want, got)
			}
		})
	}
}

// 选路:健康优先 + preferred 粘住
func TestPickBest_StickyAndFailover(t *testing.T) {
	r := NewRegistry(panicCfg())
	p, ok := r.PickBest([]int64{10, 11, 12}, 11)
	if !ok || p.AccountID != 11 {
		t.Errorf("preferred 应粘住 11,得 %+v", p)
	}
	for i := 0; i < 3; i++ {
		r.Execute(20, c503) // 熔断 20
	}
	p2, _ := r.PickBest([]int64{20, 21}, 0)
	if p2.AccountID != 21 || p2.Tier != TierHealthy {
		t.Errorf("应跳过熔断的 20 选 21,得 %+v", p2)
	}
}

// 全 Broken → panic 挑最接近恢复的
func TestPickBest_PanicClosestToRecovery(t *testing.T) {
	r := NewRegistry(panicCfg())
	for i := 0; i < 3; i++ {
		r.Execute(40, c503)
	}
	time.Sleep(80 * time.Millisecond)
	for i := 0; i < 3; i++ {
		r.Execute(41, c503)
	}
	p, ok := r.PickBest([]int64{40, 41}, 0)
	if !ok || !p.Panic || p.AccountID != 40 {
		t.Errorf("全熔断应 panic 挑最接近恢复的 40,得 %+v ok=%v", p, ok)
	}
}

// 被动半开恢复(真实流量,无合成探测)
func TestPassiveHalfOpenRecovery(t *testing.T) {
	r := NewRegistry(fastCfg())
	for i := 0; i < 3; i++ {
		r.Execute(50, c503)
	}
	if r.Tier(50) != TierBroken {
		t.Fatalf("应先熔断")
	}
	time.Sleep(220 * time.Millisecond)
	if _, _, ok := r.Route([]int64{50}, 0, func(int64) (int, []byte, error) { return cOK() }); !ok {
		t.Fatalf("过 delay 后真实流量应能半开恢复并成功")
	}
	if r.Tier(50) != TierHealthy {
		t.Errorf("半开成功后应回 healthy,得 %s", r.Tier(50))
	}
}

// 你报的冷却 bug:冷却中的号在「没有更好选择」时,必须能被强制试,绝不因冷却而完全不可用。
func TestCooldownIsNotHardBlock(t *testing.T) {
	r := NewRegistry(panicCfg()) // OpenDelay 5s,普通 Execute 会被拒
	for i := 0; i < 3; i++ {
		r.Execute(1, c503) // 熔断/冷却账号 1
	}
	if r.Tier(1) != TierBroken {
		t.Fatalf("应先进入冷却(broken)")
	}
	// 账号 1 此刻已恢复(call 返回成功),但仍在 5s 冷却窗口内。
	// 旧逻辑:Execute 被 permit 拒 → 用户失败。新逻辑:panic 兜底绕过冷却强制试 → 成功。
	gotID, _, ok := r.Route([]int64{1}, 0, func(int64) (int, []byte, error) { return cOK() })
	if !ok || gotID != 1 {
		t.Fatalf("冷却中但没有更好选择时,应绕过冷却强制试并成功,得 id=%d ok=%v", gotID, ok)
	}
	if r.Tier(1) != TierHealthy {
		t.Errorf("强制试成功后应立即恢复 healthy,得 %s", r.Tier(1))
	}
}

// 全 Broken 时,panic 优先试「最可能成功」的软冷却号,而不是死 key。
func TestPanicPrefersSoftOverDeadKey(t *testing.T) {
	r := NewRegistry(panicCfg())
	r.Execute(1, c401)       // 死 key → disabled
	for i := 0; i < 3; i++ { // 软冷却(持续 5xx)
		r.Execute(2, c503)
	}
	p, ok := r.PickBest([]int64{1, 2}, 0)
	if !ok || !p.Panic || p.AccountID != 2 {
		t.Errorf("panic 应优先试软冷却的 2,而非死key 1,得 %+v", p)
	}
}
