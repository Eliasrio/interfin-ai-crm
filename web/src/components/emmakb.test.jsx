// emmakb.test.jsx — EP-07 задача 10: вкладка «База знаний» — статус строки
// обновляется live по WS-событию emma_kb_status (без refetch), удаление
// с подтверждением «Эмма забудет содержимое файла».
import { afterEach, describe, expect, it } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import EmmaKbTab from './EmmaKbTab.jsx'
import * as emma from '../lib/emma.js'
import * as store from '../lib/store.js'
import { mockFetch } from '../test/helpers.js'

afterEach(() => {
  cleanup()
  store.reset()
})

const kbRow = (over = {}) => ({
  id: 7,
  filename: 'faq.md',
  mime: 'text/markdown',
  size: 2048,
  status: 'pending',
  chunks_count: 0,
  index_error: null,
  created_at: '2026-07-12T09:00:00Z',
  ...over,
})

describe('EmmaKbTab', () => {
  it('WS-событие emma_kb_status переводит строку pending → indexed без refetch', async () => {
    const fetch = mockFetch([{ match: (u) => u === '/api/emma/kb', reply: { body: { items: [kbRow()] } } }])
    render(<EmmaKbTab />)
    await screen.findByText('faq.md')
    expect(screen.getByTestId('kb-status-7').textContent).toContain('индексируется')

    act(() => {
      emma.applyKbEvent({
        type: 'emma_kb_status',
        file_id: 7,
        filename: 'faq.md',
        status: 'indexed',
        chunks: 12,
        ts: '2026-07-12T09:01:00Z',
      })
    })
    expect(screen.getByTestId('kb-status-7').textContent).toContain('проиндексирован (12 чанков)')
    // Ровно один GET — событие само донесло статус, поллинга нет.
    expect(fetch.calls.filter((c) => c.url === '/api/emma/kb')).toHaveLength(1)
  })

  it('событие error показывает текст ошибки индексации', async () => {
    mockFetch([{ match: (u) => u === '/api/emma/kb', reply: { body: { items: [kbRow()] } } }])
    render(<EmmaKbTab />)
    await screen.findByText('faq.md')
    act(() => {
      emma.applyKbEvent({
        type: 'emma_kb_status',
        file_id: 7,
        status: 'error',
        error: 'PDF без текстового слоя',
        ts: '2026-07-12T09:01:00Z',
      })
    })
    expect(screen.getByTestId('kb-status-7').textContent).toContain('ошибка: PDF без текстового слоя')
  })

  it('удаление — двухшаговое подтверждение «Эмма забудет содержимое файла»', async () => {
    const fetch = mockFetch([
      { match: (u) => u === '/api/emma/kb', reply: { body: { items: [kbRow({ status: 'indexed', chunks_count: 3 })] } } },
      // 204 без тела: Response с телом и статусом 204 запрещён спекой fetch.
      { match: (u, o) => u === '/api/emma/kb/7' && o.method === 'DELETE', reply: () => new Response(null, { status: 204 }) },
    ])
    render(<EmmaKbTab />)
    await screen.findByText('faq.md')

    fireEvent.click(screen.getByText('Удалить'))
    expect(screen.getByText(/Эмма забудет содержимое файла/)).toBeInTheDocument()
    expect(fetch.calls.some((c) => c.opts.method === 'DELETE')).toBe(false) // ещё не удаляли

    fireEvent.click(screen.getAllByText('Удалить').pop()) // кнопка внутри подтверждения
    await waitFor(() => expect(screen.queryByText('faq.md')).toBeNull())
    expect(fetch.calls.some((c) => c.url === '/api/emma/kb/7' && c.opts.method === 'DELETE')).toBe(true)
  })
})
