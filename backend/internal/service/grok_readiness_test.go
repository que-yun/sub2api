//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGrokAccountRequiresSuccessBeforeSchedule(t *testing.T) {
	acc := &Account{
		Platform:    PlatformGrok,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			grokHoldUntilSuccessExtraKey: true,
		},
	}
	require.True(t, GrokAccountRequiresSuccessBeforeSchedule(acc))
	require.False(t, acc.IsSchedulable(), "sticky hold must keep account out of scheduling")
}

func TestApplyGrokProbeOrTestStatusForbiddenMarksErrorAndStickyHold(t *testing.T) {
	repo := &grokQuotaAccountRepo{}
	acc := &Account{
		ID:          1001,
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{},
	}
	ApplyGrokProbeOrTestStatus(
		context.Background(),
		repo,
		nil,
		acc,
		http.StatusForbidden,
		http.Header{},
		[]byte(`{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`),
		"active_probe",
	)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Equal(t, acc.ID, repo.lastErrorID)
	require.Equal(t, grokHoldUntilSuccessReason, repo.lastErrorMsg)
	require.Equal(t, 0, repo.tempUnschedCalls)
	require.Equal(t, StatusError, acc.Status)
	require.False(t, acc.Schedulable)
	require.Equal(t, grokHoldUntilSuccessReason, acc.ErrorMessage)
	require.True(t, GrokAccountRequiresSuccessBeforeSchedule(acc))
	require.Nil(t, acc.TempUnschedulableUntil)
	require.Equal(t, "", acc.TempUnschedulableReason)
	require.Equal(t, true, repo.updates[acc.ID][grokHoldUntilSuccessExtraKey])
}

func TestApplyGrokProbeOrTestStatusSuccessClearsStickyHold(t *testing.T) {
	repo := &grokQuotaAccountRepo{}
	until := time.Now().Add(2 * time.Hour)
	acc := &Account{
		ID:                      1002,
		Platform:                PlatformGrok,
		Type:                    AccountTypeOAuth,
		Status:                  StatusActive,
		Schedulable:             true,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: grokHoldUntilSuccessReason,
		Extra: map[string]any{
			grokHoldUntilSuccessExtraKey: true,
		},
	}
	ApplyGrokProbeOrTestStatus(context.Background(), repo, nil, acc, http.StatusOK, nil, nil, "account_test")
	require.Equal(t, false, acc.Extra[grokHoldUntilSuccessExtraKey])
	require.Nil(t, acc.TempUnschedulableUntil)
	require.Equal(t, "", acc.TempUnschedulableReason)
	require.False(t, GrokAccountRequiresSuccessBeforeSchedule(acc))
	require.True(t, acc.IsSchedulable())
}

func TestApplyGrokProbeOrTestStatusSuccessRecoversErrorAccount(t *testing.T) {
	repo := &grokQuotaAccountRepo{}
	acc := &Account{
		ID:           1003,
		Platform:     PlatformGrok,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		Schedulable:  false,
		ErrorMessage: grokHoldUntilSuccessReason,
		Extra: map[string]any{
			grokHoldUntilSuccessExtraKey: true,
		},
	}
	ApplyGrokProbeOrTestStatus(context.Background(), repo, nil, acc, http.StatusOK, nil, nil, "account_test")
	require.Equal(t, StatusActive, acc.Status)
	require.True(t, acc.Schedulable)
	require.Equal(t, "", acc.ErrorMessage)
	require.False(t, GrokAccountRequiresSuccessBeforeSchedule(acc))
	require.Equal(t, 1, repo.clearErrorCalls)
	require.Equal(t, acc.ID, repo.lastClearErrorID)
	require.Equal(t, 1, repo.setSchedulableCalls)
	require.Equal(t, acc.ID, repo.lastSetSchedulableID)
	require.True(t, repo.lastSetSchedulable)
}

func TestIsGrokPermissionDeniedBody(t *testing.T) {
	require.True(t, IsGrokPermissionDeniedBody([]byte(`{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`)))
	require.False(t, IsGrokPermissionDeniedBody([]byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage"}`)))
	require.False(t, IsGrokPermissionDeniedBody(nil))
}

func TestMarkGrokTokenAcquisitionFailurePermanent(t *testing.T) {
	repo := &grokQuotaAccountRepo{}
	acc := &Account{ID: 2001, Platform: PlatformGrok, Status: StatusActive, Schedulable: true}
	MarkGrokTokenAcquisitionFailure(context.Background(), repo, acc, errors.New("oauth refresh account state changed"))
	require.Equal(t, 1, repo.setErrorCalls)
	require.Equal(t, StatusError, acc.Status)
	require.False(t, acc.Schedulable)
	require.Contains(t, acc.ErrorMessage, "token reauth required")
}

func TestMarkGrokTokenAcquisitionFailureTransient(t *testing.T) {
	repo := &grokQuotaAccountRepo{}
	acc := &Account{ID: 2002, Platform: PlatformGrok, Status: StatusActive, Schedulable: true}
	MarkGrokTokenAcquisitionFailure(context.Background(), repo, acc, errors.New("temporary network timeout"))
	require.Equal(t, 0, repo.setErrorCalls)
	require.Equal(t, 1, repo.tempUnschedCalls)
	require.Equal(t, StatusActive, acc.Status)
	require.True(t, acc.Schedulable)
	require.NotNil(t, acc.TempUnschedulableUntil)
}

