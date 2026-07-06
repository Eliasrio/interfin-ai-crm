// LeadCard — карточка на доске: имя, счётчик сообщений и индикаторы
// TTL / anti-spam / эскалации / платежа (задачи M10-2, M10-4).
import { stageById } from '../lib/stages.js'

export default function LeadCard({ lead, onOpen }) {
  const ttl = ttlBadge(lead)
  return (
    <article
      className="card"
      data-lead-id={lead.id}
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData('text/lead-id', String(lead.id))
        e.dataTransfer.effectAllowed = 'move'
      }}
      onClick={onOpen}
    >
      <div className="card-title">{displayName(lead)}</div>
      <div className="card-meta">
        <span title="inbound-сообщений">💬 {lead.message_count}</span>
        {lead.phone && <span>📞 {lead.phone}</span>}
      </div>
      <div className="card-badges">
        {ttl && (
          <span className={'badge' + (lead.ttlWarning ? ' badge-warn' : '')} title="TTL стадии (§3.4)">
            ⏳ {ttl}
          </span>
        )}
        {(lead.antiSpamAlert || lead.anti_spam_count >= 25) && (
          <span className="badge badge-warn" title="anti-spam лимит §3.5">
            🚫 спам {lead.anti_spam_count}
          </span>
        )}
        {(lead.escalated || lead.escalated_at) && (
          <span className="badge badge-warn" title="эскалация: 48ч без ответа">
            📣 эскалация
          </span>
        )}
        {lead.manual_resolution && (
          <span className="badge badge-warn" title="платёж вне tolerance — ручной разбор">
            ⚠️ ручной разбор
          </span>
        )}
        {lead.lastPayment && (
          <span
            className={'badge ' + (lead.lastPayment.tolerance_ok === false ? 'badge-warn' : 'badge-ok')}
            title="последний платёж (live)"
          >
            💰 {lead.lastPayment.amount} {lead.lastPayment.currency}
          </span>
        )}
      </div>
    </article>
  )
}

export function displayName(lead) {
  return lead.name || (lead.tg_username ? '@' + lead.tg_username : null) || `Лид #${lead.id}`
}

// ttlBadge — остаток TTL стадии от last_activity_at (клиентская подсказка;
// боевой таймер — на бэкенде, CLAUDE.md §4.7).
function ttlBadge(lead) {
  const stage = stageById(lead.stage_id)
  if (!stage?.ttlMs || !lead.last_activity_at) return null
  const left = new Date(lead.last_activity_at).getTime() + stage.ttlMs - Date.now()
  if (left <= 0) return 'истекает'
  const hours = Math.round(left / 3600_000)
  return hours >= 48 ? `${Math.floor(hours / 24)} дн` : `${hours} ч`
}
