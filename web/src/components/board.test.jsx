// board.test.jsx — критерий приёмки №3: drag-and-drop зовёт
// PATCH /api/leads/:id/stage и переставляет карточку; 409 откатывает.
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import Board from './Board.jsx'
import { moveLead } from '../App.jsx'
import * as store from '../lib/store.js'
import * as auth from '../lib/auth.js'
import { makeDataTransfer, makeJWT, makeLead, mockFetch } from '../test/helpers.js'

afterEach(() => {
  cleanup()
  store.reset()
  auth.logout()
})

async function setup(patchReply) {
  mockFetch([{ match: (u) => u === '/auth/login', reply: { body: { access_token: makeJWT('manager'), expires_in: 900 } } }])
  await auth.login('m@x', 'pw')
  store.applyLeads([makeLead({ id: 1, name: 'Иван', stage_id: 2 })])
  const fetch = mockFetch([
    { match: (u, o) => u === '/api/leads/1/stage' && o.method === 'PATCH', reply: patchReply },
    {
      match: (u) => u === '/api/leads/1',
      reply: { body: { lead: makeLead({ id: 1, name: 'Иван', stage_id: 7 }), messages: [], payment_events: [] } },
    },
  ])
  render(<Board onOpenLead={() => {}} onMoveLead={moveLead} />)
  return fetch
}

function columnOf(card) {
  return card.closest('.column').dataset.stageId
}

function dragTo(stageId) {
  const card = screen.getByText('Иван').closest('.card')
  const dt = makeDataTransfer()
  fireEvent.dragStart(card, { dataTransfer: dt })
  const col = document.querySelector(`.column[data-stage-id="${stageId}"]`)
  fireEvent.dragOver(col, { dataTransfer: dt })
  fireEvent.drop(col, { dataTransfer: dt })
}

describe('Kanban DnD', () => {
  it('рисует все 8 колонок §3.1', async () => {
    await setup({ body: {} })
    expect(document.querySelectorAll('.column')).toHaveLength(8)
    expect(screen.getByText(/Оплачено: ждут ссылку/)).toBeInTheDocument()
  })

  it('drop зовёт PATCH и переставляет карточку (оптимистично + подтверждение)', async () => {
    const fetch = await setup({ body: { lead: makeLead({ id: 1, name: 'Иван', stage_id: 5 }) } })
    dragTo(5)
    // Оптимистично карточка уже в колонке 5 — до ответа сервера.
    expect(columnOf(screen.getByText('Иван'))).toBe('5')
    await waitFor(() => {
      const patch = fetch.calls.find((c) => c.url === '/api/leads/1/stage')
      expect(patch).toBeTruthy()
      expect(JSON.parse(patch.opts.body)).toEqual({ stage_id: 5 })
    })
    expect(columnOf(screen.getByText('Иван'))).toBe('5')
  })

  it('400 ERR_INVALID_TRANSITION → откат карточки и алерт', async () => {
    await setup({ status: 400, body: { error: 'переход запрещён таблицей стадий §3.1', code: 'ERR_INVALID_TRANSITION' } })
    dragTo(3)
    await waitFor(() => expect(columnOf(screen.getByText('Иван'))).toBe('2')) // откатилась
    expect(store.getState().alerts.some((a) => a.kind === 'error')).toBe(true)
  })

  it('409 ERR_STAGE_CONFLICT → перечитывает лида с бэка', async () => {
    await setup({ status: 409, body: { error: 'стадия изменена конкурентно', code: 'ERR_STAGE_CONFLICT' } })
    dragTo(5)
    // Бэк сказал 409 и вернул актуальную стадию 7 — доска показывает её.
    await waitFor(() => expect(columnOf(screen.getByText('Иван'))).toBe('7'))
  })
})
