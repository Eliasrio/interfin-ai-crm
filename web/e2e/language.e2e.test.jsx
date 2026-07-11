// language.e2e.test.jsx — критерии приёмки M14 против живого бэкенда
// (только «мгновенные» вещи — детекция первого сообщения, бейдж, ручная
// смена, live во второй вкладке; ответы Эммы на en/es проверяют
// contract-тесты processor с фейковым Claude — живой Claude в e2e не дёргаем):
//
//   1. первое сообщение «Hola, quiero la ciudadanía» → в карточке ES
//      (детекция ingestion той же транзакцией, что сообщение);
//   2. бейдж языка виден на карточке и в модалке;
//   3. ручная смена через выпадающий список → PATCH language, сервер и
//      бейдж меняются согласованно;
//   4. событие lead_language доходит до «второй вкладки» live (actor по
//      REST, UI узнаёт только из WS) — без перезагрузки.
//
// Подготовка — как chat.e2e/takeover.e2e (tg_stub + m10_e2e_setup.sh).
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
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
const PASSWORD = 'e2e-m10-pass'
const LEAD_NAME = 'M14 E2E Лид'

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
        from: { id: leadTgID, is_bot: false, first_name: LEAD_NAME, username: 'm14_e2e' },
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

describe.skipIf(!BASE || !TG_SECRET)('M14 приёмка: язык клиента (живой бэкенд + tg_stub)', () => {
  beforeAll(async () => {
    globalThis.fetch = cookieFetch
    globalThis.WebSocket = NodeWebSocket
    await realFetch(new URL('/sent', STUB), { method: 'DELETE' }).catch(() => {
      throw new Error('tg_stub недоступен на ' + STUB + ' — запусти go run ./scripts/tg_stub')
    })
    actorToken = await rawLogin(ACTOR_EMAIL)
    // Идемпотентность: лиды M14 прошлых прогонов стираются.
    const { body: all } = await rawApi('/api/leads?limit=200&offset=0', actorToken)
    for (const stale of (all?.leads ?? []).filter((l) => l.name === LEAD_NAME)) {
      await rawApi(`/api/lgpd/leads/${stale.id}/erase`, actorToken, { method: 'DELETE' })
    }
    // Лид рождается живым вебхуком с испанским первым сообщением —
    // критерий 1: детекция ingestion, в карточке ES.
    leadTgID = 8880000000 + (Date.now() % 999999937)
    // Маркер только латиницей: кириллица в тексте увела бы детектор в ru.
    await pushInbound('Hola, quiero la ciudadanía [e2e m14 start]')
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
  })

  afterAll(() => {
    cleanup()
    store.reset()
    auth.logout()
    globalThis.fetch = realFetch
  })

  it('детекция первого сообщения: испанский текст → language=es в REST', async () => {
    const { body } = await rawApi(`/api/leads/${leadId}`, actorToken)
    expect(body.lead.language).toBe('es')
  })

  it('бейдж 🇪🇸 ES виден на карточке и в модалке; ручная смена через список', async () => {
    render(<App />)
    fireEvent.change(await screen.findByLabelText(/Email/), { target: { value: UI_EMAIL } })
    fireEvent.change(screen.getByLabelText(/Пароль/), { target: { value: PASSWORD } })
    fireEvent.click(screen.getByRole('button', { name: /Войти/ }))

    const title = await screen.findByText(LEAD_NAME, {}, { timeout: 10_000 })
    await waitFor(
      () => expect(document.querySelector('[data-connection]').dataset.connection).toBe('live'),
      { timeout: 10_000 },
    )
    const card = title.closest('.card')

    // Бейдж на карточке (критерий 2).
    expect(within(card).getByTestId('lang-badge').textContent).toContain('🇪🇸 ES')

    // В модалке: бейдж + селект на es.
    fireEvent.click(card)
    const select = await screen.findByTestId('lang-select')
    expect(select.value).toBe('es')
    expect(screen.getByTestId('lang-badge-modal').textContent).toContain('🇪🇸 ES')

    // Ручная смена es → ru (критерий 3): PATCH уходит, сервер и UI согласованы.
    fireEvent.change(select, { target: { value: 'ru' } })
    await waitFor(() => expect(screen.getByTestId('lang-badge-modal').textContent).toContain('🇷🇺 RU'), {
      timeout: 8000,
    })
    const { body } = await rawApi(`/api/leads/${leadId}`, actorToken)
    expect(body.lead.language).toBe('ru')
    // Бейдж карточки обновился тем же store.
    expect(within(card).getByTestId('lang-badge').textContent).toContain('🇷🇺 RU')
  })

  it('событие lead_language доходит до «второй вкладки» live (WS, без перезагрузки)', async () => {
    // «Другая вкладка» — actor по REST; наш UI узнаёт ТОЛЬКО из WS.
    const flip = await rawApi(`/api/leads/${leadId}/language`, actorToken, {
      method: 'PATCH',
      body: { language: 'en' },
    })
    expect(flip.status).toBe(200)
    await waitFor(
      () => expect(screen.getByTestId('lang-badge-modal').textContent).toContain('🇬🇧 EN'),
      { timeout: 8000 },
    )
    expect(screen.getByTestId('lang-select').value).toBe('en')

    // Идемпотентный повтор — 200 (критерий 4, ручка).
    const again = await rawApi(`/api/leads/${leadId}/language`, actorToken, {
      method: 'PATCH',
      body: { language: 'en' },
    })
    expect(again.status).toBe(200)
    // Мусор — 400; стёртого лида нет — 404 закрыты contract-тестами ручки.
  })
})
