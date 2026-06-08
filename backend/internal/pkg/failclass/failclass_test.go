package failclass

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// 用 mini 上真实出现过的错误语料(日志 + accounts.error_message)做表驱动验证。
func TestClassify_RealCorpus(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		err      error
		body     string
		want     Category
		penalize bool
	}{
		// —— 传输/网络/VPN:绝不归咎账号(你最担心的一类)——
		{"dial_tcp_timeout", 0, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}, "", TransportLocal, false},
		{"connection_refused", 0, errors.New("dial tcp 1.2.3.4:443: connect: connection refused"), "", TransportLocal, false},
		{"context_deadline", 0, context.DeadlineExceeded, "", TransportLocal, false},
		{"eof", 0, errors.New("unexpected EOF"), "", TransportLocal, false},
		{"probe_request_error", 0, errors.New("Health probe unavailable: request_error"), "", TransportLocal, false},
		// —— 账号永久失效:401 无效 key（真死号,该禁）——
		{"incorrect_api_key", 401, nil, `{"error":{"message":"Incorrect API key provided: sk-xxx"}}`, AccountPermanent, true},
		{"invalid_token_zh", 401, nil, `{"error":{"message":"无效的令牌"}}`, AccountPermanent, true},
		{"invalid_api_key", 401, nil, "invalid API key", AccountPermanent, true},
		// —— 配额/限流:冷却到 reset,不永久禁 ——
		{"429_rate_limit", 429, nil, "Too Many Requests", AccountQuota, false},
		{"7d_quota", 429, nil, `{"error":"openai_429_7d_limit_exhausted"}`, AccountQuota, false},
		// —— 上游 5xx:阶段性不稳定,短重试 ——
		{"503_unavailable", 503, nil, "service unavailable", AccountTransient, false},
		// —— 成功 ——
		{"ok_200", 200, nil, `{"ok":true}`, Success, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.status, tt.err, []byte(tt.body), 0)
			if got.Category != tt.want {
				t.Errorf("category = %v, want %v (reason=%q)", got.Category, tt.want, got.Reason)
			}
			if got.PenalizeHard != tt.penalize {
				t.Errorf("PenalizeHard = %v, want %v", got.PenalizeHard, tt.penalize)
			}
		})
	}
}

// 约束1(反面):任何传输/网络/VPN 错都不得归咎账号——本机/VPN 不误杀。
func TestClassify_TransportNeverPenalizes(t *testing.T) {
	transportErrs := []error{
		context.DeadlineExceeded, context.Canceled,
		&net.OpError{Op: "dial", Err: errors.New("connection refused")},
		&net.DNSError{Err: "no such host"},
		errors.New("net/http: TLS handshake timeout"),
		errors.New("proxyconnect tcp: dial tcp: i/o timeout"),
		errors.New("unexpected EOF"),
		errors.New("read: connection reset by peer"),
	}
	for _, e := range transportErrs {
		r := Classify(0, e, nil, 0)
		if r.Category != TransportLocal || r.PenalizeHard {
			t.Errorf("传输错 %q 被误判: cat=%v penalize=%v", e, r.Category, r.PenalizeHard)
		}
	}
}

// 约束1(正面):配额类必须 WaitReset,reset 前不探测/不重试 → 杜绝高频探测封号。
func TestClassify_QuotaWaitsResetNoProbe(t *testing.T) {
	r := Classify(429, nil, []byte("quota"), 7*24*time.Hour)
	if r.Category != AccountQuota {
		t.Fatalf("应为 AccountQuota,得 %v", r.Category)
	}
	if r.Recovery != RecoverWaitReset {
		t.Errorf("配额恢复应 WaitReset(reset前不探测),得 %v", r.Recovery)
	}
	if r.CooldownHint != 7*24*time.Hour {
		t.Errorf("冷却时长应等于 retryAfter,得 %v", r.CooldownHint)
	}
	if r.RetrySameAccount {
		t.Errorf("配额耗尽不应同账号重试(会触发更多封号风险)")
	}
}

// 约束2:阶段性不稳定(5xx)应可同账号短重试、被动恢复,且不永久禁。
func TestClassify_TransientRetryableNotKilled(t *testing.T) {
	r := Classify(503, nil, []byte("service unavailable"), 0)
	if r.Category != AccountTransient {
		t.Fatalf("5xx 应为 AccountTransient,得 %v", r.Category)
	}
	if !r.RetrySameAccount {
		t.Errorf("阶段性不稳定应允许同账号短重试")
	}
	if r.PenalizeHard {
		t.Errorf("阶段性不稳定不应计入硬熔断/永久禁")
	}
	if r.Recovery != RecoverPassive {
		t.Errorf("应被动恢复(下次真实流量探活),得 %v", r.Recovery)
	}
}

// 恢复策略绝不使用「主动合成探测」——本分类器只产出 None/Passive/WaitReset/Manual。
func TestClassify_NoActiveProbeRecovery(t *testing.T) {
	cases := []Result{
		Classify(0, context.DeadlineExceeded, nil, 0),
		Classify(401, nil, []byte("invalid API key"), 0),
		Classify(429, nil, []byte("quota"), time.Hour),
		Classify(503, nil, []byte("oops"), 0),
	}
	for _, r := range cases {
		switch r.Recovery {
		case RecoverNone, RecoverPassive, RecoverWaitReset, RecoverManual:
			// ok:全是被动/等待/人工,无主动探测
		default:
			t.Errorf("出现非被动恢复策略: %v", r.Recovery)
		}
	}
}
