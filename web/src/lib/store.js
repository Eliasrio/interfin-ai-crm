// store.js — состояние доски: лиды, режим соединения, лента алертов.
// Без React: снапшот иммутабельный (каждая мутация — новый объект state),
// компоненты подписываются через useSyncExternalStore, e2e-тесты — напрямую.
//
// Схема событий — internal/events/events.go (канонична, §4.3 в SRS нет):
// {type, lead_id, stage_id, old_stage_id?, actor?, anti_spam_count?,
//  amount?, currency?, tolerance_ok?, reason?, ts}.

let state = initialState()
const listeners = new Set()
let alertSeq = 0

function initialState() {
  return {
    leadsById: {},
    // connecting → live | polling | reconnecting; offline — WS и polling мертвы
    connection: 'connecting',
    // lastEventTs — ts последнего события/среза для catch-up §10.3.
    lastEventTs: null,
    alerts: [],
  }
}

export function getState() {
  return state
}

export function subscribe(fn) {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

function commit(next) {
  state = next
  for (const fn of listeners) fn()
}

export function reset() {
  commit(initialState())
}

export function setConnection(mode) {
  if (state.connection === mode) return
  commit({ ...state, connection: mode })
}

// bumpEventTs — lastEventTs только растёт: события могут прийти
// чуть вразнобой, catch-up должен помнить самый поздний момент.
function bumpEventTs(prev, ts) {
  if (!ts) return prev
  if (!prev || new Date(ts) > new Date(prev)) return ts
  return prev
}

// applyLeads — merge среза REST (начальная загрузка, catch-up, polling,
// ответ PATCH). UI-флаги (ttlWarning и пр.) живут поверх DTO и переживают
// merge, но смена стадии гасит ttlWarning — предупреждение было про TTL
// прежней стадии.
export function applyLeads(leads, { sliceTs } = {}) {
  if (!leads.length && !sliceTs) return
  const byId = { ...state.leadsById }
  for (const lead of leads) {
    const prev = byId[lead.id]
    byId[lead.id] = {
      ...prev,
      ...lead,
      ttlWarning: prev && prev.stage_id === lead.stage_id ? prev.ttlWarning : false,
    }
  }
  commit({ ...state, leadsById: byId, lastEventTs: bumpEventTs(state.lastEventTs, sliceTs) })
}

export function removeLead(id) {
  if (!state.leadsById[id]) return
  const byId = { ...state.leadsById }
  delete byId[id]
  commit({ ...state, leadsById: byId })
}

// moveLead — оптимистичное перемещение при drag-and-drop; возвращает
// прежнюю стадию для отката, если PATCH вернёт 400/409.
export function moveLead(id, stageId) {
  const lead = state.leadsById[id]
  if (!lead) return null
  const prevStage = lead.stage_id
  const byId = {
    ...state.leadsById,
    [id]: { ...lead, stage_id: stageId, ttlWarning: false },
  }
  commit({ ...state, leadsById: byId })
  return prevStage
}

export function pushAlert(kind, text, leadId) {
  const alert = { id: ++alertSeq, kind, text, leadId }
  commit({ ...state, alerts: [...state.alerts, alert].slice(-6) })
  return alert.id
}

export function dismissAlert(id) {
  commit({ ...state, alerts: state.alerts.filter((a) => a.id !== id) })
}

// applyEvent — событие crm:events с WS. Возвращает false, если лид доске
// неизвестен (socket дотянет карточку по REST), но lastEventTs двигает в
// любом случае — событие получено, catch-up его перезапрашивать не должен.
export function applyEvent(ev) {
  const lastEventTs = bumpEventTs(state.lastEventTs, ev.ts)
  const lead = state.leadsById[ev.lead_id]
  if (!lead) {
    commit({ ...state, lastEventTs })
    return false
  }

  const upd = { ...lead }
  let alerts = state.alerts
  const addAlert = (kind, text) => {
    alerts = [...alerts, { id: ++alertSeq, kind, text, leadId: ev.lead_id }].slice(-6)
  }
  const who = lead.name || lead.tg_username || `лид #${ev.lead_id}`

  switch (ev.type) {
    case 'stage_change':
      upd.stage_id = ev.stage_id
      upd.lastActor = ev.actor
      upd.ttlWarning = false
      break
    case 'payment_received':
      upd.stage_id = ev.stage_id
      upd.ttlWarning = false
      upd.lastPayment = {
        amount: ev.amount,
        currency: ev.currency,
        tolerance_ok: ev.tolerance_ok,
        ts: ev.ts,
      }
      addAlert(
        ev.tolerance_ok === false ? 'warn' : 'ok',
        `Платёж ${ev.amount} ${ev.currency} — ${who}` +
          (ev.tolerance_ok === false ? ' (вне tolerance — ручной разбор)' : ''),
      )
      break
    case 'antispam_alert':
      upd.anti_spam_count = ev.anti_spam_count
      upd.antiSpamAlert = true
      addAlert('warn', `Anti-spam: ${who} достиг лимита сообщений`)
      break
    case 'manager_escalation':
      upd.escalated = true
      addAlert('warn', `Эскалация: ${who} ждёт ответа менеджера`)
      break
    case 'ttl_warning':
      upd.ttlWarning = true
      addAlert('warn', `TTL скоро истечёт: ${who}`)
      break
    case 'dialog_mode':
      // M13: событие несёт состояние режима целиком.
      upd.dialog_mode = ev.mode
      upd.bot_silenced_until = ev.silenced_until ?? null
      upd.taken_by = ev.taken_by ?? null
      if (ev.reason === 'takeover_pickup') {
        addAlert('warn', `Эмма подхватила диалог: ${who} (менеджер не ответил)`)
      }
      break
    case 'takeover_reminder':
      // M13: клиент ждёт ответа менеджера.
      addAlert('warn', `${who} ждёт ответа ${ev.waiting_minutes} мин — ответьте или Эмма подхватит`)
      break
    case 'lead_language':
      // M14: событие несёт язык целиком (автодетекция или ручная смена).
      upd.language = ev.language || null
      break
    default:
      // Неизвестный тип события — вперёд-совместимость: молча пропускаем.
      commit({ ...state, lastEventTs })
      return true
  }
  // last_activity_at событием не трогаем: у stage_change оно меняется на
  // бэкенде (M5, CAS-переход), точное значение придёт с ближайшим срезом.
  commit({
    ...state,
    leadsById: { ...state.leadsById, [ev.lead_id]: upd },
    alerts,
    lastEventTs,
  })
  return true
}
