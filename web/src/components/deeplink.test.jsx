// Тесты deep-link ?lead=<id> (ссылка из Telegram-уведомления менеджеру).
import { describe, it, expect, vi } from 'vitest'
import { consumeLeadParam } from '../lib/deeplink.js'

function fakeEnv(search) {
  const hist = { replaceState: vi.fn() }
  const loc = { search, pathname: '/' }
  return { loc, hist }
}

describe('consumeLeadParam', () => {
  it('возвращает id и стирает параметр из адреса', () => {
    const { loc, hist } = fakeEnv('?lead=12')
    expect(consumeLeadParam(loc, hist)).toBe(12)
    expect(hist.replaceState).toHaveBeenCalledWith(null, '', '/')
  })

  it('сохраняет остальные параметры', () => {
    const { loc, hist } = fakeEnv('?foo=bar&lead=7')
    expect(consumeLeadParam(loc, hist)).toBe(7)
    expect(hist.replaceState).toHaveBeenCalledWith(null, '', '/?foo=bar')
  })

  it('без параметра — null, адрес не трогает', () => {
    const { loc, hist } = fakeEnv('')
    expect(consumeLeadParam(loc, hist)).toBeNull()
    expect(hist.replaceState).not.toHaveBeenCalled()
  })

  it('мусорный id — null, адрес не трогает', () => {
    for (const search of ['?lead=abc', '?lead=-3', '?lead=1.5', '?lead=0']) {
      const { loc, hist } = fakeEnv(search)
      expect(consumeLeadParam(loc, hist)).toBeNull()
      expect(hist.replaceState).not.toHaveBeenCalled()
    }
  })
})
