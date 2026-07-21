//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type reauthRecoverAccountRepo struct {
	mockAccountRepoForPlatform
	account                 *Account
	updateCalls             int
	clearTempUnschedCalls   int
	lastUpdatedSchedulable  bool
	lastUpdatedStatus       string
	lastUpdatedErrorMessage string
}

func (r *reauthRecoverAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account == nil || r.account.ID != id {
		return nil, ErrAccountNotFound
	}
	// return a shallow copy so callers can mutate without surprising later reads
	cp := *r.account
	if r.account.Credentials != nil {
		cp.Credentials = map[string]any{}
		for k, v := range r.account.Credentials {
			cp.Credentials[k] = v
		}
	}
	return &cp, nil
}

func (r *reauthRecoverAccountRepo) Update(_ context.Context, account *Account) error {
	r.updateCalls++
	r.lastUpdatedSchedulable = account.Schedulable
	r.lastUpdatedStatus = account.Status
	r.lastUpdatedErrorMessage = account.ErrorMessage
	cp := *account
	if account.Credentials != nil {
		cp.Credentials = map[string]any{}
		for k, v := range account.Credentials {
			cp.Credentials[k] = v
		}
	}
	r.account = &cp
	return nil
}

func (r *reauthRecoverAccountRepo) ClearTempUnschedulable(_ context.Context, id int64) error {
	r.clearTempUnschedCalls++
	if r.account != nil && r.account.ID == id {
		r.account.TempUnschedulableUntil = nil
		r.account.TempUnschedulableReason = ""
	}
	return nil
}

func TestCredentialsLookLikeAuthRefresh(t *testing.T) {
	require.False(t, credentialsLookLikeAuthRefresh(nil))
	require.False(t, credentialsLookLikeAuthRefresh(map[string]any{"model_mapping": map[string]any{"a": "b"}}))
	require.True(t, credentialsLookLikeAuthRefresh(map[string]any{"access_token": "at"}))
	require.True(t, credentialsLookLikeAuthRefresh(map[string]any{"refresh_token": "rt"}))
	require.True(t, credentialsLookLikeAuthRefresh(map[string]any{"api_key": "sk"}))
}

func TestUpdateAccountReauthRestoresSchedulable(t *testing.T) {
	until := time.Now().Add(30 * time.Minute)
	repo := &reauthRecoverAccountRepo{
		account: &Account{
			ID:                      2169,
			Name:                    "a18706782271nat6vw@2925.com",
			Platform:                PlatformOpenAI,
			Type:                    AccountTypeOAuth,
			Status:                  StatusActive,
			Schedulable:             false,
			ErrorMessage:            "Authentication failed (401)",
			TempUnschedulableUntil:  &until,
			TempUnschedulableReason: "token refresh failed",
			Credentials: map[string]any{
				"refresh_token": "old-rt",
			},
		},
	}
	blocker := &runtimeBlockRecorder{}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: blocker}

	updated, err := svc.UpdateAccount(context.Background(), 2169, &UpdateAccountInput{
		Status: StatusActive,
		Credentials: map[string]any{
			"access_token":  "new-at",
			"refresh_token": "new-rt",
			"email":         "a18706782271nat6vw@2925.com",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 1, repo.updateCalls)
	require.True(t, repo.lastUpdatedSchedulable)
	require.Equal(t, StatusActive, repo.lastUpdatedStatus)
	require.Equal(t, "", repo.lastUpdatedErrorMessage)
	require.Equal(t, 1, repo.clearTempUnschedCalls)
	require.Equal(t, []int64{2169}, blocker.clearedIDs)
	require.True(t, updated.Schedulable)
	require.Equal(t, StatusActive, updated.Status)
}

func TestUpdateAccountWithoutAuthCredentialsDoesNotForceSchedulable(t *testing.T) {
	repo := &reauthRecoverAccountRepo{
		account: &Account{
			ID:          77,
			Name:        "manual-hold",
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: false,
			Credentials: map[string]any{"refresh_token": "rt"},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.UpdateAccount(context.Background(), 77, &UpdateAccountInput{
		Priority: reauthIntPtr(5),
	})
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateCalls)
	require.False(t, repo.lastUpdatedSchedulable)
	require.Zero(t, repo.clearTempUnschedCalls)
}

func reauthIntPtr(v int) *int { return &v }
