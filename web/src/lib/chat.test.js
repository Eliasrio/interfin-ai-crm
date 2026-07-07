// chat.test.js — логика чат-панели M12: история/догрузка, live-события,
// дедуп собственного эха, перезапрос при reconnect/polling, и контракт
// «доска без чата молча игнорирует незнакомые типы событий».
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import * as auth from './auth.js'
import * as chat from './chat.js'
import * as store from './store.js'
import { makeJWT, mockFetch } from '../test/helpers.js'

function msg(id, direction, author, content) {
  return { id, direction, author, content, created_at: '2026-07-07T12:00:00Z' }
}

async function loginAs() {
  const token = makeJWT('manager')
  mockFetch([{ match: (u) => u === '/auth/login', reply: { body: { access_token: token } } }])
  await auth.login('m@x', 'pw')
}

beforeEach(async () => {
  store.reset()
  await loginAs()
})

afterEach(() => {
  chat.close()
  store.reset()
  auth.logout()
})

describe('история', () => {
  it('open грузит последнюю страницу; before_id листает назад', async () => {
    const page2 = Array.from({ length: chat.PAGE_SIZE }, (_, i) =>
      msg(100 + i, 'inbound', null, 'старое ' + i),
    )
    const fetch = mockFetch([
      {
        match: (u) => u.includes('/messages') && u.includes('before_id=100'),
        reply: { body: { messages: [msg(50, 'inbound', null, 'самое старое')] } },
      },
      {
        match: (u) => u.includes('/messages'),
        reply: { body: { messages: page2 } },
      },
    ])

    await chat.open(7)
    expect(chat.getState().leadId).toBe(7)
    expect(chat.getState().messages).toHaveLength(chat.PAGE_SIZE)
    expect(chat.getState().hasMore).toBe(true) // страница полная — выше есть ещё

    await chat.loadOlder()
    const st = chat.getState()
    expect(st.messages[0].content).toBe('самое старое') // старые встали СВЕРХУ
    expect(st.messages).toHaveLength(chat.PAGE_SIZE + 1)
    expect(st.hasMore).toBe(false) // страница неполная — упёрлись в начало
    expect(fetch.calls.some((c) => c.url.includes('before_id=100'))).toBe(true)
  })

  it('короткая история → hasMore=false, догрузка не дёргает сеть', async () => {
    const fetch = mockFetch([
      { match: (u) => u.includes('/messages'), reply: { body: { messages: [msg(1, 'inbound', null, 'привет')] } } },
    ])
    await chat.open(7)
    expect(chat.getState().hasMore).toBe(false)
    await chat.loadOlder()
    expect(fetch.calls.filter((c) => c.url.includes('/messages'))).toHaveLength(1)
  })
})

describe('live-события message', () => {
  beforeEach(async () => {
    mockFetch([{ match: (u) => u.includes('/messages'), reply: { body: { messages: [] } } }])
    await chat.open(7)
  })

  it('inbound лида и ответ Эммы добавляются; чужой лид — нет', () => {
    chat.applyEvent({ type: 'message', lead_id: 7, direction: 'inbound', content: 'вопрос', ts: 't1' })
    chat.applyEvent({ type: 'message', lead_id: 7, direction: 'outbound', author: 'bot', content: 'ответ', ts: 't2' })
    chat.applyEvent({ type: 'message', lead_id: 8, direction: 'inbound', content: 'чужое', ts: 't3' })
    const { messages } = chat.getState()
    expect(messages.map((m) => m.content)).toEqual(['вопрос', 'ответ'])
    expect(chat.bubbleRole(messages[0])).toBe('client')
    expect(chat.bubbleRole(messages[1])).toBe('bot')
  })

  it('author=NULL у старых outbound читается как Эмма, manager:* — как менеджер', () => {
    expect(chat.bubbleRole(msg(1, 'outbound', null, 'x'))).toBe('bot')
    expect(chat.bubbleRole(msg(2, 'outbound', 'bot', 'x'))).toBe('bot')
    expect(chat.bubbleRole(msg(3, 'outbound', 'manager:5', 'x'))).toBe('manager')
  })

  it('эхо собственного POST не дублирует пузырь, чужие manager-реплики проходят', async () => {
    mockFetch([
      {
        match: (u, o) => u.includes('/messages') && o.method === 'POST',
        reply: { body: { message: msg(10, 'outbound', 'manager:1', 'наш текст') } },
      },
      { match: (u) => u.includes('/messages'), reply: { body: { messages: [] } } },
    ])
    await chat.send('наш текст')
    expect(chat.getState().messages).toHaveLength(1)

    // Эхо по WS о том же сообщении — глотается.
    chat.applyEvent({ type: 'message', lead_id: 7, direction: 'outbound', author: 'manager:1', content: 'наш текст', ts: 't' })
    expect(chat.getState().messages).toHaveLength(1)

    // Реплика ДРУГОГО менеджера (второй вкладки) — добавляется.
    chat.applyEvent({ type: 'message', lead_id: 7, direction: 'outbound', author: 'manager:2', content: 'другой текст', ts: 't' })
    expect(chat.getState().messages).toHaveLength(2)

    // Повторные одинаковые live-реплики лида дедупу не подлежат.
    chat.applyEvent({ type: 'message', lead_id: 7, direction: 'inbound', content: 'да', ts: 't1' })
    chat.applyEvent({ type: 'message', lead_id: 7, direction: 'inbound', content: 'да', ts: 't2' })
    expect(chat.getState().messages).toHaveLength(4)
  })

  it('эхо ОБГОНЯЕТ ответ ручки (боевой баг): пузырь один и получает id', async () => {
    mockFetch([
      {
        match: (u, o) => u.includes('/messages') && o.method === 'POST',
        reply: { body: { message: msg(10, 'outbound', 'manager:1', 'обгон') } },
      },
      { match: (u) => u.includes('/messages'), reply: { body: { messages: [] } } },
    ])
    const inflight = chat.send('обгон')
    // WS-эхо прилетает, пока POST ещё в полёте.
    chat.applyEvent({ type: 'message', lead_id: 7, direction: 'outbound', author: 'manager:1', content: 'обгон', ts: 't' })
    await inflight

    const bubbles = chat.getState().messages.filter((m) => m.content === 'обгон')
    expect(bubbles).toHaveLength(1)
    expect(bubbles[0].id).toBe(10) // live-пузырь поднят до строки с id
  })
})

describe('reconnect/polling', () => {
  it('переход в live/polling перезапрашивает историю (событие могло потеряться)', async () => {
    const fetch = mockFetch([
      { match: (u) => u.includes('/messages'), reply: { body: { messages: [msg(1, 'inbound', null, 'x')] } } },
    ])
    store.setConnection('live') // первый connect — до открытия карточки
    await chat.open(7)
    const before = fetch.calls.filter((c) => c.url.includes('/messages')).length

    store.setConnection('reconnecting')
    store.setConnection('live') // reconnect завершён → refresh
    store.setConnection('polling') // WS умер совсем → тоже refresh
    await new Promise((r) => setTimeout(r, 0))

    const after = fetch.calls.filter((c) => c.url.includes('/messages')).length
    expect(after).toBe(before + 2)
  })
})

describe('контракт WS (M12 наружу)', () => {
  it('доска без чата молча игнорирует незнакомый тип события, но двигает lastEventTs', () => {
    store.applyLeads([{ id: 7, stage_id: 2, name: 'Лид', last_activity_at: 't' }])
    const ok = store.applyEvent({ type: 'message', lead_id: 7, stage_id: 2, direction: 'inbound', content: 'x', ts: '2026-07-07T12:00:00Z' })
    expect(ok).toBe(true) // «знаю лида, тип не мой» — не считается неизвестным лидом
    const st = store.getState()
    expect(st.lastEventTs).toBe('2026-07-07T12:00:00Z') // catch-up не перезапросит событие
    expect(st.leadsById[7].stage_id).toBe(2) // stage событие message не трогает
    expect(st.alerts).toHaveLength(0)
  })
})
