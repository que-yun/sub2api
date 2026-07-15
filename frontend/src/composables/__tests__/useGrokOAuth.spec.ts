import { describe, expect, it, vi } from 'vitest'

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn()
  })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => {
      const messages: Record<string, string> = {
        'admin.accounts.oauth.grok.failedToExchangeCode': 'Grok 授权码兑换失败',
        'admin.accounts.oauth.grok.errors.GROK_OAUTH_INVALID_STATE':
          'Grok OAuth state 与当前会话不匹配。请粘贴同一次生成的授权链接返回的回调 URL。'
      }
      return messages[key] ?? key
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    grok: {
      generateAuthUrl: vi.fn(),
      exchangeCode: vi.fn(),
      refreshGrokToken: vi.fn()
    }
  }
}))

import { useGrokOAuth } from '@/composables/useGrokOAuth'
import { adminAPI } from '@/api/admin'

describe('useGrokOAuth.exchangeAuthCode', () => {
  it('shows a state mismatch recovery hint from structured backend errors', async () => {
    vi.mocked(adminAPI.grok.exchangeCode).mockRejectedValueOnce({
      status: 400,
      reason: 'GROK_OAUTH_INVALID_STATE',
      message: 'invalid oauth state'
    })
    const oauth = useGrokOAuth()

    const tokenInfo = await oauth.exchangeAuthCode({
      code: 'code',
      sessionId: 'session-id',
      state: 'wrong-state'
    })

    expect(tokenInfo).toBeNull()
    expect(oauth.error.value).toBe(
      'Grok OAuth state 与当前会话不匹配。请粘贴同一次生成的授权链接返回的回调 URL。'
    )
  })
})

describe('useGrokOAuth.buildCredentials', () => {
  it('preserves Grok referrer and OAuth token response extras for account persistence', () => {
    const oauth = useGrokOAuth()

    const credentials = oauth.buildCredentials({
      access_token: 'access-token',
      refresh_token: 'refresh-token',
      token_type: 'Bearer',
      expires_at: 123456,
      client_id: 'client-id',
      scope: 'openid grok-cli:access',
      referrer: 'grok-build',
      oauth_token_response_extra: { team_id: 'team-123' },
      oauth_token_response_extra_keys: ['team_id'],
      oauth_token_response_summary: {
        referrer: 'grok-build',
        extra_keys: ['team_id']
      }
    })

    expect(credentials.referrer).toBe('grok-build')
    expect(credentials.oauth_token_response_extra).toEqual({ team_id: 'team-123' })
    expect(credentials.oauth_token_response_extra_keys).toEqual(['team_id'])
    expect(credentials.oauth_token_response_summary).toEqual({
      referrer: 'grok-build',
      extra_keys: ['team_id']
    })
  })
})
