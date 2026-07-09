// modebar.test.jsx — M13: индикатор режима и кнопки «Взять в работу» /
// «Вернуть Эмме» в карточке (компонент ModeBar).
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import ModeBar from './ModeBar.jsx'
import * as store from '../lib/store.js'
import * as auth from '../lib/auth.js'
import { makeJWT, makeLead, mockFetch } from '../test/helpers.js'

afterEach(() => {
  cleanup()
  store.reset()
  auth.logout()
})

async function setup(lead, patchReply) {
  mockFetch([
    { match: (u) => u === '/auth/login', reply: { body: { access_token: makeJWT('manager'), expires_in: 900 } } },
  ])
  await auth.login('m@x', 'pw')
  store.applyLeads([lead])
  const fetch = mockFetch([
    { match: (u, o) => u === `/api/leads/${lead.id}/mode` && o.method === 'PATCH', reply: patchReply },
  ])
  const view = render(<ModeBar lead={lead} />)
  return { fetch, view }
}

describe('ModeBar', () => {
  it('режим bot: 🤖 + кнопка «Взять в работу», «Вернуть Эмме» нет', async () => {
    await setup(makeLead({ id: 1 }), { body: {} })
    expect(screen.getByTestId('mode-label').textContent).toContain('ведёт Эмма')
    expect(screen.getByText('✋ Взять в работу')).toBeInTheDocument()
    expect(screen.queryByText('🤖 Вернуть Эмме')).toBeNull()
  })

  it('режим human: ✋ с номером менеджера + кнопка возврата', async () => {
    await setup(makeLead({ id: 1, dialog_mode: 'human', taken_by: 7 }), { body: {} })
    expect(screen.getByTestId('mode-label').textContent).toContain('менеджер')
    expect(screen.getByTestId('mode-label').textContent).toContain('#7')
    expect(screen.getByText('🤖 Вернуть Эмме')).toBeInTheDocument()
    expect(screen.queryByText('✋ Взять в работу')).toBeNull()
  })

  it('пауза автопилота: ⏸ и обе кнопки (взять или разбудить Эмму)', async () => {
    const until = new Date(Date.now() + 30 * 60_000).toISOString()
    await setup(makeLead({ id: 1, bot_silenced_until: until }), { body: {} })
    expect(screen.getByTestId('mode-label').textContent).toContain('на паузе до')
    expect(screen.getByText('✋ Взять в работу')).toBeInTheDocument()
    expect(screen.getByText('🤖 Вернуть Эмме')).toBeInTheDocument()
  })

  it('«Взять в работу» зовёт PATCH mode=human и обновляет store ответом', async () => {
    const fresh = makeLead({ id: 1, dialog_mode: 'human', taken_by: 1 })
    const { fetch } = await setup(makeLead({ id: 1 }), { body: { lead: fresh } })
    fireEvent.click(screen.getByText('✋ Взять в работу'))
    await waitFor(() => {
      const call = fetch.calls.find((c) => c.url === '/api/leads/1/mode')
      expect(call).toBeTruthy()
      expect(JSON.parse(call.opts.body)).toEqual({ mode: 'human' })
    })
    await waitFor(() => expect(store.getState().leadsById[1].dialog_mode).toBe('human'))
  })

  it('ошибка PATCH — тост, режим не меняется', async () => {
    await setup(makeLead({ id: 1 }), { status: 404, body: { error: 'лид не найден', code: 'ERR_NOT_FOUND' } })
    fireEvent.click(screen.getByText('✋ Взять в работу'))
    await waitFor(() => expect(store.getState().alerts.some((a) => a.kind === 'error')).toBe(true))
    expect(store.getState().leadsById[1].dialog_mode).toBe('bot')
  })

  it('индикатор меняется live по событию dialog_mode (перерисовка от store)', async () => {
    const lead = makeLead({ id: 1 })
    await setup(lead, { body: {} })
    store.applyEvent({
      type: 'dialog_mode',
      lead_id: 1,
      mode: 'human',
      taken_by: 3,
      ts: new Date().toISOString(),
    })
    // ModeBar рисует пропс lead — родитель (LeadModal) берёт его из store;
    // здесь перерисовываем с обновлённым лидом, как это делает useStore.
    cleanup()
    render(<ModeBar lead={store.getState().leadsById[1]} />)
    expect(screen.getByTestId('mode-label').textContent).toContain('менеджер')
  })
})
