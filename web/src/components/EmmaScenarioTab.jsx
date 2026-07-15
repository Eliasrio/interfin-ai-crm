// EmmaScenarioTab — вкладка «Сценарий» (EP-07 задача 7, ТЗ §3 вкладка 5):
// приветствие /start (пусто = отвечает Эмма-LLM), кнопка «Связаться с
// менеджером» (тумблер + текст), текст подтверждения handoff. Общий
// паттерн: баннер несохранённых → Применить → PATCH scenario.
import { useEffect, useState } from 'react'
import * as emma from '../lib/emma.js'
import * as store from '../lib/store.js'

export default function EmmaScenarioTab({ onDirty }) {
  const [base, setBase] = useState(null)
  const [draft, setDraft] = useState(null)
  const [error, setError] = useState(null)
  const [saving, setSaving] = useState(false)

  const load = (d) => {
    setBase(d)
    setDraft({ ...d })
  }

  useEffect(() => {
    let alive = true
    emma
      .fetchScenario()
      .then((d) => alive && load(d))
      .catch((err) => alive && setError(emma.errorText(err)))
    return () => {
      alive = false
    }
  }, [])

  const dirty =
    base != null &&
    draft != null &&
    (draft.welcome_text !== base.welcome_text ||
      draft.manager_button_enabled !== base.manager_button_enabled ||
      draft.manager_button_text !== base.manager_button_text ||
      draft.handoff_confirm_text !== base.handoff_confirm_text ||
      draft.manager_mention !== base.manager_mention ||
      draft.chat_greeting_text !== base.chat_greeting_text)

  useEffect(() => {
    onDirty(Boolean(dirty))
  }, [dirty, onDirty])

  if (error && !base) return <div className="emma-main form-error">{error}</div>
  if (!draft) return <div className="emma-main muted">Загружаем…</div>

  // Инвариант вкладки (зеркало серверного 400): включённая кнопка обязана
  // иметь текст — Применить блокируется локально с пояснением.
  const buttonInvalid = draft.manager_button_enabled && !draft.manager_button_text.trim()

  const apply = async () => {
    if (!dirty || buttonInvalid || saving) return
    setSaving(true)
    setError(null)
    try {
      const d = await emma.patchScenario({
        welcome_text: draft.welcome_text,
        manager_button_enabled: draft.manager_button_enabled,
        manager_button_text: draft.manager_button_text,
        handoff_confirm_text: draft.handoff_confirm_text,
        manager_mention: draft.manager_mention,
        chat_greeting_text: draft.chat_greeting_text,
      })
      load(d)
      store.pushAlert('ok', 'Сохранено, Эмма подхватит в течение 30 секунд')
    } catch (err) {
      setError(emma.errorText(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <section className="emma-main" aria-label="Сценарий">
      {dirty && <div className="emma-banner">⚠️ Есть несохранённые изменения</div>}
      {error && <div className="form-error">{error}</div>}

      <label className="emma-field">
        <span className="emma-label">Приветствие на /start</span>
        <textarea
          className="emma-textarea"
          aria-label="Приветствие на /start"
          rows={5}
          value={draft.welcome_text}
          onChange={(e) => setDraft({ ...draft, welcome_text: e.target.value })}
        />
        <small className="muted">
          Отправляется мгновенно, без вызова Claude. Пусто — на /start отвечает сама Эмма (как сейчас).
        </small>
      </label>

      <div className="emma-field">
        <label className="emma-switch">
          <input
            type="checkbox"
            aria-label="Кнопка «Связаться с менеджером»"
            checked={draft.manager_button_enabled}
            onChange={(e) => setDraft({ ...draft, manager_button_enabled: e.target.checked })}
          />
          Кнопка «Связаться с менеджером»
        </label>
        <small className="muted">
          Клиент видит постоянную кнопку под полем ввода начиная с приветствия; нажатие сразу переводит диалог на
          менеджера.
        </small>
      </div>

      <label className="emma-field">
        <span className="emma-label">Текст кнопки</span>
        <input
          aria-label="Текст кнопки менеджера"
          value={draft.manager_button_text}
          disabled={!draft.manager_button_enabled}
          onChange={(e) => setDraft({ ...draft, manager_button_text: e.target.value })}
        />
      </label>
      {buttonInvalid && <div className="form-error">У включённой кнопки должен быть текст.</div>}

      <label className="emma-field">
        <span className="emma-label">Ответ Эммы при передаче менеджеру</span>
        <input
          aria-label="Текст подтверждения handoff"
          placeholder="Сейчас свяжу вас с менеджером, ожидайте"
          value={draft.handoff_confirm_text}
          onChange={(e) => setDraft({ ...draft, handoff_confirm_text: e.target.value })}
        />
        <small className="muted">Эмма отвечает так на кнопку менеджера и на распознанную просьбу позвать человека.</small>
      </label>

      <label className="emma-field">
        <span className="emma-label">Заготовка приветствия в чате карточки</span>
        <textarea
          className="emma-textarea"
          aria-label="Заготовка приветствия в чате"
          rows={2}
          placeholder="Здравствуйте, меня зовут Екатерина. Чем могу помочь?"
          value={draft.chat_greeting_text}
          onChange={(e) => setDraft({ ...draft, chat_greeting_text: e.target.value })}
        />
        <small className="muted">
          Кнопка «👋 Приветствие» в чате карточки вставляет этот текст в поле ввода — менеджер может поправить и
          отправить. Пусто — кнопки нет.
        </small>
      </label>

      <label className="emma-field">
        <span className="emma-label">Упоминание менеджера в уведомлениях</span>
        <input
          aria-label="Упоминание менеджера"
          placeholder="username (без @)"
          value={draft.manager_mention}
          onChange={(e) => setDraft({ ...draft, manager_mention: e.target.value })}
        />
        <small className="muted">
          Telegram-ник менеджера: уведомления «клиент просит менеджера» будут начинаться с @упоминания — оно
          присылает сигнал даже при выключенном звуке группы. Пусто — без упоминания.
        </small>
      </label>

      <button className="btn btn-primary" disabled={!dirty || buttonInvalid || saving} onClick={apply}>
        {saving ? 'Сохраняем…' : 'Применить'}
      </button>
    </section>
  )
}
