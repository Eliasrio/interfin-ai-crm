// EmmaStatsTab — вкладка «Статистика» (EP-07 задача 8, ТЗ §3 вкладка 6):
// метрики за период, расходы Claude с pricing_note, журнал ошибок с
// фильтром и пагинацией, настройка алертов + пробный алерт.
import { useEffect, useState } from 'react'
import * as emma from '../lib/emma.js'
import * as store from '../lib/store.js'

const PERIODS = [
  ['day', 'День'],
  ['week', 'Неделя'],
  ['month', 'Месяц'],
  ['all', 'Всё время'],
]

const ERROR_KINDS = [
  ['', 'Все типы'],
  ['llm_api', 'Claude API'],
  ['telegram_api', 'Telegram API'],
  ['timeout', 'Таймаут'],
  ['file_not_found', 'Файл не найден'],
  ['kb_index', 'Индексация БЗ'],
]

const kindLabel = (k) => (ERROR_KINDS.find(([key]) => key === k) || [k, k])[1]

function fmtCost(usd) {
  return '≈ $' + (usd >= 0.01 || usd === 0 ? usd.toFixed(2) : usd.toFixed(4))
}

function Stat({ label, value, hint }) {
  return (
    <div className="emma-stat">
      <span className="muted">{label}</span>
      <b>{value}</b>
      {hint && <small className="muted">{hint}</small>}
    </div>
  )
}

export default function EmmaStatsTab() {
  const [period, setPeriod] = useState('day')
  const [stats, setStats] = useState(null)
  const [error, setError] = useState(null)

  const [kind, setKind] = useState('')
  const [journal, setJournal] = useState(null) // {errors, total, page, limit}

  const [alerts, setAlerts] = useState(null) // {chat_id, enabled}
  const [chatId, setChatId] = useState('')
  const [alertBusy, setAlertBusy] = useState(false)

  // Метрики перечитываются при смене периода; журнал — при смене фильтра.
  useEffect(() => {
    let alive = true
    setStats(null)
    emma
      .fetchStats(period)
      .then((d) => alive && setStats(d))
      .catch((err) => alive && setError(emma.errorText(err)))
    return () => {
      alive = false
    }
  }, [period])

  const loadJournal = async (type, page) => {
    try {
      const d = await emma.fetchStatsErrors({ type, page })
      setJournal((j) => (page === 1 || !j ? d : { ...d, errors: [...j.errors, ...d.errors] }))
    } catch (err) {
      store.pushAlert('error', 'Журнал ошибок: ' + emma.errorText(err))
    }
  }

  useEffect(() => {
    setJournal(null)
    loadJournal(kind, 1)
  }, [kind])

  useEffect(() => {
    let alive = true
    emma
      .fetchAlerts()
      .then((d) => {
        if (!alive) return
        setAlerts(d)
        setChatId(d.chat_id)
      })
      .catch((err) => alive && store.pushAlert('error', 'Настройки алертов: ' + emma.errorText(err)))
    return () => {
      alive = false
    }
  }, [])

  const saveAlerts = async () => {
    setAlertBusy(true)
    try {
      const d = await emma.patchAlerts(chatId.trim())
      setAlerts(d)
      setChatId(d.chat_id)
      store.pushAlert('ok', d.enabled ? 'Алерты включены' : 'Алерты выключены')
    } catch (err) {
      store.pushAlert('error', emma.errorText(err))
    } finally {
      setAlertBusy(false)
    }
  }

  const testAlert = async () => {
    setAlertBusy(true)
    try {
      await emma.sendTestAlert()
      store.pushAlert('ok', 'Пробный алерт отправлен — проверьте Telegram')
    } catch (err) {
      store.pushAlert('error', emma.errorText(err)) // 502 → «проверьте chat_id…»
    } finally {
      setAlertBusy(false)
    }
  }

  return (
    <section className="emma-main" aria-label="Статистика">
      <div className="emma-form-row">
        {PERIODS.map(([k, label]) => (
          <button key={k} className={'btn' + (k === period ? ' btn-primary' : '')} onClick={() => setPeriod(k)}>
            {label}
          </button>
        ))}
      </div>
      {error && !stats && <div className="form-error">{error}</div>}
      {!stats && !error && <div className="muted">Загружаем…</div>}
      {stats && (
        <>
          <div className="emma-stats-grid">
            <Stat label="Новые диалоги" value={stats.new_leads} />
            <Stat label="Активные диалоги" value={stats.active_dialogs_24h} hint="входящие за последние 24 ч" />
            <Stat label="Сообщений" value={`${stats.messages_in} вх / ${stats.messages_out} исх`} />
            <Stat label="Ответов Эммы" value={stats.replies} />
            <Stat
              label="Время ответа"
              value={`${Math.round(stats.avg_response_ms)} мс`}
              hint={`p95: ${Math.round(stats.p95_response_ms)} мс`}
            />
            <Stat label="Запросов менеджера (handoff)" value={stats.handoffs} />
            <Stat label="Файлов отправлено" value={stats.files_sent_total} />
            <Stat label="Токены Claude" value={`${stats.tokens_in} вх / ${stats.tokens_out} исх`} />
            <Stat
              label="Расходы Claude"
              value={fmtCost(stats.cost_usd_estimate)}
              hint={`${stats.pricing_note} (${stats.pricing_model})`}
            />
          </div>
          {stats.files_sent.length > 0 && (
            <div className="emma-field">
              <span className="emma-label">Отправки по файлам</span>
              <ul className="emma-files-breakdown">
                {stats.files_sent.map((f, i) => (
                  <li key={f.send_file_id ?? 'deleted-' + i}>
                    {f.name || <span className="muted">файл удалён</span>} — {f.count}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </>
      )}

      <h3>Журнал ошибок</h3>
      <div className="emma-form-row">
        <select aria-label="Фильтр по типу ошибки" value={kind} onChange={(e) => setKind(e.target.value)}>
          {ERROR_KINDS.map(([k, label]) => (
            <option key={k} value={k}>
              {label}
            </option>
          ))}
        </select>
        {journal && <span className="muted">всего: {journal.total}</span>}
      </div>
      {!journal && <div className="muted">Загружаем…</div>}
      {journal && journal.errors.length === 0 && <div className="muted">Ошибок нет — Эмма работает чисто.</div>}
      {journal && journal.errors.length > 0 && (
        <table className="emma-table">
          <thead>
            <tr>
              <th>Когда</th>
              <th>Тип</th>
              <th>Ошибка</th>
              <th>Лид</th>
            </tr>
          </thead>
          <tbody>
            {journal.errors.map((e) => (
              <tr key={e.id}>
                <td>{emma.fmtDate(e.created_at)}</td>
                <td>{kindLabel(e.error_kind)}</td>
                <td className="emma-error-detail">{e.detail}</td>
                <td>{e.lead_id != null ? '#' + e.lead_id : '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {journal && journal.errors.length < journal.total && (
        <button className="btn btn-ghost" onClick={() => loadJournal(kind, journal.page + 1)}>
          Показать ещё
        </button>
      )}

      <h3>Алерты в Telegram</h3>
      <div className="emma-field">
        <span className="emma-label">Chat ID группы или чата владельца</span>
        <div className="emma-form-row">
          <input
            aria-label="Chat ID для алертов"
            placeholder="-1001234567890"
            value={chatId}
            onChange={(e) => setChatId(e.target.value)}
          />
          <button className="btn btn-primary" disabled={alertBusy || !alerts} onClick={saveAlerts}>
            Сохранить
          </button>
          <button className="btn" disabled={alertBusy || !alerts?.enabled} onClick={testAlert}>
            Отправить пробный алерт
          </button>
        </div>
        <small className="muted">
          Пусто — алерты выключены. Узнать chat_id: напишите боту @userinfobot (для группы — добавьте его в группу).
        </small>
      </div>
    </section>
  )
}
