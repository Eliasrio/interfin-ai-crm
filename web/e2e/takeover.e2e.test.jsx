// takeover.e2e.test.jsx — критерии приёмки M13 против живого бэкенда
// (только «мгновенные» вещи — task M13: кнопки, индикатор, молчание Эммы;
// цепочка напоминание→подхват закрыта интеграционными тестами воркера):
//
//   1. «Взять в работу» (REST сразу после создания лида — живая гонка
//      BUG-01): inbound клиента НЕ порождает ответ Эммы — стаб Telegram
//      пуст, в messages нет outbound;
//   2. кнопки в карточке: ✋/🤖 зовут PATCH mode, индикатор и сервер
//      меняются согласованно;
//   3. индикатор меняется live «из второй вкладки» (actor по REST, UI
//      узнаёт только из WS dialog_mode) — без перезагрузки;
//   4. секция «Настройки» (admin): интервалы сохраняются через
//      PATCH /api/settings и видны в GET.
//
// Подготовка — как chat.e2e (tg_stub + m10_e2e_setup.sh, у которого теперь
// есть e2e-m10-admin).
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { WebSocket as NodeWebSocket } from 'ws'
import React from 'react'

import App from '../src/App.jsx'
import * as auth from '../src/lib/auth.js'
import * as store from '../src/lib/store.js'

const BASE = process.env.E2E_BASE
const STUB = process.env.E2E_TG_STUB || 'http://localhost:8091'
const TG_SECRET = process.env.E2E_TG_SECRET || ''
const UI_EMAIL = 'e2e-m10-ui@interfin.com'
const ACTOR_EMAIL = 'e2e-m10-actor@interfin.com'
const ADMIN_EMAIL = 'e2e-m10-admin@interfin.com'
const PASSWORD = 'e2e-m10-pass'
const LEAD_NAME = 'M13 E2E Лид'

const realFetch = globalThis.fetch
let leadId
let leadTgID
let actorToken

// cookie-jar как в chat.e2e (undici куки не хранит; refresh — Path=/auth).
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

async function stubSent() {
  const res = await realFetch(new URL('/sent?chat_id=' + leadTgID, STUB))
  return res.json()
}

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
        from: { id: leadTgID, is_bot: false, first_name: LEAD_NAME, username: 'm13_e2e' },
        chat: { id: leadTgID, type: 'private' },
      },
    }),
  })
  if (res.status !== 200) throw new Error('webhook: HTTP ' + res.status)
}

async function waitUntil(cond, what, timeout = 8000, step = 250) {
  const t0 = Date.now()
  while (!(await cond())) {
    if (Date.now() - t0 > timeout) throw new Error('timeout: ' + what)
    await new Promise((r) => setTimeout(r, step))
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

describe.skipIf(!BASE || !TG_SECRET)('M13 приёмка: takeover (живой бэкенд + tg_stub)', () => {
  beforeAll(async () => {
    globalThis.fetch = cookieFetch
    globalThis.WebSocket = NodeWebSocket
    await realFetch(new URL('/sent', STUB), { method: 'DELETE' }).catch(() => {
      throw new Error('tg_stub недоступен на ' + STUB + ' — запусти go run ./scripts/tg_stub')
    })
    actorToken = await rawLogin(ACTOR_EMAIL)
    // Идемпотентность: лиды M13 прошлых прогонов стираются.
    const { body: all } = await rawApi('/api/leads?limit=200&offset=0', actorToken)
    for (const stale of (all?.leads ?? []).filter((l) => l.name === LEAD_NAME)) {
      await rawApi(`/api/lgpd/leads/${stale.id}/erase`, actorToken, { method: 'DELETE' })
    }
    // Лид рождается живым вебхуком; менеджер забирает диалог СРАЗУ —
    // живая гонка BUG-01: Эмма уже генерирует ответ на первое сообщение,
    // но обязана отбросить его по перечитанному режиму.
    leadTgID = 8870000000 + (Date.now() % 999999937)
    await pushInbound('Здравствуйте! Хочу узнать про услуги. [e2e m13 старт]')
    await waitUntil(
      async () => {
        const { body } = await rawApi('/api/leads?limit=200&offset=0', actorToken)
        const lead = body?.leads?.find((l) => l.name === LEAD_NAME)
        if (!lead) return false
        leadId = lead.id
        return true
      },
      'лид создан вебхуком (m10_e2e_setup.sh прогнан? секрет верный?)',
    )
    const { status } = await rawApi(`/api/leads/${leadId}/mode`, actorToken, {
      method: 'PATCH',
      body: { mode: 'human' },
    })
    if (status !== 200) throw new Error('PATCH mode → HTTP ' + status)
  })

  afterAll(async () => {
    // Настройки — общие для стенда: вернуть дефолты, чтобы прогоны не текли.
    try {
      const admin = await rawLogin(ADMIN_EMAIL)
      await rawApi('/api/settings', admin, {
        method: 'PATCH',
        body: {
          'takeover.hybrid_pause_minutes': 30,
          'takeover.reminder_minutes': 10,
          'takeover.pickup_minutes': 10,
        },
      })
    } catch {
      /* админ-восстановление best effort */
    }
    cleanup()
    store.reset()
    auth.logout()
    globalThis.fetch = realFetch
  })

  it('молчание Эммы: inbound при «ведёт менеджер» не порождает ответа (включая гонку BUG-01)', async () => {
    // Окно на догон гонки из beforeAll: если ответ Эммы на первое сообщение
    // всё же прорвался бы — он успеет доехать до стаба до базового среза.
    await sleep(4000)
    const baseline = (await stubSent()).length

    await pushInbound('Ау, вы тут? [e2e m13 тишина]')
    // Даём воркеру полный цикл (latency Claude 3–15с у живого API не будет:
    // Эмма обязана выйти ДО вызова Claude) — 6с с запасом.
    await sleep(6000)
    expect((await stubSent()).length).toBe(baseline)

    // В messages после нашего inbound нет НИ одного outbound.
    const { body } = await rawApi(`/api/leads/${leadId}/messages?limit=10`, actorToken)
    const idx = body.messages.findIndex((m) => m.content.includes('[e2e m13 тишина]'))
    expect(idx).toBeGreaterThanOrEqual(0)
    expect(body.messages.slice(idx + 1).filter((m) => m.direction === 'outbound')).toHaveLength(0)
  }, 30_000)

  it('кнопки в карточке: «Вернуть Эмме» / «Взять в работу» меняют индикатор и сервер', async () => {
    render(<App />)
    fireEvent.change(await screen.findByLabelText(/Email/), { target: { value: UI_EMAIL } })
    fireEvent.change(screen.getByLabelText(/Пароль/), { target: { value: PASSWORD } })
    fireEvent.click(screen.getByRole('button', { name: /Войти/ }))

    const card = await screen.findByText(LEAD_NAME, {}, { timeout: 10_000 })
    await waitFor(
      () => expect(document.querySelector('[data-connection]').dataset.connection).toBe('live'),
      { timeout: 10_000 },
    )
    fireEvent.click(card.closest('.card'))
    await screen.findByTestId('mode-bar')

    // Диалог у менеджера (beforeAll): индикатор ✋.
    expect(screen.getByTestId('mode-label').textContent).toContain('ведёт менеджер')

    // «Вернуть Эмме» → 🤖 и dialog_mode=bot на сервере.
    fireEvent.click(screen.getByRole('button', { name: /Вернуть Эмме/ }))
    await waitFor(() =>
      expect(screen.getByTestId('mode-label').textContent).toContain('ведёт Эмма'),
    )
    let { body } = await rawApi(`/api/leads/${leadId}`, actorToken)
    expect(body.lead.dialog_mode).toBe('bot')
    expect(body.lead.taken_by ?? null).toBeNull()

    // «Взять в работу» → ✋ и dialog_mode=human, taken_by заполнен.
    fireEvent.click(screen.getByRole('button', { name: /Взять в работу/ }))
    await waitFor(() =>
      expect(screen.getByTestId('mode-label').textContent).toContain('ведёт менеджер'),
    )
    ;({ body } = await rawApi(`/api/leads/${leadId}`, actorToken))
    expect(body.lead.dialog_mode).toBe('human')
    expect(body.lead.taken_by).toBeGreaterThan(0)
  })

  it('индикатор меняется live во «второй вкладке» без перезагрузки (WS dialog_mode)', async () => {
    // «Другая вкладка» — actor по REST; наш UI узнаёт ТОЛЬКО из WS.
    const flip = await rawApi(`/api/leads/${leadId}/mode`, actorToken, {
      method: 'PATCH',
      body: { mode: 'bot' },
    })
    expect(flip.status).toBe(200)
    await waitFor(
      () => expect(screen.getByTestId('mode-label').textContent).toContain('ведёт Эмма'),
      { timeout: 8000 },
    )

    const back = await rawApi(`/api/leads/${leadId}/mode`, actorToken, {
      method: 'PATCH',
      body: { mode: 'human' },
    })
    expect(back.status).toBe(200)
    await waitFor(
      () => expect(screen.getByTestId('mode-label').textContent).toContain('ведёт менеджер'),
      { timeout: 8000 },
    )
  })

  it('секция «Настройки» (admin): интервалы сохраняются через PATCH /api/settings', async () => {
    cleanup()
    store.reset()
    auth.logout()

    render(<App />)
    fireEvent.change(await screen.findByLabelText(/Email/), { target: { value: ADMIN_EMAIL } })
    fireEvent.change(screen.getByLabelText(/Пароль/), { target: { value: PASSWORD } })
    fireEvent.click(screen.getByRole('button', { name: /Войти/ }))

    // Кнопка «Настройки» видна admin; менеджерский прогон её не видел.
    fireEvent.click(await screen.findByRole('button', { name: /Настройки/ }, { timeout: 10_000 }))
    const input = await screen.findByLabelText(/Напоминание об ожидающем клиенте/)
    fireEvent.change(input, { target: { value: '4' } })
    fireEvent.click(screen.getByRole('button', { name: 'Сохранить' }))
    await waitFor(() => expect(screen.queryByLabelText(/Напоминание об ожидающем/)).toBeNull())

    // Значение действительно в БД (GET отдаёт эффективные значения).
    const admin = await rawLogin(ADMIN_EMAIL)
    const { body } = await rawApi('/api/settings', admin)
    expect(body.settings['takeover.reminder_minutes']).toBe(4)
  })
})
