// EmmaPanel — раздел «🤖 Эмма» (EP-07 задача 1): PIN-гейт + 6 вкладок.
// Рисуется вместо доски (App: view='emma'), виден только admin — кнопку
// рисует App; настоящая защита — RequireRole(admin)+RequirePIN на бэкенде.
//
// Любой 401 PIN_REQUIRED из ручек панели (шина lib/emma.js) возвращает на
// PIN-экран: сессия истекает через 30 минут бездействия (ТЗ §2.2).
import { useEffect, useState } from 'react'
import * as emma from '../lib/emma.js'
import EmmaPinGate, { EmmaPinChangeModal } from './EmmaPinGate.jsx'
import EmmaPromptTab from './EmmaPromptTab.jsx'
import EmmaKbTab from './EmmaKbTab.jsx'
import EmmaFilesTab from './EmmaFilesTab.jsx'
import EmmaContactsTab from './EmmaContactsTab.jsx'
import EmmaScenarioTab from './EmmaScenarioTab.jsx'
import EmmaStatsTab from './EmmaStatsTab.jsx'

const TABS = [
  { key: 'prompt', label: 'Промпт', Tab: EmmaPromptTab },
  { key: 'kb', label: 'База знаний', Tab: EmmaKbTab },
  { key: 'files', label: 'Файлы', Tab: EmmaFilesTab },
  { key: 'contacts', label: 'Контакты', Tab: EmmaContactsTab },
  { key: 'scenario', label: 'Сценарий', Tab: EmmaScenarioTab },
  { key: 'stats', label: 'Статистика', Tab: EmmaStatsTab },
]

export default function EmmaPanel({ onExit }) {
  // phase: loading → setup | enter | open; unavailable — Redis лежит (503).
  const [phase, setPhase] = useState('loading')
  const [tab, setTab] = useState('prompt')
  const [dirty, setDirty] = useState(false)
  const [changePin, setChangePin] = useState(false)
  const [exiting, setExiting] = useState(false)

  // Сессия истекла посреди работы — на экран ввода (правки пропадают,
  // но сервер их уже не примет; предупреждать поздно).
  useEffect(
    () =>
      emma.onPinRequired(() => {
        setPhase('enter')
        setDirty(false)
      }),
    [],
  )

  useEffect(() => {
    if (phase !== 'loading') return undefined
    let alive = true
    emma
      .pinStatus()
      .then(({ pin_set, session_active }) => {
        if (!alive) return
        setPhase(!pin_set ? 'setup' : session_active ? 'open' : 'enter')
      })
      .catch(() => {
        // 503 PIN_UNAVAILABLE и любой другой отказ: панель закрыта
        // (fail-closed §2.2), даём кнопку «Повторить».
        if (alive) setPhase('unavailable')
      })
    return () => {
      alive = false
    }
  }, [phase])

  // confirmLeave — «уход со вкладки с правками — confirm» (задача 9).
  const confirmLeave = () => !dirty || window.confirm('Есть несохранённые изменения — уйти без сохранения?')

  const switchTab = (next) => {
    if (next === tab || !confirmLeave()) return
    setDirty(false)
    setTab(next)
  }

  // exit — «Выйти из панели»: DELETE pin/session + возврат на доску
  // (задача 1). Сессию закрыть не вышло (Redis лёг) — всё равно уходим:
  // TTL добьёт её сам, держать владельца в панели незачем.
  const exit = async () => {
    if (!confirmLeave()) return
    setExiting(true)
    try {
      await emma.pinCloseSession()
    } catch {
      /* см. выше */
    }
    onExit()
  }

  if (phase === 'loading') {
    return (
      <div className="emma">
        <div className="emma-gate muted">Загружаем…</div>
      </div>
    )
  }

  if (phase === 'unavailable') {
    return (
      <div className="emma">
        <div className="emma-gate">
          <div className="login-form">
            <h1>Панель Эммы</h1>
            <div className="form-error">Сервис временно недоступен. Попробуйте через минуту.</div>
            <button className="btn btn-primary" onClick={() => setPhase('loading')}>
              Повторить
            </button>
          </div>
        </div>
      </div>
    )
  }

  if (phase === 'setup' || phase === 'enter') {
    return (
      <div className="emma">
        <EmmaPinGate mode={phase} onSuccess={() => setPhase('open')} />
      </div>
    )
  }

  const active = TABS.find((t) => t.key === tab) || TABS[0]
  const ActiveTab = active.Tab
  return (
    <div className="emma">
      <div className="emma-head">
        <h2>🤖 Панель Эммы</h2>
        <nav className="emma-tabs" aria-label="Вкладки панели Эммы">
          {TABS.map((t) => (
            <button
              key={t.key}
              className={'emma-tab' + (t.key === tab ? ' emma-tab-active' : '')}
              onClick={() => switchTab(t.key)}
            >
              {t.label}
            </button>
          ))}
        </nav>
        <div className="emma-actions">
          <button className="btn btn-ghost" onClick={() => setChangePin(true)}>
            Сменить PIN
          </button>
          <button className="btn btn-ghost" disabled={exiting} onClick={exit}>
            Выйти из панели
          </button>
        </div>
      </div>
      <div className="emma-body">
        {/* key: смена вкладки пересоздаёт компонент — каждая вкладка сама
            загружает свои данные при маунте (fallback для WS-статусов). */}
        <ActiveTab key={tab} onDirty={setDirty} />
      </div>
      {changePin && <EmmaPinChangeModal onClose={() => setChangePin(false)} />}
    </div>
  )
}
