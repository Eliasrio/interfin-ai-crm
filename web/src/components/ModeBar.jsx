// ModeBar — индикатор режима диалога над чат-панелью карточки (M13):
// 🤖 ведёт Эмма / ✋ ведёт менеджер / ⏸ Эмма на паузе до HH:MM — live по
// WS-событию dialog_mode (стадию/режим карточка берёт из store).
// Кнопки «Взять в работу» / «Вернуть Эмме» — PATCH /api/leads/:id/mode,
// disabled на время запроса; ошибка — тостом.
import { useState } from 'react'
import * as api from '../lib/api.js'
import * as store from '../lib/store.js'
import { dialogState } from '../lib/takeover.js'

export default function ModeBar({ lead }) {
  const [busy, setBusy] = useState(false)
  const st = dialogState(lead)

  const setMode = async (mode) => {
    if (busy) return
    setBusy(true)
    try {
      const { lead: fresh } = await api.patchMode(lead.id, mode)
      store.applyLeads([fresh]) // WS-событие продублирует — merge идемпотентен
    } catch (err) {
      store.pushAlert('error', `Смена режима лида #${lead.id}: ${err.message}`, lead.id)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className={`mode-bar mode-${st.state}`} data-testid="mode-bar">
      <span className="mode-label" data-testid="mode-label">
        {st.icon} {st.label}
      </span>
      <span className="mode-actions">
        {st.state !== 'human' && (
          <button className="btn" disabled={busy} onClick={() => setMode('human')}>
            ✋ Взять в работу
          </button>
        )}
        {st.state !== 'bot' && (
          <button className="btn" disabled={busy} onClick={() => setMode('bot')}>
            🤖 Вернуть Эмме
          </button>
        )}
      </span>
    </div>
  )
}
