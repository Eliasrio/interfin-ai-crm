// emmascenario.test.jsx — EP-07: вкладка «Сценарий» — общий паттерн
// «баннер несохранённых → Применить → PATCH → тост», инвариант кнопки.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import EmmaScenarioTab from './EmmaScenarioTab.jsx'
import * as store from '../lib/store.js'
import { mockFetch } from '../test/helpers.js'

afterEach(() => {
  cleanup()
  store.reset()
})

const scenarioBody = (over = {}) => ({
  welcome_text: '',
  manager_button_enabled: false,
  manager_button_text: '',
  handoff_confirm_text: 'Сейчас свяжу вас с менеджером, ожидайте',
  manager_mention: '',
  chat_greeting_text: '',
  ...over,
})

describe('EmmaScenarioTab', () => {
  it('правка welcome → баннер; Применить → PATCH и тост про 30 секунд', async () => {
    const onDirty = vi.fn()
    const fetch = mockFetch([
      {
        match: (u, o) => u === '/api/emma/scenario' && o.method === 'PATCH',
        reply: { body: scenarioBody({ welcome_text: 'Привет!' }) },
      },
      { match: (u) => u === '/api/emma/scenario', reply: { body: scenarioBody() } },
    ])
    render(<EmmaScenarioTab onDirty={onDirty} />)
    const area = await screen.findByLabelText('Приветствие на /start')
    expect(screen.queryByText(/несохранённые изменения/)).toBeNull()

    fireEvent.change(area, { target: { value: 'Привет!' } })
    expect(screen.getByText(/несохранённые изменения/)).toBeInTheDocument()
    expect(onDirty).toHaveBeenLastCalledWith(true)

    fireEvent.click(screen.getByText('Применить'))
    await waitFor(() => expect(screen.queryByText(/несохранённые изменения/)).toBeNull())
    const patch = fetch.calls.find((c) => c.opts.method === 'PATCH')
    expect(JSON.parse(patch.opts.body).welcome_text).toBe('Привет!')
    expect(store.getState().alerts.some((a) => a.text.includes('Эмма подхватит в течение 30 секунд'))).toBe(true)
  })

  it('упоминание менеджера уходит в PATCH', async () => {
    const fetch = mockFetch([
      {
        match: (u, o) => u === '/api/emma/scenario' && o.method === 'PATCH',
        reply: { body: scenarioBody({ manager_mention: 'manager_ivan' }) },
      },
      { match: (u) => u === '/api/emma/scenario', reply: { body: scenarioBody() } },
    ])
    render(<EmmaScenarioTab onDirty={() => {}} />)
    const input = await screen.findByLabelText('Упоминание менеджера')
    fireEvent.change(input, { target: { value: 'manager_ivan' } })
    fireEvent.click(screen.getByText('Применить'))
    await waitFor(() => expect(screen.queryByText(/несохранённые изменения/)).toBeNull())
    const patch = fetch.calls.find((c) => c.opts.method === 'PATCH')
    expect(JSON.parse(patch.opts.body).manager_mention).toBe('manager_ivan')
  })

  it('включённая кнопка менеджера без текста блокирует Применить', async () => {
    mockFetch([{ match: (u) => u === '/api/emma/scenario', reply: { body: scenarioBody() } }])
    render(<EmmaScenarioTab onDirty={() => {}} />)
    const toggle = await screen.findByLabelText('Кнопка «Связаться с менеджером»')
    fireEvent.click(toggle)
    expect(screen.getByText('У включённой кнопки должен быть текст.')).toBeInTheDocument()
    expect(screen.getByText('Применить')).toBeDisabled()

    fireEvent.change(screen.getByLabelText('Текст кнопки менеджера'), { target: { value: '👤 Позвать менеджера' } })
    expect(screen.queryByText('У включённой кнопки должен быть текст.')).toBeNull()
    expect(screen.getByText('Применить')).not.toBeDisabled()
  })
})
