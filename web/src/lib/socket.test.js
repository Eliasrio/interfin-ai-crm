// socket.test.js — задача M10-3: события WS, реконнект с catch-up по §10.3
// (refresh → GET ?updated_since → новый WS) и polling-fallback.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import * as auth from './auth.js'
import * as store from './store.js'
import { KanbanSocket } from './socket.js'
import { FakeWebSocket, jsonResponse, makeJWT, makeLead, mockFetch } from '../test/helpers.js'

let socket

beforeEach(async () => {
  vi.useFakeTimers()
  FakeWebSocket.reset()
  globalThis.WebSocket = FakeWebSocket
  store.reset()
  mockFetch([{ match: (u) => u === '/auth/login', reply: { body: { access_token: makeJWT('manager'), expires_in: 900 } } }])
  await auth.login('m@x', 'pw')
})

afterEach(() => {
  socket?.stop()
  auth.logout()
  vi.useRealTimers()
})

// leadsRoute — GET /api/leads с записью updated_since каждого вызова.
function leadsRoute(leadsByCall, sinceLog) {
  let call = 0
  return {
    match: (u) => u.startsWith('/api/leads?'),
    reply: (u) => {
      sinceLog?.push(new URL(u, 'http://x').searchParams.get('updated_since'))
      const leads = leadsByCall[Math.min(call++, leadsByCall.length - 1)]
      return { body: { leads, total: leads.length, limit: 200, offset: 0 } }
    },
  }
}

async function startSocket(routes, opts = {}) {
  mockFetch(routes)
  socket = new KanbanSocket({ wsUrl: 'ws://test/ws/kanban', reconnectBaseMs: 10, log: { warn: () => {} }, ...opts })
  await socket.start()
  return FakeWebSocket.last()
}

describe('KanbanSocket', () => {
  it('подключается с субпротоколом Bearer.<token> и применяет события', async () => {
    const lead = makeLead({ id: 1, stage_id: 1 })
    const ws = await startSocket([leadsRoute([[lead]])])
    expect(ws.protocol).toBe('Bearer.' + auth.getToken())

    ws.serverOpen()
    expect(store.getState().connection).toBe('live')

    const ts = new Date().toISOString()
    ws.serverSend({ type: 'stage_change', lead_id: 1, stage_id: 2, old_stage_id: 1, actor: 'system', ts })
    expect(store.getState().leadsById[1].stage_id).toBe(2)
    expect(store.getState().lastEventTs).toBe(ts)

    ws.serverSend({ type: 'ttl_warning', lead_id: 1, stage_id: 2, ts })
    expect(store.getState().leadsById[1].ttlWarning).toBe(true)
    ws.serverSend({ type: 'antispam_alert', lead_id: 1, stage_id: 2, anti_spam_count: 25, ts })
    expect(store.getState().leadsById[1].anti_spam_count).toBe(25)
    expect(store.getState().alerts.length).toBeGreaterThan(0)
  })

  it('событие по неизвестному лиду дотягивает карточку по REST', async () => {
    const fresh = makeLead({ id: 42, stage_id: 2 })
    const ws = await startSocket([
      leadsRoute([[]]),
      { match: (u) => u === '/api/leads/42', reply: { body: { lead: fresh, messages: [], payment_events: [] } } },
    ])
    ws.serverOpen()
    ws.serverSend({ type: 'stage_change', lead_id: 42, stage_id: 2, ts: new Date().toISOString() })
    await vi.waitFor(() => expect(store.getState().leadsById[42]).toBeTruthy())
  })

  it('обрыв 4001 → refresh → catch-up ?updated_since → новый WS (§10.3)', async () => {
    const lead = makeLead({ id: 1, stage_id: 2 })
    // Событие ПОЗЖЕ стартового среза: lastEventTs только растёт, catch-up
    // должен уйти именно с ts события.
    const evTs = new Date(Date.now() + 60_000).toISOString()
    const sinceLog = []
    const order = []
    const newToken = makeJWT('manager')
    const ws = await startSocket([
      {
        match: (u) => u === '/auth/refresh',
        reply: () => {
          order.push('refresh')
          return { body: { access_token: newToken, expires_in: 900 } }
        },
      },
      {
        match: (u) => u.startsWith('/api/leads?'),
        reply: (u) => {
          order.push('catchup')
          sinceLog.push(new URL(u, 'http://x').searchParams.get('updated_since'))
          const missed = sinceLog.length > 1 ? [{ ...lead, stage_id: 7 }] : [lead]
          return { body: { leads: missed, total: missed.length, limit: 200, offset: 0 } }
        },
      },
    ])
    ws.serverOpen()
    ws.serverSend({ type: 'stage_change', lead_id: 1, stage_id: 2, ts: evTs })

    ws.serverClose(4001) // access-токен истёк (§5.3)
    expect(store.getState().connection).toBe('reconnecting')
    await vi.advanceTimersByTimeAsync(50)

    // §10.3: refresh → catch-up с last_event_ts → resubscribe.
    expect(order).toEqual(['catchup', 'refresh', 'catchup']) // первый — start()
    expect(sinceLog[1]).toBe(evTs)
    expect(store.getState().leadsById[1].stage_id).toBe(7) // пропуск добран
    const ws2 = FakeWebSocket.last()
    expect(ws2).not.toBe(ws)
    expect(ws2.protocol).toBe('Bearer.' + newToken) // новый токен
    ws2.serverOpen()
    expect(store.getState().connection).toBe('live')
  })

  it('refresh невозможен → сокет останавливается (сессия кончилась)', async () => {
    const ws = await startSocket([
      leadsRoute([[]]),
      { match: (u) => u === '/auth/refresh', reply: { status: 401, body: { error: 'x', code: 'ERR_REFRESH_EXPIRED' } } },
    ])
    ws.serverOpen()
    ws.serverClose(4001)
    await vi.advanceTimersByTimeAsync(50)
    expect(auth.getToken()).toBeNull()
    expect(FakeWebSocket.instances).toHaveLength(1) // новых попыток нет
  })

  it('polling_mode от Hub → REST-опрос; live_mode → catch-up и обратно live', async () => {
    const lead = makeLead({ id: 5, stage_id: 2 })
    const sinceLog = []
    const ws = await startSocket([
      { match: (u) => u === '/auth/refresh', reply: { body: { access_token: makeJWT('manager'), expires_in: 900 } } },
      leadsRoute([[lead], [{ ...lead, stage_id: 3 }], [{ ...lead, stage_id: 4 }]], sinceLog),
    ])
    ws.serverOpen()

    ws.serverSend({ type: 'polling_mode', poll_interval_sec: 5 })
    expect(store.getState().connection).toBe('polling')
    await vi.advanceTimersByTimeAsync(0) // немедленный первый тик
    await vi.advanceTimersByTimeAsync(5000)
    expect(store.getState().leadsById[5].stage_id).toBe(4) // два опроса прошли

    ws.serverSend({ type: 'live_mode' })
    await vi.advanceTimersByTimeAsync(0)
    expect(store.getState().connection).toBe('live')
    await vi.advanceTimersByTimeAsync(15_000)
    expect(sinceLog.length).toBe(4) // start + 2 опроса + catch-up после live_mode; интервал погашен
  })

  it('WS недоступен дважды подряд → fallback в polling, реконнект продолжается', async () => {
    const lead = makeLead({ id: 9, stage_id: 1 })
    let stage = 1
    const ws = await startSocket([
      { match: (u) => u === '/auth/refresh', reply: { body: { access_token: makeJWT('manager'), expires_in: 900 } } },
      {
        match: (u) => u.startsWith('/api/leads?'),
        reply: () => jsonResponse(200, { leads: [{ ...lead, stage_id: ++stage }], total: 1, limit: 200, offset: 0 }),
      },
    ])
    ws.serverOpen()
    ws.serverClose(1006) // обрыв
    await vi.advanceTimersByTimeAsync(15)
    FakeWebSocket.last().serverClose(1006) // реконнект тут же оборвался
    await vi.advanceTimersByTimeAsync(25)
    expect(store.getState().connection).toBe('polling') // доска живёт на REST

    const before = store.getState().leadsById[9].stage_id
    await vi.advanceTimersByTimeAsync(5000)
    expect(store.getState().leadsById[9].stage_id).toBeGreaterThan(before)

    // WS ожил → polling гаснет, снова live.
    FakeWebSocket.last().serverOpen()
    expect(store.getState().connection).toBe('live')
    const after = store.getState().leadsById[9].stage_id
    await vi.advanceTimersByTimeAsync(10_000)
    expect(store.getState().leadsById[9].stage_id).toBe(after)
  })
})
