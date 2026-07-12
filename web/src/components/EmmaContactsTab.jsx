// EmmaContactsTab — вкладка «Контакты» (EP-07 задача 6, ТЗ §3 вкладка 4):
// справочник контактов system-блока. Таблица с инлайн-редактированием,
// счётчик «N из 30 активных», CONTACTS_LIMIT → понятный текст.
import { useEffect, useState } from 'react'
import * as emma from '../lib/emma.js'
import * as store from '../lib/store.js'

export const CONTACT_TYPES = [
  ['phone', '📞 Телефон'],
  ['whatsapp', '💬 WhatsApp'],
  ['telegram', '✈️ Telegram'],
  ['email', '✉️ Email'],
  ['website', '🌐 Сайт'],
  ['other', '🔗 Другое'],
]

const typeLabel = (t) => (CONTACT_TYPES.find(([k]) => k === t) || [t, t])[1]

const EMPTY_DRAFT = { type: 'phone', name: '', value: '', comment: '', sort_order: 0, is_active: true }

export default function EmmaContactsTab() {
  const [data, setData] = useState(null) // {items, limit}
  const [error, setError] = useState(null)
  const [editingId, setEditingId] = useState(null) // id | 'new' | null
  const [draft, setDraft] = useState(EMPTY_DRAFT)
  const [saving, setSaving] = useState(false)
  const [deletingId, setDeletingId] = useState(null)

  // refetch после каждой мутации: сервер сортирует по sort_order —
  // проще перечитать, чем повторять сортировку на клиенте.
  const refetch = async () => {
    try {
      setData(await emma.fetchContacts())
    } catch (err) {
      setError(emma.errorText(err))
    }
  }

  useEffect(() => {
    refetch()
  }, [])

  const startEdit = (c) => {
    setEditingId(c.id)
    setDraft({
      type: c.type,
      name: c.name,
      value: c.value,
      comment: c.comment,
      sort_order: c.sort_order,
      is_active: c.is_active,
    })
  }

  const draftValid = draft.name.trim() && draft.value.trim()

  const save = async () => {
    if (!draftValid || saving) return
    setSaving(true)
    setError(null)
    const fields = {
      type: draft.type,
      name: draft.name.trim(),
      value: draft.value.trim(),
      comment: draft.comment.trim(),
      sort_order: Number(draft.sort_order) || 0,
      is_active: draft.is_active,
    }
    try {
      if (editingId === 'new') await emma.createContact(fields)
      else await emma.patchContact(editingId, fields)
      setEditingId(null)
      setDraft(EMPTY_DRAFT)
      await refetch()
      store.pushAlert('ok', 'Сохранено, Эмма подхватит в течение 30 секунд')
    } catch (err) {
      setError(emma.errorText(err))
    } finally {
      setSaving(false)
    }
  }

  const toggleActive = async (c) => {
    setError(null)
    try {
      await emma.patchContact(c.id, { is_active: !c.is_active })
      await refetch()
    } catch (err) {
      setError(emma.errorText(err)) // CONTACTS_LIMIT → «Лимит 30 активных…»
    }
  }

  const del = async (id) => {
    setError(null)
    try {
      await emma.deleteContact(id)
      setDeletingId(null)
      await refetch()
    } catch (err) {
      setDeletingId(null)
      setError(emma.errorText(err))
    }
  }

  if (!data && !error) return <div className="emma-main muted">Загружаем…</div>
  if (!data) return <div className="emma-main form-error">{error}</div>

  const activeCount = data.items.filter((c) => c.is_active).length

  const editorRow = (key) => (
    <tr key={key} className="emma-edit-row">
      <td>
        <select aria-label="Тип контакта" value={draft.type} onChange={(e) => setDraft({ ...draft, type: e.target.value })}>
          {CONTACT_TYPES.map(([k, label]) => (
            <option key={k} value={k}>
              {label}
            </option>
          ))}
        </select>
      </td>
      <td>
        <input
          aria-label="Название контакта"
          placeholder="Офис в Бузиосе"
          value={draft.name}
          onChange={(e) => setDraft({ ...draft, name: e.target.value })}
        />
      </td>
      <td>
        <input
          aria-label="Значение контакта"
          placeholder="+55 22 99999-99-99"
          value={draft.value}
          onChange={(e) => setDraft({ ...draft, value: e.target.value })}
        />
      </td>
      <td>
        <input
          aria-label="Комментарий для Эммы"
          placeholder="давай, когда клиент готов к консультации"
          value={draft.comment}
          onChange={(e) => setDraft({ ...draft, comment: e.target.value })}
        />
      </td>
      <td>
        <input
          type="checkbox"
          aria-label="Контакт активен"
          checked={draft.is_active}
          onChange={(e) => setDraft({ ...draft, is_active: e.target.checked })}
        />
      </td>
      <td>
        <input
          className="emma-order"
          aria-label="Порядок сортировки"
          inputMode="numeric"
          value={draft.sort_order}
          onChange={(e) => setDraft({ ...draft, sort_order: e.target.value })}
        />
      </td>
      <td>
        <span className="stage-buttons">
          <button className="btn btn-primary" disabled={!draftValid || saving} onClick={save}>
            {saving ? '…' : 'Сохранить'}
          </button>
          <button
            className="btn btn-ghost"
            onClick={() => {
              setEditingId(null)
              setDraft(EMPTY_DRAFT)
            }}
          >
            Отмена
          </button>
        </span>
      </td>
    </tr>
  )

  return (
    <section className="emma-main" aria-label="Контакты">
      <div className="emma-form-row">
        <span data-testid="contacts-counter">
          <b>{activeCount}</b> из {data.limit} активных
        </span>
        <button
          className="btn"
          disabled={editingId != null}
          onClick={() => {
            setEditingId('new')
            setDraft(EMPTY_DRAFT)
          }}
        >
          + Контакт
        </button>
      </div>
      {error && <div className="form-error">{error}</div>}
      <table className="emma-table">
        <thead>
          <tr>
            <th>Тип</th>
            <th>Название</th>
            <th>Значение</th>
            <th>Комментарий для Эммы</th>
            <th>Активен</th>
            <th>Порядок</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {editingId === 'new' && editorRow('new')}
          {data.items.map((c) =>
            editingId === c.id ? (
              editorRow(c.id)
            ) : (
              <tr key={c.id}>
                <td>{typeLabel(c.type)}</td>
                <td>{c.name}</td>
                <td>{c.value}</td>
                <td className="muted">{c.comment}</td>
                <td>
                  <input
                    type="checkbox"
                    aria-label={'Активен: ' + c.name}
                    checked={c.is_active}
                    onChange={() => toggleActive(c)}
                  />
                </td>
                <td>{c.sort_order}</td>
                <td>
                  {deletingId === c.id ? (
                    <span className="emma-confirm">
                      Удалить контакт?
                      <button className="btn btn-danger" onClick={() => del(c.id)}>
                        Да
                      </button>
                      <button className="btn btn-ghost" onClick={() => setDeletingId(null)}>
                        Отмена
                      </button>
                    </span>
                  ) : (
                    <span className="stage-buttons">
                      <button className="btn" disabled={editingId != null} onClick={() => startEdit(c)}>
                        ✎
                      </button>
                      <button className="btn btn-danger" onClick={() => setDeletingId(c.id)}>
                        Удалить
                      </button>
                    </span>
                  )}
                </td>
              </tr>
            ),
          )}
        </tbody>
      </table>
      {data.items.length === 0 && editingId !== 'new' && (
        <div className="muted">Контактов нет — Эмма не даёт клиентам никаких контактов, кроме добавленных сюда.</div>
      )}
    </section>
  )
}
