package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	grokTokenCacheSkew          = 5 * time.Minute
	grokRequestRefreshTimeout   = 8 * time.Second
	grokRefreshLockWaitTimeout  = 2 * time.Second
	grokRefreshLockPollInterval = 25 * time.Millisecond
)

var (
	errGrokOAuthRefreshNotConfigured = errors.New("grok oauth refresh is not configured")
	errGrokOAuthRefreshTokenMissing  = errors.New("grok oauth refresh token is missing")
	errGrokOAuthAccessTokenMissing   = errors.New("grok oauth access token is missing")
	errGrokOAuthAccessTokenExpired   = errors.New("grok oauth access token is expired")
	errGrokOAuthConfiguredProxyMiss  = errors.New("grok oauth configured proxy is missing")
)

type GrokTokenCache = GeminiTokenCache

type GrokTokenProvider struct {
	accountRepo       AccountRepository
	tokenCache        GrokTokenCache
	refreshAPI        *OAuthRefreshAPI
	executor          OAuthRefreshExecutor
	refreshPolicy     ProviderRefreshPolicy
	tempUnschedCache  TempUnschedCache
	requestRefreshOff bool // true: 请求路径不刷新，等待本机同步
}

func NewGrokTokenProvider(
	accountRepo AccountRepository,
	tokenCache GrokTokenCache,
) *GrokTokenProvider {
	return &GrokTokenProvider{
		accountRepo:   accountRepo,
		tokenCache:    tokenCache,
		refreshPolicy: GrokProviderRefreshPolicy(),
	}
}

func (p *GrokTokenProvider) SetRefreshAPI(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	p.refreshAPI = api
	p.executor = executor
}

func (p *GrokTokenProvider) SetRefreshPolicy(policy ProviderRefreshPolicy) {
	p.refreshPolicy = policy
}

func (p *GrokTokenProvider) SetTempUnschedCache(cache TempUnschedCache) {
	p.tempUnschedCache = cache
}

// SetRequestRefreshEnabled controls refresh-token use from the request path.
// Disable on VPS standby so only the local authority mints/rotates Grok OAuth tokens.
func (p *GrokTokenProvider) SetRequestRefreshEnabled(enabled bool) {
	if p == nil {
		return
	}
	p.requestRefreshOff = !enabled
}

func (p *GrokTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformGrok || account.Type != AccountTypeOAuth {
		return "", errors.New("not a grok oauth account")
	}
	selectedProxyID := cloneGrokProxyID(account.ProxyID)
	if eligibilityErr := grokOAuthRequestAccountEligibilityError(account); eligibilityErr != nil {
		return "", withGrokCredentialFailureSnapshot(eligibilityErr, account)
	}

	expiresAt := account.GetCredentialAsTime("expires_at")
	accountAccessToken := strings.TrimSpace(account.GetGrokAccessToken())
	if accountAccessToken == "" {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenMissing, account)
	}
	if strings.TrimSpace(account.GetGrokRefreshToken()) == "" {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthRefreshTokenMissing, account)
	}
	cacheKey := GrokTokenCacheKey(account)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil {
			cachedToken := strings.TrimSpace(token)
			if cachedToken != "" && accountAccessToken != "" && cachedToken == accountAccessToken &&
				expiresAt != nil && time.Until(*expiresAt) > grokTokenRefreshSkew {
				return cachedToken, nil
			}
		}
	}

	needsRefresh := expiresAt == nil || time.Until(*expiresAt) <= grokTokenRefreshSkew
	if needsRefresh && p.requestRefreshOff {
		// VPS / 只读节点：不调用 xAI token endpoint，沿用库内 access_token。
		// 若已过期则返回 expired，由 failure 分类做临时冷却，等本机同步新凭证。
		if expiresAt == nil || !time.Now().Before(*expiresAt) {
			return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenExpired, account)
		}
		// 仍在 skew 窗口内：继续用现有 access_token，不刷新。
	} else if needsRefresh {
		if p.refreshAPI == nil || p.executor == nil {
			return "", errGrokOAuthRefreshNotConfigured
		}
		refreshCtx, cancel := context.WithTimeout(ctx, grokRequestRefreshTimeout)
		defer cancel()
		result, err := p.refreshAPI.RefreshIfNeeded(withOAuthRefreshRequestPath(refreshCtx), account, p.executor, grokTokenRefreshSkew)
		if err != nil {
			if p.refreshPolicy.OnRefreshError == ProviderRefreshErrorReturn {
				return "", err
			}
		} else if result != nil && result.LockHeld {
			if p.refreshPolicy.OnLockHeld == ProviderLockHeldWaitForCache {
				token, waitErr := p.waitForRefreshedToken(refreshCtx, account, cacheKey)
				return token, withGrokCredentialFailureSnapshot(waitErr, account)
			}
			if expiresAt == nil || !time.Now().Before(*expiresAt) {
				return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenExpired, account)
			}
		} else if result != nil && result.Account != nil {
			if eligibilityErr := grokOAuthRequestAccountEligibilityError(result.Account); eligibilityErr != nil {
				return "", withGrokCredentialFailureSnapshot(eligibilityErr, result.Account)
			}
			if !grokCredentialProxyIDsEqual(result.Account.ProxyID, selectedProxyID) {
				return "", withGrokCredentialFailureSnapshot(errOAuthRefreshAccountStateChanged, result.Account)
			}
			account = result.Account
			expiresAt = account.GetCredentialAsTime("expires_at")
		}
	}

	accessToken := account.GetGrokAccessToken()
	if strings.TrimSpace(accessToken) == "" {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenMissing, account)
	}
	if expiresAt != nil && !time.Now().Before(*expiresAt) {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenExpired, account)
	}

	if p.tokenCache != nil {
		latestAccount, isStale := CheckTokenVersion(ctx, account, p.accountRepo)
		if isStale && latestAccount != nil {
			if eligibilityErr := grokOAuthRequestAccountEligibilityError(latestAccount); eligibilityErr != nil {
				return "", withGrokCredentialFailureSnapshot(eligibilityErr, latestAccount)
			}
			if !grokCredentialProxyIDsEqual(latestAccount.ProxyID, selectedProxyID) {
				return "", withGrokCredentialFailureSnapshot(errOAuthRefreshAccountStateChanged, latestAccount)
			}
			accessToken = latestAccount.GetGrokAccessToken()
			if strings.TrimSpace(accessToken) == "" {
				return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenMissing, latestAccount)
			}
			latestExpiry := latestAccount.GetCredentialAsTime("expires_at")
			if latestExpiry == nil || !time.Now().Before(*latestExpiry) {
				return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenExpired, latestAccount)
			}
		} else {
			ttl := 30 * time.Minute
			if expiresAt != nil {
				until := time.Until(*expiresAt)
				switch {
				case until > grokTokenCacheSkew:
					ttl = until - grokTokenCacheSkew
				case until > 0:
					ttl = until
				default:
					ttl = time.Minute
				}
			}
			_ = p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl)
		}
	}

	return accessToken, nil
}

// GetAccessTokenForOperatorProbe acquires a usable access token for admin/recovery probes.
// Unlike GetAccessToken, it can refresh sticky error accounts that are not schedulable.
// Local recovery path: allows inactive accounts, supports requestRefreshOff (VPS read-only),
// and waits for in-flight refresh locks instead of failing closed.
func (p *GrokTokenProvider) GetAccessTokenForOperatorProbe(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformGrok || account.Type != AccountTypeOAuth {
		return "", errors.New("not a grok oauth account")
	}
	if account.ProxyID != nil && account.Proxy == nil {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthConfiguredProxyMiss, account)
	}
	if strings.TrimSpace(account.GetGrokRefreshToken()) == "" && strings.TrimSpace(account.GetGrokAccessToken()) == "" {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthRefreshTokenMissing, account)
	}

	// Prefer still-valid access token without forcing active/schedulable eligibility.
	expiresAt := account.GetCredentialAsTime("expires_at")
	accessToken := strings.TrimSpace(account.GetGrokAccessToken())
	if accessToken != "" && expiresAt != nil && time.Until(*expiresAt) > grokTokenRefreshSkew {
		return accessToken, nil
	}
	if accessToken != "" && expiresAt != nil && time.Now().Before(*expiresAt) && strings.TrimSpace(account.GetGrokRefreshToken()) == "" {
		return accessToken, nil
	}
	if p == nil || p.refreshAPI == nil || p.executor == nil || p.requestRefreshOff {
		// requestRefreshOff: 管理探测也不在 VPS 上 mint/rotate RT，只读库内 access。
		if accessToken != "" && expiresAt != nil && time.Now().Before(*expiresAt) {
			return accessToken, nil
		}
		if p != nil && p.requestRefreshOff {
			return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenExpired, account)
		}
		return "", errGrokOAuthRefreshNotConfigured
	}
	if strings.TrimSpace(account.GetGrokRefreshToken()) == "" {
		if accessToken != "" && expiresAt != nil && time.Now().Before(*expiresAt) {
			return accessToken, nil
		}
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthRefreshTokenMissing, account)
	}

	refreshCtx, cancel := context.WithTimeout(ctx, grokRequestRefreshTimeout)
	defer cancel()
	refreshCtx = withOAuthRefreshAllowInactive(refreshCtx)
	// Keep request-path locking/race recovery, but allow inactive Grok recovery accounts.
	refreshCtx = withOAuthRefreshRequestPath(refreshCtx)
	result, err := p.refreshAPI.RefreshIfNeeded(refreshCtx, account, p.executor, grokTokenRefreshSkew)
	if err != nil {
		return "", err
	}
	if result != nil && result.LockHeld {
		token, waitErr := p.waitForRefreshedToken(refreshCtx, account, GrokTokenCacheKey(account))
		if waitErr == nil && strings.TrimSpace(token) != "" {
			return token, nil
		}
		// Fall through to whatever credential we still have.
	}
	if result != nil && result.Account != nil {
		account = result.Account
	}
	accessToken = strings.TrimSpace(account.GetGrokAccessToken())
	if accessToken == "" {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenMissing, account)
	}
	expiresAt = account.GetCredentialAsTime("expires_at")
	if expiresAt != nil && !time.Now().Before(*expiresAt) {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenExpired, account)
	}
	return accessToken, nil
}

// GetAccessTokenForManualTest returns an access token for an admin-initiated
// "test connection" probe. Unlike GetAccessToken it does not apply the
// request-path scheduling eligibility gate (manual Schedulable switch,
// rate-limit / overload / temp-unschedulable cooldowns): a manual test exists
// precisely to check accounts in those states, matching how Codex/OpenAI
// account tests read credentials regardless of scheduling state (#4598).
//
// Credential integrity still applies: the configured-proxy-missing check, the
// shared refresh lock protocol, and the refresh API's own account re-read.
// Credential rotation for non-active (disabled/error) accounts remains
// blocked inside RefreshIfNeeded; their still-valid tokens are probed as-is.
//
// Local hardening retained: requestRefreshOff read-only path, access-token-only
// accounts, credential failure snapshots, and lock-wait recovery.
func (p *GrokTokenProvider) GetAccessTokenForManualTest(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformGrok || account.Type != AccountTypeOAuth {
		return "", errors.New("not a grok oauth account")
	}
	if account.ProxyID != nil && account.Proxy == nil {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthConfiguredProxyMiss, account)
	}
	if strings.TrimSpace(account.GetGrokRefreshToken()) == "" && strings.TrimSpace(account.GetGrokAccessToken()) == "" {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthRefreshTokenMissing, account)
	}

	accessToken := strings.TrimSpace(account.GetGrokAccessToken())
	expiresAt := account.GetCredentialAsTime("expires_at")
	tokenValid := accessToken != "" && expiresAt != nil && time.Now().Before(*expiresAt)
	if accessToken != "" && expiresAt != nil && time.Until(*expiresAt) > grokTokenRefreshSkew {
		return accessToken, nil
	}
	if accessToken != "" && expiresAt != nil && time.Now().Before(*expiresAt) && strings.TrimSpace(account.GetGrokRefreshToken()) == "" {
		return accessToken, nil
	}

	if p == nil || p.refreshAPI == nil || p.executor == nil || p.requestRefreshOff {
		if tokenValid {
			return accessToken, nil
		}
		if p != nil && p.requestRefreshOff {
			return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenExpired, account)
		}
		return "", errGrokOAuthRefreshNotConfigured
	}
	if strings.TrimSpace(account.GetGrokRefreshToken()) == "" {
		if tokenValid {
			return accessToken, nil
		}
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthRefreshTokenMissing, account)
	}

	// Deliberately not marked as a request-path refresh: the request path
	// re-applies scheduling eligibility inside RefreshIfNeeded, which is
	// exactly what a manual test must bypass. Local recovery still allows
	// inactive accounts so admin tests can probe sticky error rows.
	refreshCtx, cancel := context.WithTimeout(ctx, grokRequestRefreshTimeout)
	defer cancel()
	refreshCtx = withOAuthRefreshAllowInactive(refreshCtx)
	result, err := p.refreshAPI.RefreshIfNeeded(refreshCtx, account, p.executor, grokTokenRefreshSkew)
	if err != nil {
		if tokenValid {
			return accessToken, nil
		}
		return "", err
	}
	if result != nil && result.LockHeld {
		if tokenValid {
			return accessToken, nil
		}
		token, waitErr := p.waitForRefreshedToken(refreshCtx, account, GrokTokenCacheKey(account))
		if waitErr == nil && strings.TrimSpace(token) != "" {
			return token, nil
		}
		return "", errors.New("token refresh is already in progress on another worker; retry in a few seconds")
	}
	if result != nil && result.Account != nil {
		account = result.Account
	}

	accessToken = strings.TrimSpace(account.GetGrokAccessToken())
	if accessToken == "" {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenMissing, account)
	}
	if latestExpiry := account.GetCredentialAsTime("expires_at"); latestExpiry != nil && !time.Now().Before(*latestExpiry) {
		return "", withGrokCredentialFailureSnapshot(errGrokOAuthAccessTokenExpired, account)
	}
	return accessToken, nil
}

func (p *GrokTokenProvider) waitForRefreshedToken(ctx context.Context, account *Account, cacheKey string) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, grokRefreshLockWaitTimeout)
	defer cancel()

	initialToken := strings.TrimSpace(account.GetGrokAccessToken())
	initialVersion := account.GetCredentialAsInt64("_token_version")
	selectedProxyID := cloneGrokProxyID(account.ProxyID)
	sawAuthoritativeState := false
	var lastAccountReadErr error
	ticker := time.NewTicker(grokRefreshLockPollInterval)
	defer ticker.Stop()

	for {
		cachedToken := ""
		if p.tokenCache != nil {
			if token, err := p.tokenCache.GetAccessToken(waitCtx, cacheKey); err == nil {
				cachedToken = strings.TrimSpace(token)
			}
		}

		if p.accountRepo != nil {
			latest, err := p.accountRepo.GetByID(waitCtx, account.ID)
			if err != nil {
				lastAccountReadErr = err
			} else if latest == nil {
				return "", errOAuthRefreshAccountStateChanged
			} else {
				sawAuthoritativeState = true
				allowInactive := isOAuthRefreshAllowInactive(waitCtx) && latest.IsGrokOAuth()
				if !allowInactive {
					if eligibilityErr := grokOAuthRequestAccountEligibilityError(latest); eligibilityErr != nil {
						return "", withGrokCredentialFailureSnapshot(eligibilityErr, latest)
					}
				}
				if !grokCredentialProxyIDsEqual(latest.ProxyID, selectedProxyID) {
					return "", withGrokCredentialFailureSnapshot(errOAuthRefreshAccountStateChanged, latest)
				}
				token := strings.TrimSpace(latest.GetGrokAccessToken())
				version := latest.GetCredentialAsInt64("_token_version")
				expiresAt := latest.GetCredentialAsTime("expires_at")
				changed := token != initialToken || (version > 0 && version > initialVersion)
				valid := expiresAt != nil && time.Now().Before(*expiresAt)
				if token != "" && changed && valid {
					// The versioned DB credential is authoritative. A stale cache must
					// not hold the request on the old expired token; repair it best-effort.
					if cachedToken != "" && cachedToken != token {
						ttl := time.Until(*expiresAt)
						if ttl > grokTokenCacheSkew {
							ttl -= grokTokenCacheSkew
						}
						_ = p.tokenCache.SetAccessToken(waitCtx, cacheKey, token, ttl)
					}
					return token, nil
				}
			}
		}

		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if !sawAuthoritativeState {
				if lastAccountReadErr == nil {
					lastAccountReadErr = waitCtx.Err()
				}
				return "", fmt.Errorf("%w: %v", errOAuthRefreshAccountRereadFailed, lastAccountReadErr)
			}
			// Another worker still owns the refresh and the authoritative row is
			// unchanged. Do not quarantine the old credential: its refresh may
			// commit immediately after this bounded wait.
			return "", errOAuthRefreshAccountStateChanged
		case <-ticker.C:
		}
	}
}

func grokOAuthRequestAccountEligibilityError(account *Account) error {
	if account == nil || !account.IsGrokOAuth() || !account.IsSchedulable() {
		return errOAuthRefreshAccountStateChanged
	}
	if account.ProxyID != nil && account.Proxy == nil {
		return errGrokOAuthConfiguredProxyMiss
	}
	return nil
}

func cloneGrokProxyID(proxyID *int64) *int64 {
	if proxyID == nil {
		return nil
	}
	value := *proxyID
	return &value
}

func (p *GrokTokenProvider) InvalidateToken(ctx context.Context, account *Account) error {
	if p == nil || p.tokenCache == nil || account == nil {
		return nil
	}
	return p.tokenCache.DeleteAccessToken(ctx, GrokTokenCacheKey(account))
}

func GrokTokenCacheKey(account *Account) string {
	if account == nil {
		return "grok:account:0"
	}
	return "grok:account:" + strconv.FormatInt(account.ID, 10)
}
