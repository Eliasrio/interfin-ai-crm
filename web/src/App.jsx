// App — корень: сессия есть → доска + WS, нет → login-форма (задача M10-1).
import { useEffect, useState } from 'react'
import * as api from './lib/api.js'
import * as auth from './lib/auth.js'
import * as store from './lib/store.js'
import { KanbanSocket } from './lib/socket.js'
import { useClaims, useStore } from './hooks.js'
import LoginForm from './components/LoginForm.jsx'
import Board from './components/Board.jsx'
import LeadModal from './components/LeadModal.jsx'
import SettingsModal from './components/SettingsModal.jsx'
import Toasts from './components/Toasts.jsx'
import ConnectionBadge from './components/ConnectionBadge.jsx'

export default function App() {
  const claims = useClaims()
  const { alerts, connection } = useStore()
  const [selectedLeadId, setSelectedLeadId] = useState(null)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const authed = Boolean(claims)

  // Сокет живёт, пока жива сессия. Ключ — authed (bool), не сами claims:
  // каждый тихий refresh выпускает новый токен, пересоздавать WS из-за
  // этого нельзя (клиент и так переживает 4001 реконнектом §10.3).
  useEffect(() => {
    if (!authed) return undefined
    const socket = new KanbanSocket()
    socket.start()
    return () => {
      socket.stop()
      store.reset()
      setSelectedLeadId(null)
    }
  }, [authed])

  if (!authed) return <LoginForm />

  return (
    <div className="app">
      <header className="topbar">
        <h1>Interfin AI-CRM — Kanban</h1>
        <div className="topbar-right">
          <ConnectionBadge mode={connection} />
          <span className="whoami">
            {claims.role} #{claims.sub}
          </span>
          {claims.role === 'admin' && (
            <button className="btn btn-ghost" onClick={() => setSettingsOpen(true)}>
              ⚙ Настройки
            </button>
          )}
          <button className="btn btn-ghost" onClick={() => auth.logout()}>
            Выйти
          </button>
        </div>
      </header>
      <Board onOpenLead={setSelectedLeadId} onMoveLead={moveLead} />
      {selectedLeadId != null && (
        <LeadModal
          leadId={selectedLeadId}
          role={claims.role}
          onClose={() => setSelectedLeadId(null)}
          onMoveLead={moveLead}
        />
      )}
      {settingsOpen && <SettingsModal onClose={() => setSettingsOpen(false)} />}
      <Toasts alerts={alerts} onDismiss={store.dismissAlert} />
    </div>
  )
}

// moveLead — единая точка ручной смены стадии: и drag-and-drop (задача
// M10-2), и кнопки Stage 5/6/7 в карточке (задача M10-4). Оптимистично
// двигаем, PATCH подтверждает; 400/409 — откат и объяснение (§4.2).
// Экспортирован: компонентные тесты дёргают его без монтирования App.
export async function moveLead(leadId, toStage) {
  const lead = store.getState().leadsById[leadId]
  if (!lead || lead.stage_id === toStage) return
  const prevStage = store.moveLead(leadId, toStage)
  try {
    const { lead: fresh } = await api.patchStage(leadId, toStage)
    store.applyLeads([fresh])
  } catch (err) {
    if (prevStage != null) store.moveLead(leadId, prevStage)
    if (err instanceof api.ApiError && err.code === 'ERR_STAGE_CONFLICT') {
      // 409: стадию сменили конкурентно — показываем актуальную с бэка.
      try {
        const { lead: fresh } = await api.fetchLead(leadId)
        store.applyLeads([fresh])
      } catch {
        /* не дотянулись — придёт с ближайшим событием/поллингом */
      }
    }
    store.pushAlert('error', `Переход лида #${leadId}: ${err.message}`, leadId)
  }
}
