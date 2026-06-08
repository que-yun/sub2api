package service

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/failclass"
	"github.com/Wei-Shaw/sub2api/internal/pkg/failsafebreaker"
)

// failsafe-go 接入 Stage 1+2a:observe-only。
// flag GATEWAY_FAILSAFE_ROUTING 默认关 -> 全部 no-op,行为零改动。
// 打开后仅对上游错误/成功做分类+记账+日志(不改路由),用于灰度验证 Tier 是否准确。
var failsafeObserveReg = failsafebreaker.NewRegistry(failsafebreaker.DefaultConfig())

func failsafeRoutingEnabled() bool {
	switch os.Getenv("GATEWAY_FAILSAFE_ROUTING") {
	case "1", "true", "shadow", "on":
		return true
	}
	return false
}

// observeFailsafeUpstreamError 由 HandleUpstreamError 调用(observe-only)。
func observeFailsafeUpstreamError(account *Account, statusCode int, headers http.Header, responseBody []byte, requestedModel []string) {
	if account == nil || !failsafeRoutingEnabled() {
		return
	}
	var resetAfter time.Duration
	if rt := calculateOpenAI429ResetTime(headers); rt != nil {
		if d := time.Until(*rt); d > 0 {
			resetAfter = d
		}
	}
	model := ""
	if len(requestedModel) > 0 {
		model = requestedModel[0]
	}
	res := failclass.Classify(statusCode, nil, responseBody, resetAfter)
	failsafeObserveReg.Record(account.ID, res)
	slog.Info("failsafe.observe",
		"event", "error",
		"account_id", account.ID,
		"model", model,
		"status", statusCode,
		"category", res.Category.String(),
		"tier", failsafeObserveReg.Tier(account.ID).String(),
		"penalize_hard", res.PenalizeHard,
		"reason", res.Reason,
	)
}

// observeFailsafeUpstreamSuccess 由成功路径调用(observe-only):记成功 -> 清掉不可用标记,Tier 恢复 Healthy。
func observeFailsafeUpstreamSuccess(account *Account) {
	if account == nil || !failsafeRoutingEnabled() {
		return
	}
	failsafeObserveReg.Record(account.ID, failclass.Result{Category: failclass.Success})
}
