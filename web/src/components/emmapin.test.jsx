// emmapin.test.jsx — EP-07 задача 10: PIN-гейт панели Эммы — все четыре
// состояния (установка / ввод / сразу вкладки / блокировка с таймером),
// недоступность контура (503) и возврат на PIN-экран по 401 PIN_REQUIRED.
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import EmmaPanel from './EmmaPanel.jsx'
import * as store from '../lib/store.js'
import * as auth from '../lib/auth.js'
import { mockFetch } from '../test/helpers.js'

afterEach(() => {
  cleanup()
  store.reset()
  auth.logout()
})

const statusReply = (pinSet, sessionActive) => ({ body: { pin_set: pinSet, session_active: sessionActive } })

// Открытая панель сразу монтирует вкладку «Промпт» — её ручки нужны везде,
// где гейт пройден.
const promptBody = {
  version_id: 1,
  system_prompt: 'Ты — Эмма.',
  forbidden_topics: [],
  style: 'neutral',
  created_at: '2026-07-12T10:00:00Z',
  token_estimate: 4,
  token_limit: 1200,
}
const tabRoutes = [
  { match: (u) => u === '/api/emma/prompt', reply: { body: promptBody } },
  { match: (u) => u.startsWith('/api/emma/prompt/history'), reply: { body: { items: [], total: 0, page: 1 } } },
]

describe('EmmaPanel: PIN-гейт', () => {
  it('PIN не задан → экран установки: два поля, совпадение, ровно 6 цифр', async () => {
    const fetch = mockFetch([
      { match: (u) => u === '/api/emma/pin/status', reply: statusReply(false, false) },
      {
        match: (u, o) => u === '/api/emma/pin/setup' && o.method === 'POST',
        reply: { body: { pin_set: true, session_active: true } },
      },
      ...tabRoutes,
    ])
    render(<EmmaPanel onExit={() => {}} />)
    await screen.findByText('Установите PIN')

    const submit = screen.getByText('Установить PIN')
    fireEvent.change(screen.getByLabelText('PIN'), { target: { value: '123456' } })
    expect(submit).toBeDisabled() // второе поле пустое

    fireEvent.change(screen.getByLabelText('Повторите PIN'), { target: { value: '654321' } })
    expect(screen.getByText('PIN-коды не совпадают.')).toBeInTheDocument()
    expect(submit).toBeDisabled()

    fireEvent.change(screen.getByLabelText('Повторите PIN'), { target: { value: '123456' } })
    fireEvent.click(submit)
    await screen.findByText('История версий') // вкладки открылись
    const call = fetch.calls.find((c) => c.url === '/api/emma/pin/setup')
    expect(JSON.parse(call.opts.body)).toEqual({ pin: '123456' })
  })

  it('PIN задан, сессии нет → экран ввода; верный PIN открывает вкладки', async () => {
    mockFetch([
      { match: (u) => u === '/api/emma/pin/status', reply: statusReply(true, false) },
      {
        match: (u, o) => u === '/api/emma/pin/verify' && o.method === 'POST',
        reply: { body: { session_active: true } },
      },
      ...tabRoutes,
    ])
    render(<EmmaPanel onExit={() => {}} />)
    await screen.findByText('Введите PIN')
    fireEvent.change(screen.getByLabelText('PIN'), { target: { value: '123456' } })
    fireEvent.click(screen.getByText('Войти'))
    await screen.findByText('История версий')
  })

  it('сессия активна → сразу вкладки, PIN-экран не показывается', async () => {
    mockFetch([{ match: (u) => u === '/api/emma/pin/status', reply: statusReply(true, true) }, ...tabRoutes])
    render(<EmmaPanel onExit={() => {}} />)
    await screen.findByText('История версий')
    expect(screen.queryByText('Введите PIN')).toBeNull()
    expect(screen.queryByText('Установите PIN')).toBeNull()
  })

  it('неверный PIN → понятный русский текст', async () => {
    mockFetch([
      { match: (u) => u === '/api/emma/pin/status', reply: statusReply(true, false) },
      {
        match: (u) => u === '/api/emma/pin/verify',
        reply: { status: 401, body: { error: 'неверный PIN', code: 'PIN_INVALID' } },
      },
    ])
    render(<EmmaPanel onExit={() => {}} />)
    await screen.findByText('Введите PIN')
    fireEvent.change(screen.getByLabelText('PIN'), { target: { value: '111111' } })
    fireEvent.click(screen.getByText('Войти'))
    await screen.findByText('Неверный PIN')
  })

  it('PIN_LOCKED → таймер обратного отсчёта из retry_after, поля заблокированы', async () => {
    mockFetch([
      { match: (u) => u === '/api/emma/pin/status', reply: statusReply(true, false) },
      {
        match: (u) => u === '/api/emma/pin/verify',
        reply: { status: 429, body: { error: 'слишком много попыток', code: 'PIN_LOCKED', retry_after: 90 } },
      },
    ])
    render(<EmmaPanel onExit={() => {}} />)
    await screen.findByText('Введите PIN')
    fireEvent.change(screen.getByLabelText('PIN'), { target: { value: '111111' } })
    fireEvent.click(screen.getByText('Войти'))

    const timer = await screen.findByTestId('pin-lock-timer')
    expect(timer.textContent).toContain('1:30')
    expect(screen.getByLabelText('PIN')).toBeDisabled()
    expect(screen.getByText('Войти')).toBeDisabled()

    // Реальная секунда вместо fake-таймеров: vi.useFakeTimers ломает
    // waitFor тестовой библиотеки (внутренний setTimeout тоже фейковый).
    await waitFor(() => expect(screen.getByTestId('pin-lock-timer').textContent).toContain('1:29'), {
      timeout: 3000,
    })
  })

  it('PIN-контур недоступен (Redis лежит) → «Сервис временно недоступен»', async () => {
    mockFetch([
      {
        match: (u) => u === '/api/emma/pin/status',
        reply: { status: 503, body: { error: 'PIN-контур временно недоступен', code: 'PIN_UNAVAILABLE' } },
      },
    ])
    render(<EmmaPanel onExit={() => {}} />)
    await screen.findByText(/Сервис временно недоступен/)
    expect(screen.getByText('Повторить')).toBeInTheDocument()
  })

  it('401 PIN_REQUIRED от ручки вкладки возвращает на PIN-экран (сессия истекла)', async () => {
    mockFetch([
      { match: (u) => u === '/api/emma/pin/status', reply: statusReply(true, true) },
      {
        match: (u) => u === '/api/emma/prompt',
        reply: { status: 401, body: { error: 'требуется PIN', code: 'PIN_REQUIRED' } },
      },
      { match: (u) => u.startsWith('/api/emma/prompt/history'), reply: { body: { items: [], total: 0, page: 1 } } },
    ])
    render(<EmmaPanel onExit={() => {}} />)
    // Гейт пройден по status, но первая же ручка вкладки ответила 401 —
    // панель обязана вернуться на экран ввода PIN.
    await screen.findByText('Введите PIN')
  })
})
