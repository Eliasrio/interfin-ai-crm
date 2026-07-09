// SettingsModal — «Настройки» (M13, видна только admin — кнопку рисует App;
// настоящая проверка ролей — RequireRole(admin) на PATCH /api/settings).
// Три интервала контура takeover, сохранение через PATCH; значения в
// минутах 1..1440 — валидацию дублирует бэкенд (400 при мусоре).
import { useEffect, useState } from 'react'
import * as api from '../lib/api.js'
import * as store from '../lib/store.js'

// FIELDS — известные UI ключи M13; порядок = порядок строк формы.
const FIELDS = [
  {
    key: 'takeover.hybrid_pause_minutes',
    label: 'Пауза Эммы после реплики менеджера (мин)',
    hint: 'Ответили клиенту из карточки — Эмма молчит столько минут, потом включается сама.',
  },
  {
    key: 'takeover.reminder_minutes',
    label: 'Напоминание об ожидающем клиенте (мин)',
    hint: 'Клиент написал, Эмма молчит (диалог у менеджера/пауза) — через столько минут придёт напоминание.',
  },
  {
    key: 'takeover.pickup_minutes',
    label: 'Подхват Эммой после напоминания (мин)',
    hint: 'Менеджер так и не ответил — ещё через столько минут Эмма подхватит диалог сама.',
  },
]

export default function SettingsModal({ onClose }) {
  const [values, setValues] = useState(null) // {key: строка из input}
  const [error, setError] = useState(null)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    let alive = true
    api
      .fetchSettings()
      .then(({ settings }) => {
        if (!alive) return
        const v = {}
        for (const f of FIELDS) v[f.key] = String(settings[f.key] ?? '')
        setValues(v)
      })
      .catch((err) => alive && setError(err.message))
    return () => {
      alive = false
    }
  }, [])

  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const valid =
    values && FIELDS.every((f) => /^\d+$/.test(values[f.key].trim()) && +values[f.key] >= 1 && +values[f.key] <= 1440)

  const save = async () => {
    if (!valid || saving) return
    setSaving(true)
    setError(null)
    try {
      const body = {}
      for (const f of FIELDS) body[f.key] = Number(values[f.key])
      await api.patchSettings(body)
      store.pushAlert('ok', 'Настройки сохранены')
      onClose()
    } catch (err) {
      setError(err.message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal modal-settings" role="dialog" aria-label="Настройки" onClick={(e) => e.stopPropagation()}>
        <header className="modal-head">
          <h2>Настройки</h2>
          <button className="btn btn-ghost" onClick={onClose} aria-label="Закрыть">
            ✕
          </button>
        </header>
        <section className="modal-section">
          <h3>Перехват диалога (Эмма ↔ менеджер)</h3>
          {error && <div className="form-error">{error}</div>}
          {!values && !error && <div className="muted">Загружаем…</div>}
          {values &&
            FIELDS.map((f) => (
              <label key={f.key} className="settings-field">
                <span>{f.label}</span>
                <input
                  aria-label={f.label}
                  inputMode="numeric"
                  value={values[f.key]}
                  onChange={(e) => setValues({ ...values, [f.key]: e.target.value })}
                />
                <small className="muted">{f.hint}</small>
              </label>
            ))}
          {values && !valid && <div className="form-error">Интервалы — целые минуты от 1 до 1440.</div>}
          <button className="btn btn-primary" disabled={!valid || saving} onClick={save}>
            {saving ? 'Сохраняем…' : 'Сохранить'}
          </button>
        </section>
      </div>
    </div>
  )
}
