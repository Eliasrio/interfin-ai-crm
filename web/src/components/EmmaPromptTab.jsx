// EmmaPromptTab — вкладка «Промпт» (EP-07 задача 3, ТЗ §3 вкладка 1):
// системный промпт с live-счётчиком токенов ФОРМУЛОЙ ВОРКЕРА, теги
// запретных тем, стиль, боковая история версий с просмотром и restore.
import { useEffect, useState } from 'react'
import * as emma from '../lib/emma.js'
import * as store from '../lib/store.js'

export const STYLES = [
  ['formal', 'Формальный'],
  ['friendly', 'Дружелюбный'],
  ['neutral', 'Нейтральный'],
  ['expert', 'Экспертный'],
]

const styleLabel = (key) => (STYLES.find(([k]) => k === key) || [key, key])[1]

export default function EmmaPromptTab({ onDirty }) {
  const [base, setBase] = useState(null) // снапшот сервера (ответ GET/PUT/restore)
  const [draft, setDraft] = useState(null) // {text, topics, style}
  const [tag, setTag] = useState('')
  const [error, setError] = useState(null)
  const [saving, setSaving] = useState(false)

  const [history, setHistory] = useState(null) // {items, total, page}
  const [viewed, setViewed] = useState(null) // полная версия в read-only модалке
  const [restoreId, setRestoreId] = useState(null) // версия, ждущая подтверждения

  // load — единая точка приёма ответа сервера: PUT и restore отдают то же
  // тело, что GET (контракт EP-02) — база и форма всегда синхронны.
  const load = (data) => {
    setBase(data)
    setDraft({ text: data.system_prompt, topics: data.forbidden_topics, style: data.style })
  }

  const loadHistory = async (page) => {
    try {
      const d = await emma.fetchPromptHistory(page)
      setHistory((h) => (page === 1 || !h ? d : { ...d, items: [...h.items, ...d.items] }))
    } catch (err) {
      // История — не блокер редактора: показываем в тосте, «показать ещё»
      // можно нажать повторно.
      store.pushAlert('error', 'История версий: ' + emma.errorText(err))
    }
  }

  useEffect(() => {
    let alive = true
    emma
      .fetchPrompt()
      .then((d) => alive && load(d))
      .catch((err) => alive && setError(emma.errorText(err)))
    loadHistory(1)
    return () => {
      alive = false
    }
  }, [])

  const dirty =
    base != null &&
    draft != null &&
    (draft.text !== base.system_prompt ||
      draft.style !== base.style ||
      JSON.stringify(draft.topics) !== JSON.stringify(base.forbidden_topics))

  useEffect(() => {
    onDirty(Boolean(dirty))
  }, [dirty, onDirty])

  if (error && !base) return <div className="emma-main form-error">{error}</div>
  if (!base || !draft) return <div className="emma-main muted">Загружаем…</div>

  const tokens = emma.estimateTokens(draft.text)
  const limit = base.token_limit
  const over = tokens > limit

  const addTag = () => {
    const t = tag.trim()
    if (!t || draft.topics.includes(t)) return
    setDraft({ ...draft, topics: [...draft.topics, t] })
    setTag('')
  }

  const apply = async () => {
    if (!dirty || over || saving) return
    setSaving(true)
    setError(null)
    try {
      const d = await emma.putPrompt({
        systemPrompt: draft.text,
        forbiddenTopics: draft.topics,
        style: draft.style,
      })
      load(d)
      store.pushAlert('ok', 'Сохранено, Эмма подхватит в течение 30 секунд')
      loadHistory(1) // прежняя активная версия ушла в историю
    } catch (err) {
      // PROMPT_TOO_LONG сюда попадает только при обходе локальной блокировки
      // (правка limit в devtools) — сервер решает окончательно.
      setError(emma.errorText(err))
    } finally {
      setSaving(false)
    }
  }

  const restore = async (id) => {
    try {
      const d = await emma.restorePromptVersion(id)
      load(d)
      setRestoreId(null)
      setViewed(null)
      store.pushAlert('ok', 'Версия восстановлена — Эмма подхватит в течение 30 секунд')
      loadHistory(1)
    } catch (err) {
      setRestoreId(null)
      store.pushAlert('error', emma.errorText(err))
    }
  }

  const openVersion = async (id) => {
    try {
      setViewed(await emma.fetchPromptVersion(id))
    } catch (err) {
      store.pushAlert('error', emma.errorText(err))
    }
  }

  return (
    <>
      <section className="emma-main" aria-label="Промпт">
        {dirty && <div className="emma-banner">⚠️ Есть несохранённые изменения</div>}
        {error && <div className="form-error">{error}</div>}

        <label className="emma-field">
          <span className="emma-label">
            Системный промпт{' '}
            <span className={'emma-tokens' + (over ? ' emma-tokens-over' : '')} data-testid="token-counter">
              ≈{tokens} / {limit} токенов
            </span>
          </span>
          <textarea
            className="emma-textarea"
            aria-label="Системный промпт"
            rows={20}
            value={draft.text}
            onChange={(e) => setDraft({ ...draft, text: e.target.value })}
          />
        </label>
        {over && <div className="form-error">Промпт длиннее лимита — сократите текст, иначе не сохранится.</div>}

        <div className="emma-field">
          <span className="emma-label">Запретные темы</span>
          <div className="emma-tags">
            {draft.topics.map((t) => (
              <span key={t} className="emma-tag">
                {t}
                <button
                  className="emma-tag-x"
                  aria-label={'Удалить тему ' + t}
                  onClick={() => setDraft({ ...draft, topics: draft.topics.filter((x) => x !== t) })}
                >
                  ×
                </button>
              </span>
            ))}
            <input
              aria-label="Новая запретная тема"
              placeholder="тема + Enter"
              value={tag}
              onChange={(e) => setTag(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  addTag()
                }
              }}
            />
          </div>
          <small className="muted">Попадут в промпт секцией «Никогда не обсуждай: …».</small>
        </div>

        <label className="emma-field">
          <span className="emma-label">Стиль общения</span>
          <select
            aria-label="Стиль общения"
            value={draft.style}
            onChange={(e) => setDraft({ ...draft, style: e.target.value })}
          >
            {STYLES.map(([k, label]) => (
              <option key={k} value={k}>
                {label}
              </option>
            ))}
          </select>
        </label>

        <button className="btn btn-primary" disabled={!dirty || over || saving} onClick={apply}>
          {saving ? 'Сохраняем…' : 'Применить'}
        </button>
      </section>

      <aside className="emma-history" aria-label="История версий">
        <h3>История версий</h3>
        {!history && <div className="muted">Загружаем…</div>}
        {history &&
          history.items.map((it) => (
            <div key={it.id} className="emma-history-item">
              <div>
                <b>{emma.fmtDate(it.created_at)}</b>{' '}
                <span className="muted">{it.created_by != null ? `менеджер #${it.created_by}` : 'система'}</span>
              </div>
              <div className="muted">{styleLabel(it.style)}</div>
              <div className="emma-preview">{it.preview}</div>
              {restoreId === it.id ? (
                <div className="emma-confirm">
                  Вернутся текст, темы и стиль версии от {emma.fmtDate(it.created_at)}.
                  <div className="stage-buttons">
                    <button className="btn btn-danger" onClick={() => restore(it.id)}>
                      Да, восстановить
                    </button>
                    <button className="btn btn-ghost" onClick={() => setRestoreId(null)}>
                      Отмена
                    </button>
                  </div>
                </div>
              ) : (
                <div className="stage-buttons">
                  <button className="btn" onClick={() => openVersion(it.id)}>
                    Просмотреть
                  </button>
                  <button className="btn" onClick={() => setRestoreId(it.id)}>
                    Восстановить
                  </button>
                </div>
              )}
            </div>
          ))}
        {history && history.items.length < history.total && (
          <button className="btn btn-ghost" onClick={() => loadHistory(history.page + 1)}>
            Показать ещё
          </button>
        )}
      </aside>

      {viewed && (
        <div className="modal-backdrop" onClick={() => setViewed(null)}>
          <div className="modal" role="dialog" aria-label="Просмотр версии" onClick={(e) => e.stopPropagation()}>
            <header className="modal-head">
              <h2>
                Версия от {emma.fmtDate(viewed.created_at)}
                {viewed.is_current ? ' (текущая)' : ''}
              </h2>
              <button className="btn btn-ghost" onClick={() => setViewed(null)} aria-label="Закрыть">
                ✕
              </button>
            </header>
            <section className="modal-section">
              <textarea className="emma-textarea" rows={16} readOnly value={viewed.system_prompt} />
              <div className="emma-tags">
                {viewed.forbidden_topics.map((t) => (
                  <span key={t} className="emma-tag">
                    {t}
                  </span>
                ))}
                {viewed.forbidden_topics.length === 0 && <span className="muted">Запретных тем нет</span>}
              </div>
              <div className="muted">Стиль: {styleLabel(viewed.style)}</div>
            </section>
          </div>
        </div>
      )}
    </>
  )
}
