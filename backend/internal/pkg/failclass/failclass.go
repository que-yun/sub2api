// Package failclass 把上游调用失败集中归类成统一的「类别 + 恢复策略」。
//
// 这是把现在散落在各 handler 的网络错/状态码判断收敛成一处的纯函数,便于 failsafe-go 等
// 失败策略层统一复用。两个硬约束直接编码进类别与策略:
//
//  1. 测活频率 / 封号:恢复一律「被动」——让下一次真实流量去探活,不发任何合成探测请求;
//     配额类必须等到 reset 时刻才放行(RecoverWaitReset),reset 前不重试、不探测。
//     => 从源头杜绝「高频/定期探测导致封号」。
//  2. 阶段性不稳定:区分「永久失效(禁用)」与「阶段性不稳定(短重试,不永久禁)」,
//     避免把偶发抖动的好账号误杀。
//
// 关键不变量:任何传输/网络/VPN 错误一律 TransportLocal 且 PenalizeHard=false——本机/VPN 问题绝不归咎账号。
package failclass

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

type Category int

const (
	Success          Category = iota
	TransportLocal            // 本机/网络/VPN:连不上/超时/TLS/代理/DNS/EOF。绝不归咎账号。
	AccountPermanent          // 账号永久失效:401 无效 key、403 封号。禁用,需人工/重校验。
	AccountQuota              // 配额/限流:429、7天/月配额。冷却到 reset,reset 前不探测。
	AccountTransient          // 阶段性不稳定:5xx、间歇空响应/偶发抖动。短退避重试,不永久禁。
	Unknown                   // 无法判定:保守不归咎账号(宁漏禁不误杀)。
)

func (c Category) String() string {
	switch c {
	case Success:
		return "success"
	case TransportLocal:
		return "transport_local"
	case AccountPermanent:
		return "account_permanent"
	case AccountQuota:
		return "account_quota"
	case AccountTransient:
		return "account_transient"
	default:
		return "unknown"
	}
}

type Recovery int

const (
	RecoverNone      Recovery = iota // 账号本身没问题(传输错)
	RecoverPassive                   // 被动:下次真实流量探活,不发合成探测
	RecoverWaitReset                 // 等到配额 reset;reset 前不探测/不重试
	RecoverManual                    // 永久失效:不自动恢复
)

func (r Recovery) String() string {
	switch r {
	case RecoverNone:
		return "none"
	case RecoverPassive:
		return "passive_real_traffic"
	case RecoverWaitReset:
		return "wait_quota_reset"
	default:
		return "manual_only"
	}
}

// Result 归类结果 + 给失败策略层的提示。
type Result struct {
	Category         Category
	PenalizeHard     bool          // 计入「硬熔断/禁用」;仅 AccountPermanent。传输/5xx/配额都不计硬熔断。
	RetrySameAccount bool          // 阶段性不稳定:值得同账号短重试(配合退避+jitter)。
	Recovery         Recovery      // 恢复方式;被动优先,杜绝高频合成探测导致封号。
	CooldownHint     time.Duration // 建议冷却时长;AccountQuota 用 reset 时长,其它为短值或 0。
	Reason           string
}

// Classify 集中归类。
//
//	status:     HTTP 状态码(无响应时传 0)
//	err:        传输层错误(无则 nil)
//	body:       上游响应体(可空)
//	retryAfter: 上游给的 Retry-After / reset 时长(无则 0)
func Classify(status int, err error, body []byte, retryAfter time.Duration) Result {
	// 1) 传输/网络层最先判;命中即绝不归咎账号(对应「本机/VPN 别误杀」)
	if err != nil && isTransport(err) {
		return Result{Category: TransportLocal, Recovery: RecoverNone, Reason: "transport/network/vpn: " + err.Error()}
	}
	low := strings.ToLower(string(body))

	// 2) 配额/限流:冷却到 reset,reset 前不探测/不重试
	if status == 429 || matchAny(low, "too many requests", "rate limit", "rate_limit",
		"insufficient_quota", "quota", "7d_limit", "limit_exhausted") {
		return Result{Category: AccountQuota, Recovery: RecoverWaitReset, CooldownHint: retryAfter, Reason: "quota/rate-limit"}
	}

	// 3) 账号永久失效:无效 key / 封号 / 无权限
	if status == 401 || status == 403 || matchAny(low, "incorrect api key", "invalid api key",
		"invalid_api_key", "无效的令牌", "unauthorized", "permission denied") {
		return Result{Category: AccountPermanent, PenalizeHard: true, Recovery: RecoverManual, Reason: "auth/permission failure"}
	}

	// 4) 上游 5xx / 间歇错:阶段性不稳定 → 短重试 + 被动恢复,不永久禁
	if status >= 500 || matchAny(low, "internal server error", "bad gateway", "service unavailable",
		"gateway timeout", "overloaded", "temporar") {
		return Result{Category: AccountTransient, RetrySameAccount: true, Recovery: RecoverPassive, Reason: "upstream 5xx/transient"}
	}

	// 5) 2xx 且无传输错 → 成功
	if status >= 200 && status < 300 && err == nil {
		return Result{Category: Success, Recovery: RecoverNone}
	}

	// 6) 兜底:保守不归咎账号(宁漏禁不误杀)
	return Result{Category: Unknown, RetrySameAccount: true, Recovery: RecoverPassive, Reason: "unclassified; conservative no-penalize"}
}

func matchAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// isTransport 判断是否本机/网络/VPN 传输层错误(绝不归咎账号)。
// 先用类型/哨兵判断,再退回字符串匹配兜底(覆盖被包裹的传输错)。
func isTransport(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return matchAny(msg,
		"dial tcp", "connection refused", "connection reset", "i/o timeout",
		"no such host", "tls handshake", "tls:", "proxyconnect", "network is unreachable",
		"eof", "context deadline", "broken pipe", "request_error", "probe unavailable",
		"client.timeout", "no route to host",
	)
}
