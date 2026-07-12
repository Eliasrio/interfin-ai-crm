// emma.test.js — EP-07: счётчик токенов формулой воркера, шина WS-событий
// базы знаний, централизованный возврат на PIN-экран по 401 PIN_REQUIRED.
import { afterEach, describe, expect, it } from 'vitest'
import * as emma from './emma.js'
import * as auth from './auth.js'
import { makeJWT, mockFetch } from '../test/helpers.js'

afterEach(() => auth.logout())

describe('estimateTokens — формула воркера len(bytes)/4', () => {
  it('кириллица считается байтами: 8-байтовая строка = 2 токена', () => {
    // 'абвг' — 4 символа, 8 байт UTF-8: воркер видит 2 токена.
    expect(emma.estimateTokens('абвг')).toBe(2)
    // Наивный text.length/4 дал бы 1 — вдвое меньше (грабля из ТЗ §3).
    expect('абвг'.length / 4).toBe(1)
  })

  it('деление целочисленное вниз, как len/4 в Go', () => {
    expect(emma.estimateTokens('')).toBe(0)
    expect(emma.estimateTokens('abc')).toBe(0) // 3 байта
    expect(emma.estimateTokens('abcd')).toBe(1) // 4 байта
    expect(emma.estimateTokens('абв')).toBe(1) // 6 байт
  })
})

describe('шина emma_kb_status', () => {
  it('подписчик получает только события своего типа; отписка работает', () => {
    const got = []
    const off = emma.onKbStatus((ev) => got.push(ev))
    emma.applyKbEvent({ type: 'emma_kb_status', file_id: 5, status: 'indexed', chunks: 3 })
    emma.applyKbEvent({ type: 'stage_change', lead_id: 1 }) // чужое — мимо
    off()
    emma.applyKbEvent({ type: 'emma_kb_status', file_id: 6, status: 'error' })
    expect(got).toHaveLength(1)
    expect(got[0].file_id).toBe(5)
  })
})

describe('guard: 401 PIN_REQUIRED', () => {
  it('уведомляет подписчиков (панель вернётся на PIN-экран) и пробрасывает ошибку', async () => {
    mockFetch([
      { match: (u) => u === '/auth/login', reply: { body: { access_token: makeJWT('admin') } } },
      {
        match: (u) => u === '/api/emma/scenario',
        reply: { status: 401, body: { error: 'требуется PIN', code: 'PIN_REQUIRED' } },
      },
    ])
    await auth.login('a@x', 'pw')
    let notified = 0
    const off = emma.onPinRequired(() => notified++)
    await expect(emma.fetchScenario()).rejects.toMatchObject({ code: 'PIN_REQUIRED' })
    expect(notified).toBe(1)
    off()
  })
})
