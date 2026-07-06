// api.test.js — задачи M10-1/M10-6: токен в памяти, формат ошибок
// {"error","code"}, 401 → тихий refresh ровно с одним повтором.
import { afterEach, describe, expect, it } from 'vitest'
import * as auth from './auth.js'
import { apiFetch, ApiError, patchStage } from './api.js'
import { makeJWT, mockFetch } from '../test/helpers.js'

afterEach(() => auth.logout())

async function loginAs(role) {
  const token = makeJWT(role)
  mockFetch([
    { match: (u) => u === '/auth/login', reply: { body: { access_token: token, expires_in: 900 } } },
  ])
  await auth.login('m@x', 'pw')
  return token
}

describe('auth', () => {
  it('login кладёт токен в память и декодирует роль из JWT', async () => {
    await loginAs('admin')
    expect(auth.getClaims()).toMatchObject({ role: 'admin', sub: '1' })
    // Токен нигде, кроме памяти модуля: ни localStorage, ни document.cookie.
    expect(Object.keys(localStorage)).toHaveLength(0)
    expect(document.cookie).toBe('')
  })
})

describe('apiFetch', () => {
  it('шлёт Authorization и разбирает ошибку {"error","code"}', async () => {
    const token = await loginAs('manager')
    const fetch = mockFetch([
      {
        match: (u) => u.startsWith('/api/leads/7/stage'),
        reply: { status: 400, body: { error: 'переход запрещён', code: 'ERR_INVALID_TRANSITION' } },
      },
    ])
    const err = await patchStage(7, 3).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.code).toBe('ERR_INVALID_TRANSITION')
    expect(err.status).toBe(400)
    expect(fetch.calls[0].opts.headers.Authorization).toBe('Bearer ' + token)
    expect(JSON.parse(fetch.calls[0].opts.body)).toEqual({ stage_id: 3 })
  })

  it('401 → тихий refresh → повтор с новым токеном', async () => {
    const oldToken = await loginAs('manager')
    const newToken = makeJWT('manager', '1')
    let leadCalls = 0
    const fetch = mockFetch([
      {
        match: (u) => u.startsWith('/api/leads'),
        reply: () =>
          ++leadCalls === 1
            ? { status: 401, body: { error: 'токен истёк', code: 'ERR_TOKEN_EXPIRED' } }
            : { body: { leads: [], total: 0 } },
      },
      { match: (u) => u === '/auth/refresh', reply: { body: { access_token: newToken, expires_in: 900 } } },
    ])
    const data = await apiFetch('/api/leads?limit=200&offset=0')
    expect(data).toEqual({ leads: [], total: 0 })
    const authHeaders = fetch.calls.filter((c) => c.url.startsWith('/api/')).map((c) => c.opts.headers.Authorization)
    expect(authHeaders).toEqual(['Bearer ' + oldToken, 'Bearer ' + newToken])
  })

  it('refresh не помог → сессия сброшена, App уйдёт на login', async () => {
    await loginAs('manager')
    mockFetch([
      { match: (u) => u.startsWith('/api/'), reply: { status: 401, body: { error: 'x', code: 'ERR_TOKEN_EXPIRED' } } },
      {
        match: (u) => u === '/auth/refresh',
        reply: { status: 401, body: { error: 'refresh просрочен', code: 'ERR_REFRESH_EXPIRED' } },
      },
    ])
    const err = await apiFetch('/api/leads').catch((e) => e)
    expect(err.code).toBe('ERR_SESSION_EXPIRED')
    expect(auth.getToken()).toBeNull() // токен выброшен из памяти
  })

  it('пачка одновременных 401 жжёт ровно один refresh (ротация M7)', async () => {
    await loginAs('manager')
    const newToken = makeJWT('manager')
    const seen = new Set()
    const fetch = mockFetch([
      {
        match: (u, o) => u.startsWith('/api/'),
        reply: (u, o) => {
          if (!seen.has(u) && o.headers.Authorization !== 'Bearer ' + newToken) {
            seen.add(u)
            return { status: 401, body: { error: 'x', code: 'ERR_TOKEN_EXPIRED' } }
          }
          return { body: { ok: true } }
        },
      },
      { match: (u) => u === '/auth/refresh', reply: { body: { access_token: newToken, expires_in: 900 } } },
    ])
    await Promise.all([apiFetch('/api/leads/1'), apiFetch('/api/leads/2'), apiFetch('/api/leads/3')])
    expect(fetch.calls.filter((c) => c.url === '/auth/refresh')).toHaveLength(1)
  })
})
