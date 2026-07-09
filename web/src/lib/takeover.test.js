// takeover.test.js — M13: вычисление UI-состояния режима (lib/takeover) и
// обработка новых WS-событий dialog_mode / takeover_reminder стором.
import { afterEach, describe, expect, it } from 'vitest'
import { dialogState } from './takeover.js'
import * as store from './store.js'
import { makeLead } from '../test/helpers.js'

afterEach(() => store.reset())

describe('dialogState', () => {
  it('bot без паузы → «ведёт Эмма»', () => {
    const st = dialogState(makeLead({ dialog_mode: 'bot' }))
    expect(st.state).toBe('bot')
    expect(st.icon).toBe('🤖')
  })

  it('human → «ведёт менеджер» с номером взявшего', () => {
    const st = dialogState(makeLead({ dialog_mode: 'human', taken_by: 5 }))
    expect(st.state).toBe('human')
    expect(st.label).toContain('менеджер')
    expect(st.label).toContain('#5')
  })

  it('bot + пауза в будущем → «на паузе до HH:MM»', () => {
    const now = new Date('2026-07-08T12:00:00Z')
    const until = new Date('2026-07-08T12:30:00Z').toISOString()
    const st = dialogState(makeLead({ dialog_mode: 'bot', bot_silenced_until: until }), now)
    expect(st.state).toBe('paused')
    expect(st.label).toContain('на паузе до')
  })

  it('просроченная пауза = обычный bot (авто-снятие по месту)', () => {
    const now = new Date('2026-07-08T13:00:00Z')
    const until = new Date('2026-07-08T12:30:00Z').toISOString()
    expect(dialogState(makeLead({ bot_silenced_until: until }), now).state).toBe('bot')
  })

  it('human главнее паузы (состояние не смешивается)', () => {
    const until = new Date(Date.now() + 60_000).toISOString()
    const st = dialogState(makeLead({ dialog_mode: 'human', bot_silenced_until: until }))
    expect(st.state).toBe('human')
  })
})

describe('store: события M13', () => {
  it('dialog_mode обновляет режим лида live', () => {
    store.applyLeads([makeLead({ id: 1 })])
    store.applyEvent({
      type: 'dialog_mode',
      lead_id: 1,
      stage_id: 1,
      mode: 'human',
      taken_by: 5,
      ts: new Date().toISOString(),
    })
    const lead = store.getState().leadsById[1]
    expect(lead.dialog_mode).toBe('human')
    expect(lead.taken_by).toBe(5)
    expect(lead.bot_silenced_until).toBeNull()
  })

  it('dialog_mode с reason=takeover_pickup показывает тост о подхвате', () => {
    store.applyLeads([makeLead({ id: 1, name: 'Иван' })])
    store.applyEvent({
      type: 'dialog_mode',
      lead_id: 1,
      mode: 'bot',
      reason: 'takeover_pickup',
      ts: new Date().toISOString(),
    })
    const alerts = store.getState().alerts
    expect(alerts.some((a) => a.text.includes('Эмма подхватила') && a.text.includes('Иван'))).toBe(true)
  })

  it('ручной dialog_mode (без reason) тостов не плодит', () => {
    store.applyLeads([makeLead({ id: 1 })])
    store.applyEvent({ type: 'dialog_mode', lead_id: 1, mode: 'bot', ts: new Date().toISOString() })
    expect(store.getState().alerts).toHaveLength(0)
  })

  it('takeover_reminder → тост «ждёт ответа N минут»', () => {
    store.applyLeads([makeLead({ id: 1, name: 'Иван' })])
    store.applyEvent({
      type: 'takeover_reminder',
      lead_id: 1,
      waiting_minutes: 10,
      ts: new Date().toISOString(),
    })
    const alerts = store.getState().alerts
    expect(alerts).toHaveLength(1)
    expect(alerts[0].text).toContain('Иван')
    expect(alerts[0].text).toContain('10 мин')
  })

  it('неизвестный тип события по-прежнему игнорируется молча', () => {
    store.applyLeads([makeLead({ id: 1 })])
    const before = store.getState().leadsById[1]
    const ok = store.applyEvent({ type: 'm14_unknown_event', lead_id: 1, ts: new Date().toISOString() })
    expect(ok).toBe(true)
    expect(store.getState().leadsById[1]).toEqual(before)
    expect(store.getState().alerts).toHaveLength(0)
  })
})
