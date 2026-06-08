package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/failsafebreaker"
	"github.com/stretchr/testify/require"
)

func TestRouteOpenAIChatCompletionsFailsafe_FlagOffNoops(t *testing.T) {
	t.Setenv("GATEWAY_FAILSAFE_ROUTING", "")
	resetFailsafeObserveRegistryForTest(t)
	groupID := openAIFailsafeRoutingGroupID
	svc := &OpenAIGatewayService{}

	accountID, _, ok := svc.RouteOpenAIChatCompletionsFailsafe(context.Background(), &groupID, "", "gpt-5.1", func(account *Account, selection *AccountSelectionResult) (int, []byte, error) {
		t.Fatal("call should not run when failsafe routing is disabled")
		return http.StatusOK, nil, nil
	})

	require.False(t, ok)
	require.Zero(t, accountID)
}

func TestRouteOpenAIChatCompletionsFailsafe_NonTargetGroupNoops(t *testing.T) {
	t.Setenv("GATEWAY_FAILSAFE_ROUTING", "1")
	resetFailsafeObserveRegistryForTest(t)
	groupID := openAIFailsafeRoutingGroupID + 1
	svc := &OpenAIGatewayService{}

	accountID, _, ok := svc.RouteOpenAIChatCompletionsFailsafe(context.Background(), &groupID, "", "gpt-5.1", func(account *Account, selection *AccountSelectionResult) (int, []byte, error) {
		t.Fatal("call should not run for non-target group")
		return http.StatusOK, nil, nil
	})

	require.False(t, ok)
	require.Zero(t, accountID)
}

func TestRouteOpenAIChatCompletionsFailsafe_TargetGroupRoutesUntilSuccess(t *testing.T) {
	t.Setenv("GATEWAY_FAILSAFE_ROUTING", "1")
	resetFailsafeObserveRegistryForTest(t)
	groupID := openAIFailsafeRoutingGroupID
	accounts := []Account{
		{ID: 71001, Name: "bad", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true},
		{ID: 71002, Name: "good", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true},
	}
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
	}
	calls := make([]int64, 0, len(accounts))

	accountID, res, ok := svc.RouteOpenAIChatCompletionsFailsafe(context.Background(), &groupID, "", "", func(account *Account, selection *AccountSelectionResult) (int, []byte, error) {
		require.NotNil(t, selection)
		require.NotNil(t, selection.ReleaseFunc)
		calls = append(calls, account.ID)
		if account.ID == 71001 {
			return http.StatusBadGateway, []byte(`{"error":{"message":"bad gateway"}}`), &UpstreamFailoverError{StatusCode: http.StatusBadGateway, ResponseBody: []byte(`{"error":{"message":"bad gateway"}}`)}
		}
		return http.StatusOK, nil, nil
	})

	require.True(t, ok)
	require.Equal(t, int64(71002), accountID)
	require.Equal(t, []int64{71001, 71002}, calls)
	require.Equal(t, failsafebreaker.TierDegraded, failsafeObserveReg.Tier(71001))
	require.Equal(t, "success", res.Category.String())
}
