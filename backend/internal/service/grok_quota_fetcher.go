package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

const grokQuotaSnapshotExtraKey = "grok_usage_snapshot"

// grokNegativeSnapshotFreshWindow 限定 401/403 这类"负面"快照多久内才被采信为实时权限状态。
//
// 背景：单次 403(permission-denied)/401 会写入快照并永久钉住，直到下一次成功探测覆盖。
// xAI 侧的权限判定会波动(例如故障窗口内短暂 403、之后又放行)，若继续据此显示 forbidden，
// 会把实际已恢复可用的账号误标为封禁(账号 status 是"正常"绿灯却挂红色 forbidden，自相矛盾)。
//
// 正在被调度尝试的账号，每次转发都会用真实响应刷新快照(见 openai_gateway_grok.go 的
// updateGrokUsageSnapshot)，因此不会陈旧；只有"已恢复但长期无流量复测"的账号会挂着旧的
// 负面快照。超过该窗口即视为陈旧、不再断言 forbidden/needs_reauth，交由下一次真实流量或主动
// 探测(/quota)刷新。
const grokNegativeSnapshotFreshWindow = 15 * time.Minute

type GrokQuotaFetcher struct{}

func NewGrokQuotaFetcher() *GrokQuotaFetcher {
	return &GrokQuotaFetcher{}
}

func (f *GrokQuotaFetcher) BuildUsageInfo(account *Account) *UsageInfo {
	now := time.Now()
	usage := &UsageInfo{
		Source:    "passive",
		UpdatedAt: &now,
	}
	if account == nil {
		usage.ErrorCode = "quota_unknown"
		usage.Error = "Grok quota is unknown until the first upstream response includes xAI rate-limit headers"
		return usage
	}

	snapshot, err := grokQuotaSnapshotFromExtra(account.Extra)
	if err != nil || snapshot == nil {
		usage.ErrorCode = "quota_unknown"
		usage.Error = "Grok quota is unknown until the first upstream response includes xAI rate-limit headers"
		return usage
	}

	if parsedAt, err := time.Parse(time.RFC3339, snapshot.UpdatedAt); err == nil {
		usage.UpdatedAt = &parsedAt
	}
	usage.GrokRequestQuota = snapshot.Requests
	usage.GrokTokenQuota = snapshot.Tokens
	usage.GrokRetryAfterSeconds = snapshot.RetryAfterSeconds
	usage.SubscriptionTier = snapshot.SubscriptionTier
	usage.SubscriptionTierRaw = snapshot.SubscriptionTier
	usage.GrokEntitlementStatus = snapshot.EntitlementStatus
	usage.GrokLastQuotaProbeAt = snapshot.LastProbeAt
	usage.GrokLastHeadersSeenAt = snapshot.LastHeadersSeenAt
	usage.GrokLastStatusCode = snapshot.StatusCode
	if snapshot.HasObservedHeaders() {
		usage.GrokQuotaSnapshotState = "observed"
	} else {
		usage.GrokQuotaSnapshotState = "no_headers"
		usage.ErrorCode = "quota_unknown"
		usage.Error = "No xAI quota headers observed on the latest Grok probe"
	}

	negativeSnapshotFresh := grokSnapshotWithinWindow(snapshot, now, grokNegativeSnapshotFreshWindow)
	switch snapshot.StatusCode {
	case 401:
		if negativeSnapshotFresh {
			usage.NeedsReauth = true
			usage.ErrorCode = "unauthenticated"
		} else {
			// 陈旧 401：token 可能已刷新恢复，不再断言需要重新授权。
			usage.GrokQuotaSnapshotState = "stale"
		}
	case 403:
		if negativeSnapshotFresh {
			usage.IsForbidden = true
			usage.ForbiddenType = "forbidden"
			usage.ErrorCode = "forbidden"
			if usage.GrokEntitlementStatus == "" {
				usage.GrokEntitlementStatus = "forbidden"
			}
		} else {
			// 陈旧 403：xAI 侧可能已恢复放行，继续显示 forbidden 会误标实际可用的账号。
			usage.GrokQuotaSnapshotState = "stale"
		}
	case 429:
		if snapshot.FreeUsageExhausted {
			usage.ErrorCode = "free_usage_exhausted"
			if usage.GrokEntitlementStatus == "" {
				usage.GrokEntitlementStatus = "free_usage_exhausted"
			}
			if usage.Error == "" {
				usage.Error = "Grok free Build usage exhausted over rolling window"
			}
		} else {
			usage.ErrorCode = "rate_limited"
		}
	}
	if snapshot.FreeUsageExhausted {
		usage.ErrorCode = "free_usage_exhausted"
		if usage.GrokEntitlementStatus == "" {
			usage.GrokEntitlementStatus = "free_usage_exhausted"
		}
	}
	return usage
}

// grokSnapshotWithinWindow 报告快照的观测时间是否落在 now 之前的 window 窗口内。
// 无有效时间戳时保守返回 false（视为陈旧），避免解析失败反而让旧负面状态一直生效。
func grokSnapshotWithinWindow(snapshot *xai.QuotaSnapshot, now time.Time, window time.Duration) bool {
	if snapshot == nil {
		return false
	}
	ts := strings.TrimSpace(snapshot.UpdatedAt)
	if ts == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return !parsed.After(now) && now.Sub(parsed) <= window
}

func grokQuotaSnapshotFromExtra(extra map[string]any) (*xai.QuotaSnapshot, error) {
	if extra == nil {
		return nil, nil
	}
	raw, ok := extra[grokQuotaSnapshotExtraKey]
	if !ok || raw == nil {
		return nil, nil
	}
	switch snapshot := raw.(type) {
	case *xai.QuotaSnapshot:
		return snapshot, nil
	case xai.QuotaSnapshot:
		return &snapshot, nil
	case map[string]any:
		data, err := json.Marshal(snapshot)
		if err != nil {
			return nil, err
		}
		var out xai.QuotaSnapshot
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return &out, nil
	default:
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("marshal grok quota snapshot: %w", err)
		}
		var out xai.QuotaSnapshot
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
}
