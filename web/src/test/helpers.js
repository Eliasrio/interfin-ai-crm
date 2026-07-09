// helpers.js — общие заглушки тестов: фейковый JWT, роутер fetch-моков,
// фейковый WebSocket и DataTransfer (jsdom их не реализует).
import { vi } from 'vitest'

// makeJWT — неподписанный JWT: клиент подпись не проверяет (lib/auth
// декодирует только payload), бэкенда в юнит-тестах нет. iat-счётчик
// делает каждый токен уникальным — тесты различают «старый» и «новый».
let jwtSeq = 0
export function makeJWT(role = 'manager', sub = '1', ttlSec = 900) {
  const b64 = (obj) =>
    Buffer.from(JSON.stringify(obj)).toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  const payload = { sub, role, iat: ++jwtSeq, exp: Math.floor(Date.now() / 1000) + ttlSec }
  return `${b64({ alg: 'RS256', typ: 'JWT' })}.${b64(payload)}.fakesig`
}

export function jsonResponse(status, body) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

// mockFetch — маршрутизатор: routes = [{match: (url, opts) => bool, reply}].
// reply — объект {status, body} или функция (url, opts) => Response|{...}.
export function mockFetch(routes) {
  const calls = []
  const fn = vi.fn(async (url, opts = {}) => {
    calls.push({ url: String(url), opts })
    for (const r of routes) {
      if (!r.match(String(url), opts)) continue
      const reply = typeof r.reply === 'function' ? r.reply(String(url), opts) : r.reply
      if (reply instanceof Response) return reply
      return jsonResponse(reply.status ?? 200, reply.body ?? {})
    }
    throw new Error('mockFetch: нет маршрута для ' + url)
  })
  fn.calls = calls
  globalThis.fetch = fn
  return fn
}

// FakeWebSocket — управляемая замена браузерного WebSocket.
export class FakeWebSocket {
  static instances = []
  constructor(url, protocol) {
    this.url = url
    this.protocol = protocol
    this.closed = false
    FakeWebSocket.instances.push(this)
  }
  close(code = 1000) {
    if (this.closed) return
    this.closed = true
    this.onclose?.({ code })
  }
  // серверная сторона:
  serverOpen() {
    this.onopen?.()
  }
  serverSend(obj) {
    this.onmessage?.({ data: JSON.stringify(obj) })
  }
  serverClose(code) {
    this.closed = true
    this.onclose?.({ code })
  }
  static reset() {
    FakeWebSocket.instances = []
  }
  static last() {
    return FakeWebSocket.instances[FakeWebSocket.instances.length - 1]
  }
}

// makeDataTransfer — минимум DataTransfer для HTML5 DnD в jsdom.
export function makeDataTransfer() {
  const data = {}
  return {
    setData: (type, val) => {
      data[type] = val
    },
    getData: (type) => data[type] ?? '',
    effectAllowed: '',
    dropEffect: '',
  }
}

let leadSeq = 100
export function makeLead(overrides = {}) {
  const id = overrides.id ?? ++leadSeq
  return {
    id,
    telegram_user_id: 1000 + id,
    name: 'Лид ' + id,
    phone: null,
    tg_username: 'user' + id,
    stage_id: 1,
    message_count: 3,
    anti_spam_count: 0,
    manual_resolution: false,
    dialog_mode: 'bot', // M13: зеркалит DEFAULT 'bot' в DTO
    bot_silenced_until: null,
    taken_by: null,
    last_activity_at: new Date().toISOString(),
    created_at: new Date().toISOString(),
    ...overrides,
  }
}
