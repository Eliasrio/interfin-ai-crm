// EmmaKbTab — вкладка «База знаний» (EP-07 задача 4, ТЗ §3 вкладка 2):
// таблица файлов со статусами индексации, drag&drop-загрузка TXT/MD/PDF,
// live-статус по WS emma_kb_status (+ refetch при открытии — fallback),
// переиндексация, удаление с подтверждением.
import { useEffect, useRef, useState } from 'react'
import * as emma from '../lib/emma.js'
import * as store from '../lib/store.js'

const MIME_LABELS = {
  'text/plain': 'TXT',
  'text/markdown': 'MD',
  'application/pdf': 'PDF',
}

function statusCell(f) {
  if (f.status === 'indexed') return <span className="emma-status-ok">✅ проиндексирован ({f.chunks_count} чанков)</span>
  if (f.status === 'error') return <span className="emma-status-err">❌ ошибка: {f.index_error}</span>
  return <span className="emma-status-wait">⏳ индексируется…</span>
}

export default function EmmaKbTab() {
  const [files, setFiles] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const [dragOver, setDragOver] = useState(false)
  const [deletingId, setDeletingId] = useState(null)
  const inputRef = useRef(null)

  useEffect(() => {
    let alive = true
    // Fallback к WS: событие могло прийти, пока вкладка была закрыта, —
    // при каждом открытии список перечитывается (task §4).
    emma
      .fetchKb()
      .then((d) => alive && setFiles(d.items))
      .catch((err) => alive && setError(emma.errorText(err)))
    return () => {
      alive = false
    }
  }, [])

  // Live-статус: финал индексации (indexed/error) приходит WS-событием.
  useEffect(
    () =>
      emma.onKbStatus((ev) => {
        setFiles(
          (fs) =>
            fs &&
            fs.map((f) =>
              f.id === ev.file_id
                ? { ...f, status: ev.status, chunks_count: ev.chunks || 0, index_error: ev.error || null }
                : f,
            ),
        )
      }),
    [],
  )

  const upload = async (fileList) => {
    if (busy || !fileList?.length) return
    setBusy(true)
    setError(null)
    try {
      for (const file of Array.from(fileList)) {
        const row = await emma.uploadKb(file)
        // Повторная загрузка того же имени — ТА ЖЕ строка (upsert по
        // filename, id стабилен — QA-фикс ТЗ §3): заменяем по id.
        setFiles((fs) => [row, ...(fs || []).filter((f) => f.id !== row.id)])
        store.pushAlert('ok', `«${row.filename}» загружен — индексация запущена`)
      }
    } catch (err) {
      setError(emma.errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const reindex = async (id) => {
    setError(null)
    try {
      const row = await emma.reindexKb(id)
      setFiles((fs) => fs.map((f) => (f.id === row.id ? row : f)))
    } catch (err) {
      setError(emma.errorText(err))
    }
  }

  const del = async (id) => {
    setError(null)
    try {
      await emma.deleteKb(id)
      setFiles((fs) => fs.filter((f) => f.id !== id))
      setDeletingId(null)
      store.pushAlert('ok', 'Файл удалён из базы знаний')
    } catch (err) {
      setDeletingId(null)
      setError(emma.errorText(err))
    }
  }

  return (
    <section className="emma-main" aria-label="База знаний">
      <div
        className={'emma-drop' + (dragOver ? ' emma-drop-over' : '')}
        data-testid="kb-drop"
        onDragOver={(e) => {
          e.preventDefault()
          setDragOver(true)
        }}
        onDragLeave={() => setDragOver(false)}
        onDrop={(e) => {
          e.preventDefault()
          setDragOver(false)
          upload(e.dataTransfer.files)
        }}
      >
        <div>Перетащите файл сюда или</div>
        <button className="btn" disabled={busy} onClick={() => inputRef.current?.click()}>
          {busy ? 'Загружаем…' : 'Выбрать файл'}
        </button>
        <input
          ref={inputRef}
          type="file"
          accept=".txt,.md,.pdf"
          aria-label="Файл базы знаний"
          hidden
          onChange={(e) => {
            upload(e.target.files)
            e.target.value = ''
          }}
        />
        <small>TXT, MD или PDF до 50 МБ. Повторная загрузка с тем же именем заменяет файл.</small>
      </div>

      {error && <div className="form-error">{error}</div>}
      {!files && !error && <div className="muted">Загружаем…</div>}
      {files && files.length === 0 && <div className="muted">Файлов пока нет — Эмма отвечает без базы знаний.</div>}
      {files && files.length > 0 && (
        <table className="emma-table">
          <thead>
            <tr>
              <th>Файл</th>
              <th>Тип</th>
              <th>Размер</th>
              <th>Загружен</th>
              <th>Статус</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {files.map((f) => (
              <tr key={f.id}>
                <td>{f.filename}</td>
                <td>{MIME_LABELS[f.mime] || f.mime}</td>
                <td>{emma.fmtSize(f.size)}</td>
                <td>{emma.fmtDate(f.created_at)}</td>
                <td data-testid={'kb-status-' + f.id}>{statusCell(f)}</td>
                <td>
                  {deletingId === f.id ? (
                    <span className="emma-confirm">
                      Эмма забудет содержимое файла.
                      <button className="btn btn-danger" onClick={() => del(f.id)}>
                        Удалить
                      </button>
                      <button className="btn btn-ghost" onClick={() => setDeletingId(null)}>
                        Отмена
                      </button>
                    </span>
                  ) : (
                    <span className="stage-buttons">
                      <button className="btn" onClick={() => reindex(f.id)}>
                        Переиндексировать
                      </button>
                      <button className="btn btn-danger" onClick={() => setDeletingId(f.id)}>
                        Удалить
                      </button>
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}
