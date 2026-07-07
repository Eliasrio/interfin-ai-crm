// chat.e2e.test.jsx — критерии приёмки M12 против ЖИВОГО бэкенда:
//
//   1. сообщение из карточки доходит «в Telegram» (стаб scripts/tg_stub) и
//      ложится в messages (outbound, author=manager:<id>);
//   2. реплика второго менеджера появляется в открытой карточке live;
//   3. inbound лида (через POST /webhook/telegram) приходит WS-событием
//      message — чат живой в обе стороны;
//   4. счёт из карточки: менеджер видит URL, ссылка уходит клиенту в чат
//      (E2E_INVOICE=1 — нужен CRYPTOBOT_TESTNET_TOKEN на сервере; сам
//      контур «оплата двигает карточку» закрыт contract-тестами M6);
//   5. обрыв WS не теряет реплику — история перезапрашивается (§10.3).
//
// Подготовка (сверх m10; боевой Telegram недоставит фейковому лиду,
// поэтому сервер смотрит в стаб):
//   docker compose up -d postgres redis && docker compose stop app
//   go run ./scripts/tg_stub &                       # фейковый Bot API :8091
//   TELEGRAM_API_URL=http://localhost:8091 go run ./cmd/server &
//   bash scripts/m10_e2e_setup.sh
//   cd web && E2E_BASE=http://localhost:8080 \
//     E2E_TG_STUB=http://localhost:8091 \
//     E2E_TG_SECRET=$TELEGRAM_WEBHOOK_SECRET npm run test:e2e
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { WebSocket as NodeWebSocket } from 'ws'
import React from 'react'

import App from '../src/App.jsx'
import * as auth from '../src/lib/auth.js'
import * as chat from '../src/lib/chat.js'
import * as store from '../src/lib/store.js'
import { KanbanSocket } from '../src/lib/socket.js'

const BASE = process.env.E2E_BASE
const STUB = process.env.E2E_TG_STUB || 'http://localhost:8091'
const TG_SECRET = process.env.E2E_TG_SECRET || ''
const UI_EMAIL = 'e2e-m10-ui@interfin.com' // учётки — из scripts/m10_e2e_setup.sh
const ACTOR_EMAIL = 'e2e-m10-actor@interfin.com'
const PASSWORD = 'e2e-m10-pass'
// Лид у M12 СВОЙ — создаётся в beforeAll живым вебхуком (ingestion M2):
// порядок e2e-файлов недетерминирован, а kanban-сценарий стирает своего
// лида LGPD-тестом. tg id уникален на прогон (см. m10_e2e_setup.sh о хеше).
const LEAD_NAME = 'M12 E2E Лид'

const realFetch = globalThis.fetch
let leadId
let leadTgID // telegram_user_id лида — chat id доставок в стабе
let actorToken

// cookie-jar как в kanban.e2e (undici куки не хранит; refresh — Path=/auth).
const jar = new Map()
function cookieFetch(input, init) {
  const url = new URL(input, BASE)
  const headers = new Headers(init?.headers)
  if (url.pathname.startsWith('/auth') && jar.size) {
    headers.set('cookie', [...jar].map(([k, v]) => `${k}=${v}`).join('; '))
  }
  return realFetch(url, { ...init, headers }).then((res) => {
    for (const sc of res.headers.getSetCookie?.() ?? []) {
      const [pair] = sc.split(';')
      const eq = pair.indexOf('=')
      const k = pair.slice(0, eq).trim()
      const v = pair.slice(eq + 1)
      if (v) jar.set(k, v)
      else jar.delete(k)
    }
    return res
  })
}

async function rawLogin(email) {
  const res = await realFetch(new URL('/auth/login', BASE), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password: PASSWORD }),
  })
  if (!res.ok) throw new Error('rawLogin ' + email + ': HTTP ' + res.status)
  return (await res.json()).access_token
}

async function rawApi(path, token, opts = {}) {
  const res = await realFetch(new URL(path, BASE), {
    method: opts.method || 'GET',
    headers: {
      Authorization: 'Bearer ' + token,
      ...(opts.body ? { 'Content-Type': 'application/json' } : {}),
    },
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  })
  return { status: res.status, body: await res.json().catch(() => null) }
}

// stubSent — доставки sendMessage, принятые фейковым Telegram.
async function stubSent() {
  const res = await realFetch(new URL('/sent?chat_id=' + leadTgID, STUB))
  return res.json()
}

// pushInbound — «лид написал боту»: живой контур ingestion M2 целиком.
async function pushInbound(text) {
  const nonce = Math.floor(Date.now() % 1e9)
  const res = await realFetch(new URL('/webhook/telegram', BASE), {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-Telegram-Bot-Api-Secret-Token': TG_SECRET,
    },
    body: JSON.stringify({
      update_id: nonce,
      message: {
        message_id: nonce,
        date: Math.floor(Date.now() / 1000),
        text,
        from: { id: leadTgID, is_bot: false, first_name: LEAD_NAME, username: 'm12_e2e' },
        chat: { id: leadTgID, type: 'private' },
      },
    }),
  })
  if (res.status !== 200) throw new Error('webhook: HTTP ' + res.status)
}

async function waitUntil(cond, what, timeout = 8000, step = 25) {
  const t0 = Date.now()
  while (!(await cond())) {
    if (Date.now() - t0 > timeout) throw new Error('timeout: ' + what)
    await new Promise((r) => setTimeout(r, step))
  }
}

describe.skipIf(!BASE || !TG_SECRET)('M12 приёмка: чат менеджера (живой бэкенд + tg_stub)', () => {
  beforeAll(async () => {
    globalThis.fetch = cookieFetch
    globalThis.WebSocket = NodeWebSocket
    // Стаб обязателен: без него «доставку в Telegram» не проверить.
    await realFetch(new URL('/sent', STUB), { method: 'DELETE' }).catch(() => {
      throw new Error('tg_stub недоступен на ' + STUB + ' — запусти go run ./scripts/tg_stub')
    })
    actorToken = await rawLogin(ACTOR_EMAIL)
    // Идемпотентность: лиды M12 прошлых прогонов стираются LGPD-erase'ом,
    // иначе на доске несколько карточек с одним именем.
    const { body: all } = await rawApi('/api/leads?limit=200&offset=0', actorToken)
    for (const stale of (all?.leads ?? []).filter((l) => l.name === LEAD_NAME)) {
      await rawApi(`/api/lgpd/leads/${stale.id}/erase`, actorToken, { method: 'DELETE' })
    }
    // Лид рождается живым вебхуком — как в проде (findOrCreateLead M2).
    leadTgID = 8880000000 + (Date.now() % 999999937)
    await pushInbound('Здравствуйте! Расскажите про гражданство через роды. [e2e старт]')
    await waitUntil(
      async () => {
        const { body } = await rawApi('/api/leads?limit=200&offset=0', actorToken)
        const lead = body?.leads?.find((l) => l.name === LEAD_NAME)
        if (!lead) return false
        leadId = lead.id
        return true
      },
      'лид создан вебхуком (m10_e2e_setup.sh прогнан? секрет верный?)',
      8000,
      250, // не частить: /api под rate limit 100/мин
    )
  })

  afterAll(() => {
    cleanup()
    store.reset()
    auth.logout()
    globalThis.fetch = realFetch
  })

  it('сообщение из карточки: стаб Telegram получил текст, в messages outbound c author=manager:<id>', async () => {
    render(<App />)
    fireEvent.change(await screen.findByLabelText(/Email/), { target: { value: UI_EMAIL } })
    fireEvent.change(screen.getByLabelText(/Пароль/), { target: { value: PASSWORD } })
    fireEvent.click(screen.getByRole('button', { name: /Войти/ }))

    // Доска загрузилась, WS live; открываем карточку кликом.
    const card = await screen.findByText(LEAD_NAME, {}, { timeout: 10_000 })
    await waitFor(
      () => expect(document.querySelector('[data-connection]').dataset.connection).toBe('live'),
      { timeout: 10_000 },
    )
    fireEvent.click(card.closest('.card'))
    await screen.findByTestId('chat-panel')

    // Реплика менеджера из поля ввода.
    const text = 'Здравствуйте! Это менеджер, продолжу диалог. [e2e ' + Date.now() + ']'
    fireEvent.change(screen.getByLabelText('Сообщение лиду'), { target: { value: text } })
    fireEvent.click(screen.getByRole('button', { name: 'Отправить' }))

    // Пузырь менеджера появился (ответ ручки), зелёный класс менеджера.
    // Ищем ВНУТРИ списка чата: React зеркалит value textarea в textContent,
    // и глобальный findByText находил бы сам textarea.
    const list = within(screen.getByTestId('chat-list'))
    const bubble = await list.findByText(text, {}, { timeout: 8000 })
    expect(bubble.closest('.msg').className).toContain('msg-role-manager')

    // «Дошло лиду в Telegram» — доставка лежит в стабе.
    await waitUntil(async () => (await stubSent()).some((m) => m.text === text), 'стаб получил sendMessage')

    // Строка в messages: outbound, author=manager:<id> (0012).
    const { body } = await rawApi(`/api/leads/${leadId}/messages?limit=5`, actorToken)
    const last = body.messages[body.messages.length - 1]
    expect(last.direction).toBe('outbound')
    expect(last.author).toMatch(/^manager:\d+$/)
    expect(last.content).toBe(text)
  })

  it('вторая вкладка live: реплика другого менеджера появляется без перезагрузки', async () => {
    // «Другая вкладка» — actor шлёт по REST; наш UI узнаёт ТОЛЬКО из WS.
    const text = 'Реплика второго менеджера [e2e ' + Date.now() + ']'
    const posted = await rawApi(`/api/leads/${leadId}/messages`, actorToken, {
      method: 'POST',
      body: { text },
    })
    expect(posted.status).toBe(200)
    const bubble = await screen.findByText(text, {}, { timeout: 5000 })
    expect(bubble.closest('.msg').className).toContain('msg-role-manager')
  })

  it.skipIf(!TG_SECRET)('inbound лида приходит WS-событием message — чат живой в обе стороны', async () => {
    const text = 'Живой вопрос клиента [e2e ' + Date.now() + ']'
    await pushInbound(text)
    const bubble = await screen.findByText(text, {}, { timeout: 5000 })
    expect(bubble.closest('.msg').className).toContain('msg-role-client')
  })

  it.skipIf(!process.env.E2E_INVOICE)('счёт из карточки: менеджер видит URL, ссылка уходит в чат', async () => {
    fireEvent.click(screen.getByRole('button', { name: 'Выставить счёт' }))
    fireEvent.change(screen.getByLabelText('Сумма счёта'), { target: { value: '1' } })
    fireEvent.change(screen.getByLabelText('Описание счёта'), { target: { value: 'e2e смоук M12' } })
    fireEvent.click(screen.getByRole('button', { name: 'Выставить счёт…' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Да, выставить' }))

    // Менеджер видит URL (копируемый) …
    const result = await screen.findByTestId('invoice-result', {}, { timeout: 15_000 })
    const url = result.querySelector('a').href
    expect(url).toMatch(/^https:\/\/t\.me\//)
    // … и ссылка ушла клиенту в чат через бота.
    await waitUntil(async () => (await stubSent()).some((m) => m.text.includes(url)), 'ссылка в чате лида')
    // Оплата двигает карточку — контур M6 (вебхук+машина), закрыт его
    // contract-тестами; живой смоук — tasks/M12 §7 (checklist, testnet).
  })

  it('обрыв WS не теряет реплику: история перезапрашивается при реконнекте (§10.3)', async () => {
    cleanup() // App размонтирован; дальше — уровень lib, как AQ²-5 в m10
    store.reset()
    await auth.login(UI_EMAIL, PASSWORD)
    const socket = new KanbanSocket({
      wsUrl: BASE.replace(/^http/, 'ws') + '/ws/kanban',
      reconnectBaseMs: 2500, // окно «клиент отвалился»
    })
    try {
      await socket.start()
      await waitUntil(() => store.getState().connection === 'live', 'WS live')
      await chat.open(leadId)

      // Обрыв TCP без вежливого handshake (иначе событие успеет доехать).
      socket.ws.terminate()
      await waitUntil(() => store.getState().connection !== 'live', 'обрыв замечен')

      const text = 'Потерянная в обрыве реплика [e2e ' + Date.now() + ']'
      const posted = await rawApi(`/api/leads/${leadId}/messages`, actorToken, {
        method: 'POST',
        body: { text },
      })
      expect(posted.status).toBe(200)
      // Событие издано в окно обрыва — WS-путь его потерял навсегда.
      expect(chat.getState().messages.some((m) => m.content === text)).toBe(false)

      // Реконнект → live → chat.refresh() добирает историю по REST.
      await waitUntil(
        () => chat.getState().messages.some((m) => m.content === text),
        'история добрана после reconnect',
        15_000,
      )
    } finally {
      socket.stop()
      chat.close()
      store.reset()
      auth.logout()
    }
  })
})
