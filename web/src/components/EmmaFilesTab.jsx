// EmmaFilesTab — вкладка «Файлы» (EP-07 задача 5, ТЗ §3 вкладка 3):
// библиотека вложений, которые Эмма отправляет клиентам по маркеру
// {{file:N}}. Карточки с тумблером «Активен», загрузка PDF/JPG/PNG с
// обязательным описанием-подсказкой для Эммы, удаление с подтверждением.
import { useEffect, useRef, useState } from 'react'
import * as emma from '../lib/emma.js'
import * as store from '../lib/store.js'

const MIME_LABELS = {
  'application/pdf': 'PDF',
  'image/jpeg': 'JPG',
  'image/png': 'PNG',
}

export default function EmmaFilesTab() {
  const [files, setFiles] = useState(null)
  const [error, setError] = useState(null)
  const [form, setForm] = useState({ name: '', description: '', file: null })
  const [saving, setSaving] = useState(false)
  const [deletingId, setDeletingId] = useState(null)
  const inputRef = useRef(null)

  useEffect(() => {
    let alive = true
    emma
      .fetchSendFiles()
      .then((d) => alive && setFiles(d.items))
      .catch((err) => alive && setError(emma.errorText(err)))
    return () => {
      alive = false
    }
  }, [])

  const formValid = form.name.trim() && form.description.trim() && form.file

  const upload = async (e) => {
    e.preventDefault()
    if (!formValid || saving) return
    setSaving(true)
    setError(null)
    try {
      const row = await emma.uploadSendFile({
        name: form.name.trim(),
        description: form.description.trim(),
        file: form.file,
      })
      setFiles((fs) => [row, ...(fs || [])])
      setForm({ name: '', description: '', file: null })
      if (inputRef.current) inputRef.current.value = ''
      store.pushAlert('ok', `«${row.name}» добавлен — Эмма подхватит в течение 30 секунд`)
    } catch (err) {
      setError(emma.errorText(err))
    } finally {
      setSaving(false)
    }
  }

  const toggle = async (f) => {
    setError(null)
    try {
      const row = await emma.patchSendFile(f.id, { is_active: !f.is_active })
      setFiles((fs) => fs.map((x) => (x.id === row.id ? row : x)))
    } catch (err) {
      setError(emma.errorText(err))
    }
  }

  const del = async (id) => {
    setError(null)
    try {
      await emma.deleteSendFile(id)
      setFiles((fs) => fs.filter((f) => f.id !== id))
      setDeletingId(null)
      store.pushAlert('ok', 'Файл удалён из библиотеки')
    } catch (err) {
      setDeletingId(null)
      setError(emma.errorText(err))
    }
  }

  return (
    <section className="emma-main" aria-label="Файлы для отправки">
      <form className="emma-upload-form" onSubmit={upload}>
        <h3>Новый файл</h3>
        <label className="emma-field">
          <span className="emma-label">Название</span>
          <input
            aria-label="Название файла"
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
          />
        </label>
        <label className="emma-field">
          <span className="emma-label">Описание для Эммы (когда отправлять)</span>
          <textarea
            className="emma-textarea"
            aria-label="Описание для Эммы"
            rows={2}
            placeholder="отправь, когда клиент спрашивает цены"
            value={form.description}
            onChange={(e) => setForm({ ...form, description: e.target.value })}
          />
          <small className="muted">
            Обязательное поле: по этой подсказке Эмма решает, когда отправить файл клиенту.
          </small>
        </label>
        <label className="emma-field">
          <span className="emma-label">Файл (PDF, JPG или PNG, до 50 МБ)</span>
          <input
            ref={inputRef}
            type="file"
            aria-label="Файл для отправки"
            accept=".pdf,.jpg,.jpeg,.png"
            onChange={(e) => setForm({ ...form, file: e.target.files[0] || null })}
          />
        </label>
        <button className="btn btn-primary" type="submit" disabled={!formValid || saving}>
          {saving ? 'Загружаем…' : 'Добавить файл'}
        </button>
      </form>

      {error && <div className="form-error">{error}</div>}
      {!files && !error && <div className="muted">Загружаем…</div>}
      {files && files.length === 0 && <div className="muted">Файлов пока нет — Эмме нечего отправлять клиентам.</div>}
      <div className="emma-cards">
        {files?.map((f) => (
          <div key={f.id} className="emma-card">
            <div className="emma-card-body">
              <b>{f.name}</b>
              <div className="muted">{f.description}</div>
              <small className="muted">
                {MIME_LABELS[f.mime] || f.mime} · {emma.fmtSize(f.size)} · {emma.fmtDate(f.created_at)}
              </small>
            </div>
            <label className="emma-switch">
              <input
                type="checkbox"
                aria-label={'Активен: ' + f.name}
                checked={f.is_active}
                onChange={() => toggle(f)}
              />
              Активен
            </label>
            {deletingId === f.id ? (
              <span className="emma-confirm">
                Эмма больше не сможет отправить этот файл.
                <button className="btn btn-danger" onClick={() => del(f.id)}>
                  Удалить
                </button>
                <button className="btn btn-ghost" onClick={() => setDeletingId(null)}>
                  Отмена
                </button>
              </span>
            ) : (
              <button className="btn btn-danger" onClick={() => setDeletingId(f.id)}>
                Удалить
              </button>
            )}
          </div>
        ))}
      </div>
    </section>
  )
}
