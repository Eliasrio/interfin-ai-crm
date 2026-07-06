// LgpdPanel — erasure и export (задача M10-5, SRS §9). Панель видят только
// роли admin/manager; это UI-гейт, настоящий — RequireRole на бэкенде (M8):
// чужой токен получит 403 независимо от того, что нарисовал клиент.
import { useState } from 'react'
import * as api from '../lib/api.js'
import * as store from '../lib/store.js'

export const LGPD_ROLES = ['admin', 'manager']

export default function LgpdPanel({ leadId, role }) {
  const [busy, setBusy] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [error, setError] = useState(null)

  if (!LGPD_ROLES.includes(role)) return null

  async function doExport() {
    setBusy(true)
    setError(null)
    try {
      const data = await api.lgpdExport(leadId)
      downloadJSON(data, `lead-${leadId}-lgpd-export.json`)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  // Erasure необратим (§9.1) — кнопка требует второго клика-подтверждения.
  async function doErase() {
    if (!confirming) {
      setConfirming(true)
      return
    }
    setBusy(true)
    setError(null)
    try {
      await api.lgpdErase(leadId)
      // Лид обезличен и soft-deleted: с доски он исчезает (список M8 стёртых
      // не отдаёт). payment_events остаются в БД (CLAUDE.md §4.8).
      store.removeLead(leadId)
      store.pushAlert('ok', `LGPD: лид #${leadId} стёрт (фин. записи сохранены §9.3)`)
      setConfirming(false)
    } catch (err) {
      setError(err.message)
      setConfirming(false)
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="modal-section lgpd" aria-label="LGPD">
      <h3>LGPD</h3>
      <div className="stage-buttons">
        <button className="btn" disabled={busy} onClick={doExport}>
          ⬇ Export данных (§9.1)
        </button>
        <button className="btn btn-danger" disabled={busy} onClick={doErase}>
          {confirming ? 'Точно стереть? Это необратимо' : '🗑 Erasure — право на забвение'}
        </button>
        {confirming && (
          <button className="btn btn-ghost" disabled={busy} onClick={() => setConfirming(false)}>
            Отмена
          </button>
        )}
      </div>
      {error && <div className="form-error">{error}</div>}
      <div className="muted">Финансовые записи хранятся 5 лет и не стираются (§9.3).</div>
    </section>
  )
}

function downloadJSON(data, filename) {
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
