// lgpd.test.jsx — критерий приёмки №4 (UI-половина): erasure/export видны
// только ролям admin/manager; вторая половина — RequireRole бэкенда,
// проверяется e2e (системный токен → 403).
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import LgpdPanel from './LgpdPanel.jsx'
import LeadModal from './LeadModal.jsx'
import * as store from '../lib/store.js'
import * as auth from '../lib/auth.js'
import { makeJWT, makeLead, mockFetch } from '../test/helpers.js'

afterEach(() => {
  cleanup()
  store.reset()
  auth.logout()
})

describe('LgpdPanel: ролевой гейт', () => {
  it.each(['admin', 'manager'])('роль %s видит erasure и export', (role) => {
    render(<LgpdPanel leadId={1} role={role} />)
    expect(screen.getByText(/Erasure/)).toBeInTheDocument()
    expect(screen.getByText(/Export/)).toBeInTheDocument()
  })

  it.each(['system', 'viewer', undefined])('роль %s панель не видит', (role) => {
    const { container } = render(<LgpdPanel leadId={1} role={role} />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe('LgpdPanel: действия', () => {
  async function loginManager() {
    mockFetch([{ match: (u) => u === '/auth/login', reply: { body: { access_token: makeJWT('manager'), expires_in: 900 } } }])
    await auth.login('m@x', 'pw')
  }

  it('erasure требует подтверждения, зовёт DELETE и убирает лида с доски', async () => {
    await loginManager()
    store.applyLeads([makeLead({ id: 3 })])
    const fetch = mockFetch([
      {
        match: (u, o) => u === '/api/lgpd/leads/3/erase' && o.method === 'DELETE',
        reply: { body: { status: 'erased', lead_id: 3, financial_records: 'сохранены (§9.3)' } },
      },
    ])
    render(<LgpdPanel leadId={3} role="admin" />)
    fireEvent.click(screen.getByText(/Erasure/))
    // Первый клик — только подтверждение, DELETE ещё не ушёл.
    expect(fetch.calls).toHaveLength(0)
    fireEvent.click(screen.getByText(/Точно стереть/))
    await screen.findByText(/Erasure/) // busy отпустило
    expect(fetch.calls.some((c) => c.url === '/api/lgpd/leads/3/erase')).toBe(true)
    expect(store.getState().leadsById[3]).toBeUndefined()
  })

  it('export зовёт GET и скачивает JSON', async () => {
    await loginManager()
    const fetch = mockFetch([
      {
        match: (u) => u === '/api/lgpd/leads/3/export',
        reply: { body: { lead_id: 3, erased: false, messages: [], payment_events: [], lgpd_audit: [] } },
      },
    ])
    // jsdom не умеет createObjectURL — заглушка.
    let downloaded = null
    globalThis.URL.createObjectURL = (blob) => {
      downloaded = blob
      return 'blob:test'
    }
    globalThis.URL.revokeObjectURL = () => {}
    render(<LgpdPanel leadId={3} role="manager" />)
    fireEvent.click(screen.getByText(/Export/))
    await screen.findByText(/Export/)
    expect(fetch.calls.some((c) => c.url === '/api/lgpd/leads/3/export')).toBe(true)
    expect(downloaded).toBeTruthy()
  })
})

describe('LeadModal', () => {
  it('показывает диалог, платежи и кнопки ручных переходов 5/6/7', async () => {
    mockFetch([
      { match: (u) => u === '/auth/login', reply: { body: { access_token: makeJWT('manager'), expires_in: 900 } } },
    ])
    await auth.login('m@x', 'pw')
    store.applyLeads([makeLead({ id: 8, name: 'Мария', stage_id: 4, manual_resolution: true })])
    mockFetch([
      {
        match: (u) => u === '/api/leads/8',
        reply: {
          body: {
            lead: makeLead({ id: 8, name: 'Мария', stage_id: 4 }),
            messages: [
              { id: 1, direction: 'inbound', content: 'Здравствуйте, хочу консультацию', created_at: '2026-07-06T10:00:00Z' },
              { id: 2, direction: 'outbound', content: 'Добрый день! Подскажите…', created_at: '2026-07-06T10:00:05Z' },
            ],
            payment_events: [
              {
                id: 1, gateway: 'cryptobot', status: 'underpaid',
                amount_due: { Decimal: '100', Valid: true },
                amount_received: { Decimal: '99', Valid: true },
                net_received: { Decimal: '96.03', Valid: true },
                currency: 'USDT', tolerance_ok: false, created_at: '2026-07-06T11:00:00Z',
              },
            ],
          },
        },
      },
    ])
    render(<LeadModal leadId={8} role="manager" onClose={() => {}} onMoveLead={() => {}} />)
    await screen.findByText('Здравствуйте, хочу консультацию')
    expect(screen.getByText('Добрый день! Подскажите…')).toBeInTheDocument()
    expect(screen.getByText('underpaid')).toBeInTheDocument()
    expect(screen.getByText(/вне tolerance/)).toBeInTheDocument() // manual_resolution
    // Кнопки ручных переходов — задача M10-4.
    expect(screen.getByText(/→ 5\./)).toBeInTheDocument()
    expect(screen.getByText(/→ 6\./)).toBeInTheDocument()
    expect(screen.getByText(/→ 7\./)).toBeInTheDocument()
    // TTL-индикатор стадии 4 виден на карточке через бейдж — проверено в LeadCard через Board.
  })
})
