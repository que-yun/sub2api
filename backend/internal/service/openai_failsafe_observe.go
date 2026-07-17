package service

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
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

// failsafeRoutingFull 仅在完全打开(非 shadow)时 true:控制是否真改路由;shadow 只 observe(记账+日志),不改路由。
func failsafeRoutingFull() bool {
	switch os.Getenv("GATEWAY_FAILSAFE_ROUTING") {
	case "1", "true", "on":
		return true
	}
	return false
}

// openAIFailsafeRoutingGroupExcluded 安全阀: env GATEWAY_FAILSAFE_ROUTING_EXCLUDE_GROUPS
// 逗号分隔组ID, 命中的组退回手写路由(任一组异常可秒排除, 不必全关 flag)。
func openAIFailsafeRoutingGroupExcluded(groupID int64) bool {
	raw := os.Getenv("GATEWAY_FAILSAFE_ROUTING_EXCLUDE_GROUPS")
	if raw == "" {
		return false
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.ParseInt(part, 10, 64); err == nil && n == groupID {
			return true
		}
	}
	return false
}

func OpenAIFailsafeRoutingEnabledForGroup(groupID *int64) bool {
	return failsafeRoutingFull() && groupID != nil && !openAIFailsafeRoutingGroupExcluded(*groupID)
}

type OpenAIFailsafeRouteCall func(account *Account, selection *AccountSelectionResult) (status int, body []byte, err error)

func (s *OpenAIGatewayService) RouteOpenAIChatCompletionsFailsafe(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	requestedModel string,
	call OpenAIFailsafeRouteCall,
) (int64, failclass.Result, bool) {
	if s == nil || call == nil || !OpenAIFailsafeRoutingEnabledForGroup(groupID) {
		return 0, failclass.Result{}, false
	}
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		return 0, failclass.Result{}, false
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID, PlatformOpenAI)
	if err != nil || len(accounts) == 0 {
		return 0, failclass.Result{}, false
	}

	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, groupID)
	byID := make(map[int64]Account, len(accounts))
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		if account.Platform != PlatformOpenAI {
			continue
		}
		if !accountSupportsOpenAICapabilities(&account, OpenAIEndpointCapabilityChatCompletions, "") {
			continue
		}
		if requestedModel != "" && !isOpenAICompatibleAccountEligibleForRequest(ctx, &account, PlatformOpenAI, requestedModel, false, OpenAIEndpointCapabilityChatCompletions) {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, &account, requestedModel, false) {
			continue
		}
		ids = append(ids, account.ID)
		byID[account.ID] = account
	}
	if len(ids) == 0 {
		return 0, failclass.Result{}, false
	}

	var stickyAccountID int64
	if sessionHash != "" && s.cache != nil {
		if accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash); err == nil && accountID > 0 {
			stickyAccountID = accountID
		}
	}

	return failsafeObserveReg.Route(ids, stickyAccountID, func(accountID int64) (int, []byte, error) {
		account, ok := byID[accountID]
		if !ok {
			return 0, []byte("failsafe route selected unknown account"), ErrNoAvailableAccounts
		}
		selection, err := s.selectionForOpenAIFailsafeRouteAccount(ctx, groupID, sessionHash, requestedModel, &account)
		if err != nil {
			return 0, []byte(err.Error()), err
		}
		return call(selection.Account, selection)
	})
}

func (s *OpenAIGatewayService) selectionForOpenAIFailsafeRouteAccount(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, account *Account) (*AccountSelectionResult, error) {
	if s == nil || account == nil {
		return nil, ErrNoAvailableAccounts
	}
	fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, account, PlatformOpenAI, requestedModel, false, OpenAIEndpointCapabilityChatCompletions)
	if fresh == nil {
		return nil, ErrNoAvailableAccounts
	}
	fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, groupID, PlatformOpenAI, requestedModel, false, OpenAIEndpointCapabilityChatCompletions)
	if fresh == nil || !accountSupportsOpenAICapabilities(fresh, OpenAIEndpointCapabilityChatCompletions, "") {
		return nil, ErrNoAvailableAccounts
	}

	cfg := s.schedulingConfig()
	accountConcurrency := fresh.Concurrency
	result, err := s.tryAcquireAccountSlot(ctx, fresh.ID, accountConcurrency)
	if err == nil && result != nil && result.Acquired {
		return s.newAcquiredSelectionResult(ctx, fresh, result.ReleaseFunc)
	}
	if sessionHash != "" {
		return s.newSelectionResult(ctx, fresh, false, nil, &AccountWaitPlan{
			AccountID:      fresh.ID,
			MaxConcurrency: accountConcurrency,
			Timeout:        cfg.StickySessionWaitTimeout,
			MaxWaiting:     cfg.StickySessionMaxWaiting,
		})
	}
	return s.newSelectionResult(ctx, fresh, false, nil, &AccountWaitPlan{
		AccountID:      fresh.ID,
		MaxConcurrency: accountConcurrency,
		Timeout:        cfg.FallbackWaitTimeout,
		MaxWaiting:     cfg.FallbackMaxWaiting,
	})
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
