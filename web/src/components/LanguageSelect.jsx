// LanguageSelect — язык клиента в модалке лида (M14): бейдж (live по WS
// lead_language, стейт из store через пропс lead) + выпадающий выбор
// ru/en/es → PATCH /api/leads/:id/language, disabled на время запроса;
// ошибка — тостом. Язык не определён (null) — бейджа нет, в селекте
// placeholder «язык не определён».
import { useState } from 'react'
import * as api from '../lib/api.js'
import * as store from '../lib/store.js'
import { LANGUAGES, languageBadge } from '../lib/language.js'

export default function LanguageSelect({ lead }) {
  const [busy, setBusy] = useState(false)

  const onChange = async (e) => {
    const language = e.target.value
    if (busy || !language || language === lead.language) return
    setBusy(true)
    try {
      const { lead: fresh } = await api.patchLanguage(lead.id, language)
      store.applyLeads([fresh]) // WS-событие продублирует — merge идемпотентен
    } catch (err) {
      store.pushAlert('error', `Смена языка лида #${lead.id}: ${err.message}`, lead.id)
    } finally {
      setBusy(false)
    }
  }

  return (
    <span className="lang-select" data-testid="lang-bar">
      {languageBadge(lead.language) && (
        <span className="badge" data-testid="lang-badge-modal" title="язык клиента">
          {languageBadge(lead.language)}
        </span>
      )}
      <select
        aria-label="Язык клиента"
        data-testid="lang-select"
        value={lead.language ?? ''}
        disabled={busy}
        onChange={onChange}
      >
        <option value="" disabled>
          язык не определён
        </option>
        {LANGUAGES.map((l) => (
          <option key={l.code} value={l.code}>
            {l.badge}
          </option>
        ))}
      </select>
    </span>
  )
}
