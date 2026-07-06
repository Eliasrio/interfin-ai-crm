// kanban.e2e.test.jsx — критерии приёмки M10 против ЖИВОГО бэкенда
// (docker compose: postgres + redis + app; данные — scripts/m10_e2e_setup.sh):
//
//   1. IQ-10: смена стадии на бэке → карточка в UI < 500 мс (WS push);
//   2. AQ²-5: реконнект WS не теряет события (catch-up ?updated_since);
//   3. drag-and-drop → PATCH /api/leads/:id/stage (живой контракт);
//   4. LGPD erasure/export — только соответствующим ролям (RequireRole).
//
// UI настоящий (React 18 в jsdom), WebSocket настоящий (пакет ws), fetch
// настоящий (undici) с мини-jar для refresh-cookie — undici куки не хранит.
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { WebSocket as NodeWebSocket } from 'ws'
import { createSign } from 'node:crypto'
import { readFileSync } from 'node:fs'
import React from 'react'

import App from '../src/App.jsx'
import * as auth from '../src/lib/auth.js'
import * as store from '../src/lib/store.js'
import { KanbanSocket } from '../src/lib/socket.js'
import { makeDataTransfer } from '../src/test/helpers.js'

const BASE = process.env.E2E_BASE
const UI_EMAIL = 'e2e-m10-ui@interfin.com'
const ACTOR_EMAIL = 'e2e-m10-actor@interfin.com'
const PASSWORD = 'e2e-m10-pass'
// Лид ищется по имени: telegram_user_id у setup-скрипта уникален на каждый
// прогон (повторный LGPD-erase одного tg id бьётся об UNIQUE — хеш §9.3
// детерминированный).
const LEAD_NAME = 'M10 E2E Лид'
const JWT_KEY_PATH = process.env.E2E_JWT_KEY || '../secrets/jwt_private.pem'

const realFetch = globalThis.fetch
let leadId // id тестового лида — узнаём по имени через API
let actorToken // токен второго менеджера: «смена стадии на бэке», не наш UI

// --- fetch с cookie-jar (refresh-cookie Path=/auth) и абсолютными URL ---
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

// rawLogin/rawApi — вторая сессия (актор) в обход модуля auth: у того токен
// одной сессии в памяти, а нам нужны два независимых менеджера.
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

async function waitUntil(cond, what, timeout = 8000, step = 10) {
  const t0 = Date.now()
  while (!(await cond())) {
    if (Date.now() - t0 > timeout) throw new Error('timeout: ' + what)
    await new Promise((r) => setTimeout(r, step))
  }
}

// signJWT — токен с ролью system: учётки system в managers не бывает (M7),
// для проверки RequireRole подписываем сами dev-ключом бэкенда.
function signJWT(role, sub) {
  const pem = readFileSync(JWT_KEY_PATH)
  const b64u = (s) => Buffer.from(s).toString('base64url')
  const now = Math.floor(Date.now() / 1000)
  const header = b64u(JSON.stringify({ alg: 'RS256', typ: 'JWT' }))
  const payload = b64u(JSON.stringify({ sub, role, iat: now, exp: now + 300 }))
  const sign = createSign('RSA-SHA256')
  sign.update(header + '.' + payload)
  return `${header}.${payload}.${sign.sign(pem).toString('base64url')}`
}

describe.skipIf(!BASE)('M10 приёмка (живой бэкенд)', () => {
  beforeAll(async () => {
    globalThis.fetch = cookieFetch
    globalThis.WebSocket = NodeWebSocket
    actorToken = await rawLogin(ACTOR_EMAIL)
    const { body } = await rawApi('/api/leads?limit=200&offset=0', actorToken)
    const lead = body.leads.find((l) => l.name === LEAD_NAME)
    if (!lead) throw new Error('нет тестового лида — запусти scripts/m10_e2e_setup.sh')
    leadId = lead.id
    if (lead.stage_id !== 2) throw new Error('лид не на стадии 2 — перезапусти setup')
  })

  afterAll(() => {
    cleanup()
    auth.logout()
    globalThis.fetch = realFetch
  })

  it('IQ-10: login через форму, смена стадии на бэке → карточка в UI < 500 мс', async () => {
    render(<App />)

    // Логин настоящей формой (задача M10-1): POST /auth/login, токен в память.
    fireEvent.change(await screen.findByLabelText(/Email/), { target: { value: UI_EMAIL } })
    fireEvent.change(screen.getByLabelText(/Пароль/), { target: { value: PASSWORD } })
    fireEvent.click(screen.getByRole('button', { name: /Войти/ }))

    // Доска загрузилась по REST, WS дошёл до live.
    await waitFor(() => expect(document.querySelectorAll('.column')).toHaveLength(8), { timeout: 10_000 })
    const card = await screen.findByText(LEAD_NAME, {}, { timeout: 10_000 })
    expect(card.closest('.column').dataset.stageId).toBe('2')
    await waitFor(
      () => expect(document.querySelector('[data-connection]').dataset.connection).toBe('live'),
      { timeout: 10_000 },
    )

    // «Смена стадии на бэке» — другой менеджер, другая сессия: наш клиент
    // узнаёт о ней ТОЛЬКО из WS-события (state machine M5 → Redis → Hub).
    const t0 = performance.now()
    const patched = await rawApi(`/api/leads/${leadId}/stage`, actorToken, {
      method: 'PATCH',
      body: { stage_id: 5 },
    })
    expect(patched.status).toBe(200)
    await waitFor(
      () => expect(screen.getByText(LEAD_NAME).closest('.column').dataset.stageId).toBe('5'),
      { timeout: 5000, interval: 5 },
    )
    const ms = performance.now() - t0
    // IQ-10: < 500 мс, включая сам PATCH-роундтрип.
    expect(ms).toBeLessThan(500)
    // eslint-disable-next-line no-console
    console.log(`IQ-10: бэкенд → карточка в UI за ${ms.toFixed(1)} мс`)
  })

  it('DnD: перетаскивание карточки зовёт живой PATCH и двигает её', async () => {
    // Продолжаем сессию UI-менеджера из предыдущего теста (лид на стадии 5).
    const card = screen.getByText(LEAD_NAME).closest('.card')
    const dt = makeDataTransfer()
    fireEvent.dragStart(card, { dataTransfer: dt })
    const col7 = document.querySelector('.column[data-stage-id="7"]')
    fireEvent.dragOver(col7, { dataTransfer: dt })
    fireEvent.drop(col7, { dataTransfer: dt })

    // Карточка в колонке 7 сразу (оптимистичное перемещение)…
    expect(screen.getByText(LEAD_NAME).closest('.column').dataset.stageId).toBe('7')
    // …а подтверждение ждём от БЭКА: стадия в БД сменилась — PATCH дошёл
    // (проверка DOM сразу после drop видела бы оптимистичный сдвиг даже
    // при упавшем PATCH).
    await waitUntil(
      async () => (await rawApi(`/api/leads/${leadId}/stage`, actorToken)).body.stage_id === 7,
      'PATCH подтверждён бэком',
      5000,
      100,
    )
    // Отката не случилось — карточка осталась в 7.
    expect(screen.getByText(LEAD_NAME).closest('.column').dataset.stageId).toBe('7')
    cleanup() // App размонтирован — WS следующему тесту не мешает
    store.reset()
  })

  it('AQ²-5: обрыв WS не теряет события — catch-up ?updated_since при реконнекте', async () => {
    await auth.login(UI_EMAIL, PASSWORD)
    const socket = new KanbanSocket({
      wsUrl: BASE.replace(/^http/, 'ws') + '/ws/kanban',
      reconnectBaseMs: 2500, // окно «клиент отвалился»: успеваем сменить стадию
    })
    try {
      await socket.start()
      await waitUntil(() => store.getState().connection === 'live', 'WS live')
      expect(store.getState().leadsById[leadId].stage_id).toBe(7)

      // Обрыв соединения. terminate() (ws-специфичный) рвёт TCP мгновенно,
      // как реальный обрыв сети: close() делал бы вежливый handshake, и
      // событие успевало бы доехать по полузакрытому сокету — тест ничего
      // не доказывал бы. Redis pub/sub fire-and-forget: событие, изданное
      // сейчас, наш клиент из WS уже НЕ получит никогда.
      socket.ws.terminate()
      await waitUntil(() => store.getState().connection !== 'live', 'обрыв замечен')

      const patched = await rawApi(`/api/leads/${leadId}/stage`, actorToken, {
        method: 'PATCH',
        body: { stage_id: 6 }, // 7→6: manager может любой переход (§3.1)
      })
      expect(patched.status).toBe(200)
      // Событие издано, пока WS закрыт — потерян именно WS-путь.
      expect(store.getState().connection).not.toBe('live')
      expect(store.getState().leadsById[leadId].stage_id).toBe(7) // ещё старая

      // Реконнект §10.3: refresh → GET ?updated_since → resubscribe.
      await waitUntil(() => store.getState().leadsById[leadId].stage_id === 6, 'catch-up добрал событие')
      await waitUntil(() => store.getState().connection === 'live', 'снова live')
    } finally {
      socket.stop()
      store.reset()
      auth.logout()
    }
  })

  it('LGPD: export/erase живут за RequireRole — system получает 403, без токена 401', async () => {
    const exportPath = `/api/lgpd/leads/${leadId}/export`
    // Без токена — 401 в формате §4.2.
    const anon = await realFetch(new URL(exportPath, BASE))
    expect(anon.status).toBe(401)
    const anonBody = await anon.json()
    expect(anonBody.code).toMatch(/^ERR_/)

    // Роль system подписана НАСТОЯЩИМ ключом бэкенда, но в списке ролей
    // /api её нет (§5.2: admin и manager перечислены явно) — 403.
    const systemToken = signJWT('system', '0')
    for (const path of ['/api/leads?limit=1&offset=0', exportPath]) {
      const res = await rawApi(path, systemToken)
      expect(res.status).toBe(403)
      expect(res.body.code).toBe('ERR_FORBIDDEN')
    }

    // Менеджер — полный LGPD-доступ: export отдаёт данные и след аудита.
    const managerToken = await rawLogin(UI_EMAIL)
    const exported = await rawApi(exportPath, managerToken)
    expect(exported.status).toBe(200)
    expect(exported.body.lead_id).toBe(leadId)
    expect(exported.body.financial_records).toBeTruthy()
    expect(exported.body.lgpd_audit.some((a) => a.action === 'export')).toBe(true)

    // Erasure (последним — лид исчезает): 200 + пометка §9.3, затем 404.
    const erased = await rawApi(`/api/lgpd/leads/${leadId}/erase`, managerToken, { method: 'DELETE' })
    expect(erased.status).toBe(200)
    expect(erased.body.status).toBe('erased')
    expect(erased.body.financial_records).toBeTruthy()
    const gone = await rawApi(`/api/leads/${leadId}`, managerToken)
    expect(gone.status).toBe(404)
  })
})
