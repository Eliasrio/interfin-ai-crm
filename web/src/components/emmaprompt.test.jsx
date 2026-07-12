// emmaprompt.test.jsx — EP-07 задача 10: вкладка «Промпт» — счётчик токенов
// на кириллице совпадает с сервером, баннер несохранённых изменений,
// локальная блокировка при превышении лимита + серверный PROMPT_TOO_LONG,
// restore с подтверждением «вернутся текст, темы и стиль».
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import EmmaPromptTab from './EmmaPromptTab.jsx'
import * as store from '../lib/store.js'
import { mockFetch } from '../test/helpers.js'

afterEach(() => {
  cleanup()
  store.reset()
})

const promptBody = (over = {}) => ({
  version_id: 3,
  system_prompt: 'абвг',
  forbidden_topics: ['политика'],
  style: 'neutral',
  created_at: '2026-07-10T10:00:00Z',
  token_estimate: 2, // сервер тоже считает len(bytes)/4: 'абвг' = 8 байт
  token_limit: 1200,
  ...over,
})

const emptyHistory = {
  match: (u) => u.startsWith('/api/emma/prompt/history') && !u.includes('/restore'),
  reply: { body: { items: [], total: 0, page: 1 } },
}

describe('EmmaPromptTab', () => {
  it('счётчик на кириллице совпадает с token_estimate сервера', async () => {
    mockFetch([{ match: (u) => u === '/api/emma/prompt', reply: { body: promptBody() } }, emptyHistory])
    render(<EmmaPromptTab onDirty={() => {}} />)
    const counter = await screen.findByTestId('token-counter')
    // ±0: формула одна и та же — UI обязан показать ровно серверную оценку.
    expect(counter.textContent).toContain(`≈${promptBody().token_estimate} / 1200`)
    expect(counter.className).not.toContain('emma-tokens-over')
  })

  it('правка → жёлтый баннер; Применить → PUT, баннер гаснет, тост про 30 секунд', async () => {
    const onDirty = vi.fn()
    const fetch = mockFetch([
      {
        match: (u, o) => u === '/api/emma/prompt' && o.method === 'PUT',
        reply: { body: promptBody({ version_id: 4, system_prompt: 'абвг и ещё' }) },
      },
      { match: (u) => u === '/api/emma/prompt', reply: { body: promptBody() } },
      emptyHistory,
    ])
    render(<EmmaPromptTab onDirty={onDirty} />)
    const area = await screen.findByLabelText('Системный промпт')
    expect(screen.queryByText(/несохранённые изменения/)).toBeNull()

    fireEvent.change(area, { target: { value: 'абвг и ещё' } })
    expect(screen.getByText(/несохранённые изменения/)).toBeInTheDocument()
    expect(onDirty).toHaveBeenLastCalledWith(true)

    fireEvent.click(screen.getByText('Применить'))
    await waitFor(() => expect(screen.queryByText(/несохранённые изменения/)).toBeNull())
    const put = fetch.calls.find((c) => c.url === '/api/emma/prompt' && c.opts.method === 'PUT')
    expect(JSON.parse(put.opts.body)).toEqual({
      system_prompt: 'абвг и ещё',
      forbidden_topics: ['политика'],
      style: 'neutral',
    })
    expect(store.getState().alerts.some((a) => a.text.includes('Эмма подхватит в течение 30 секунд'))).toBe(true)
    expect(onDirty).toHaveBeenLastCalledWith(false)
  })

  it('превышение лимита: счётчик красный, Применить заблокирован локально', async () => {
    mockFetch([
      { match: (u) => u === '/api/emma/prompt', reply: { body: promptBody({ token_limit: 2 }) } },
      emptyHistory,
    ])
    render(<EmmaPromptTab onDirty={() => {}} />)
    const area = await screen.findByLabelText('Системный промпт')
    fireEvent.change(area, { target: { value: 'абвгабвг' } }) // 16 байт = 4 токена > 2
    expect(screen.getByTestId('token-counter').className).toContain('emma-tokens-over')
    expect(screen.getByText('Применить')).toBeDisabled()
  })

  it('обход локальной блокировки ловится сервером — показывается PROMPT_TOO_LONG', async () => {
    mockFetch([
      {
        match: (u, o) => u === '/api/emma/prompt' && o.method === 'PUT',
        reply: {
          status: 400,
          body: { error: 'промпт длиннее лимита токенов', code: 'PROMPT_TOO_LONG', estimate: 5000, limit: 1200 },
        },
      },
      { match: (u) => u === '/api/emma/prompt', reply: { body: promptBody() } },
      emptyHistory,
    ])
    render(<EmmaPromptTab onDirty={() => {}} />)
    const area = await screen.findByLabelText('Системный промпт')
    fireEvent.change(area, { target: { value: 'абвг подлиннее' } })
    fireEvent.click(screen.getByText('Применить'))
    await screen.findByText('Промпт длиннее лимита: ≈5000 токенов при лимите 1200')
  })

  it('restore: подтверждение «вернутся текст, темы и стиль», после — форма из ответа', async () => {
    const fetch = mockFetch([
      {
        match: (u, o) => u === '/api/emma/prompt/history/2/restore' && o.method === 'POST',
        reply: {
          body: promptBody({ version_id: 5, system_prompt: 'Старый текст', style: 'formal', forbidden_topics: [] }),
        },
      },
      {
        match: (u) => u.startsWith('/api/emma/prompt/history'),
        reply: {
          body: {
            items: [{ id: 2, created_at: '2026-07-09T09:00:00Z', created_by: 1, preview: 'Старый текст', style: 'formal' }],
            total: 1,
            page: 1,
          },
        },
      },
      { match: (u) => u === '/api/emma/prompt', reply: { body: promptBody() } },
    ])
    render(<EmmaPromptTab onDirty={() => {}} />)
    await screen.findByText('Старый текст') // история загрузилась

    fireEvent.click(screen.getByText('Восстановить'))
    // Первый клик — только подтверждение, POST ещё не ушёл.
    expect(screen.getByText(/Вернутся текст, темы и стиль версии от/)).toBeInTheDocument()
    expect(fetch.calls.some((c) => c.url.includes('/restore'))).toBe(false)

    fireEvent.click(screen.getByText('Да, восстановить'))
    await waitFor(() => expect(screen.getByLabelText('Системный промпт').value).toBe('Старый текст'))
    expect(fetch.calls.some((c) => c.url === '/api/emma/prompt/history/2/restore')).toBe(true)
  })
})
