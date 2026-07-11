// LeadModal — карточка лида (задача M10-4): чат менеджера (M12), платежи,
// кнопки ручных переходов Stage 5/6/7 и LGPD-панель (задача M10-5).
// Живёт на GET /api/leads/:id; стадия берётся из store — карточка двигается
// live, даже пока модалка открыта. Диалог — ChatPanel (история + live-чат).
import { useEffect, useState } from 'react'
import * as api from '../lib/api.js'
import { MANUAL_TARGETS, stageById } from '../lib/stages.js'
import { useStore } from '../hooks.js'
import { displayName } from './LeadCard.jsx'
import ChatPanel from './ChatPanel.jsx'
import LanguageSelect from './LanguageSelect.jsx'
import LgpdPanel from './LgpdPanel.jsx'
import ModeBar from './ModeBar.jsx'

export default function LeadModal({ leadId, role, onClose, onMoveLead }) {
  const { leadsById } = useStore()
  const lead = leadsById[leadId]
  const [detail, setDetail] = useState(null) // {messages, payment_events}
  const [error, setError] = useState(null)

  useEffect(() => {
    let alive = true
    api
      .fetchLead(leadId)
      .then((data) => alive && setDetail(data))
      .catch((err) => alive && setError(err.message))
    return () => {
      alive = false
    }
  }, [leadId])

  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  if (!lead) {
    // Лид исчез со доски (erasure/retention) — модалке больше нечего показывать.
    return null
  }
  const stage = stageById(lead.stage_id)

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" role="dialog" aria-label={`Лид #${leadId}`} onClick={(e) => e.stopPropagation()}>
        <header className="modal-head">
          <div>
            <h2>{displayName(lead)}</h2>
            <div className="modal-sub">
              #{lead.id} · стадия {lead.stage_id}. {stage?.title} · 💬 {lead.message_count}
              {lead.tg_username && <> · @{lead.tg_username}</>}
              {lead.phone && <> · {lead.phone}</>}
              {' · '}
              <LanguageSelect lead={lead} />
            </div>
          </div>
          <button className="btn btn-ghost" onClick={onClose} aria-label="Закрыть">
            ✕
          </button>
        </header>

        <section className="modal-section">
          <h3>Ручной переход</h3>
          <div className="stage-buttons">
            {MANUAL_TARGETS.map((sid) => (
              <button
                key={sid}
                className="btn"
                disabled={lead.stage_id === sid}
                onClick={() => onMoveLead(lead.id, sid)}
              >
                → {sid}. {stageById(sid).title}
              </button>
            ))}
          </div>
        </section>

        <section className="modal-section">
          <h3>Платежи</h3>
          <PaymentStatus lead={lead} events={detail?.payment_events} />
        </section>

        <section className="modal-section">
          <h3>Чат</h3>
          <ModeBar lead={lead} />
          {error && <div className="form-error">{error}</div>}
          <ChatPanel lead={lead} />
        </section>

        <LgpdPanel leadId={lead.id} role={role} />
      </div>
    </div>
  )
}

function PaymentStatus({ lead, events }) {
  return (
    <>
      {lead.manual_resolution && (
        <div className="form-error">Платёж вне tolerance (§3.3) — требуется ручной разбор</div>
      )}
      {!events?.length && <div className="muted">Платежей не было</div>}
      {events?.length > 0 && (
        <table className="payments">
          <thead>
            <tr>
              <th>Когда</th>
              <th>Статус</th>
              <th>Сумма</th>
              <th>Net</th>
              <th>Tolerance</th>
            </tr>
          </thead>
          <tbody>
            {events.map((p) => (
              <tr key={p.id}>
                <td>{fmtTs(p.created_at)}</td>
                <td>{p.status ?? '—'}</td>
                <td>
                  {dec(p.amount_received) ?? dec(p.amount_due) ?? '—'} {p.currency ?? ''}
                </td>
                <td>{dec(p.net_received) ?? '—'}</td>
                <td>{p.tolerance_ok == null ? '—' : p.tolerance_ok ? 'ok' : '❌'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}

// dec — decimal.NullDecimal с бэка: {"Decimal":"1.23","Valid":true} либо null.
function dec(v) {
  if (v == null) return null
  if (typeof v === 'object') return v.Valid ? v.Decimal : null
  return String(v)
}

function fmtTs(ts) {
  return new Date(ts).toLocaleString('ru-RU', { dateStyle: 'short', timeStyle: 'short' })
}
