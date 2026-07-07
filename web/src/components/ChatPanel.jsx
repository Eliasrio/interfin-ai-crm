// ChatPanel — чат менеджера в карточке лида (M12): история с догрузкой
// вверх, живые обновления по WS `message`, поле ввода и счёт CryptoBot.
// Вся логика — в lib/chat.js; компонент только рисует снапшот.
import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import * as api from '../lib/api.js'
import * as chat from '../lib/chat.js'
import * as store from '../lib/store.js'

function useChat() {
  return useSyncExternalStore(chat.subscribe, chat.getState)
}

const ROLE_LABEL = { client: 'лид', bot: 'Эмма', manager: '' }

export default function ChatPanel({ lead }) {
  const { leadId, messages, hasMore, loadingOlder, sending } = useChat()
  const [text, setText] = useState('')
  const [invoiceOpen, setInvoiceOpen] = useState(false)
  const listRef = useRef(null)
  const stickBottom = useRef(true)

  useEffect(() => {
    chat
      .open(lead.id)
      .catch((err) => store.pushAlert('error', `История чата: ${err.message}`, lead.id))
    return () => chat.close()
  }, [lead.id])

  // Автопрокрутка вниз на новых сообщениях — только если менеджер и так
  // был у низа (читает свежее, а не листает архив).
  useEffect(() => {
    const el = listRef.current
    if (el && stickBottom.current) el.scrollTop = el.scrollHeight
  }, [messages])

  const onScroll = () => {
    const el = listRef.current
    if (!el) return
    stickBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
    if (el.scrollTop < 30 && hasMore && !loadingOlder) {
      const prevHeight = el.scrollHeight
      chat
        .loadOlder()
        .then(() => {
          // держим видимые сообщения на месте после вставки страницы сверху
          requestAnimationFrame(() => {
            el.scrollTop = el.scrollHeight - prevHeight
          })
        })
        .catch((err) => store.pushAlert('error', `Догрузка истории: ${err.message}`, lead.id))
    }
  }

  const submit = async (e) => {
    e.preventDefault()
    const t = text.trim()
    if (!t || sending) return
    try {
      await chat.send(t)
      setText('')
    } catch (err) {
      // Ошибка — тостом; 502 = лид недоступен в Telegram, в истории пусто.
      store.pushAlert('error', `Сообщение не отправлено: ${err.message}`, lead.id)
    }
  }

  const ready = leadId === lead.id
  return (
    <div className="chat" data-testid="chat-panel">
      <div className="chat-list" ref={listRef} onScroll={onScroll} data-testid="chat-list">
        {hasMore && (
          <div className="muted chat-more">{loadingOlder ? 'Загружаем…' : '↑ прокрутите вверх — история'}</div>
        )}
        {!ready && <div className="muted">Загружаем…</div>}
        {ready && !messages.length && <div className="muted">Сообщений нет</div>}
        {ready && messages.map((m) => <Bubble key={m.id ?? m.liveKey} m={m} />)}
      </div>

      <form className="chat-input" onSubmit={submit}>
        <textarea
          aria-label="Сообщение лиду"
          rows={2}
          maxLength={4096}
          placeholder="Написать клиенту от имени бота…"
          value={text}
          disabled={sending}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) submit(e)
          }}
        />
        <div className="chat-actions">
          <button className="btn btn-primary" type="submit" disabled={sending || !text.trim()}>
            {sending ? 'Отправляем…' : 'Отправить'}
          </button>
          <button className="btn" type="button" onClick={() => setInvoiceOpen((v) => !v)}>
            Выставить счёт
          </button>
        </div>
      </form>

      {invoiceOpen && <InvoiceForm lead={lead} onClose={() => setInvoiceOpen(false)} />}
    </div>
  )
}

function Bubble({ m }) {
  const role = chat.bubbleRole(m)
  const label = role === 'manager' ? m.author : ROLE_LABEL[role]
  return (
    <div className={`msg msg-${m.direction} msg-role-${role}`}>
      <div className="msg-content">{m.content || '(без текста: фото/стикер/голос)'}</div>
      <div className="msg-ts">
        {label} · {fmtTs(m.created_at)}
      </div>
    </div>
  )
}

// InvoiceForm — сумма + валюта + описание, шаг подтверждения, после успеха
// ссылка показывается и копируется (в чат она приходит WS-событием; при
// polling чат перезапрашивается refresh'ем ниже).
function InvoiceForm({ lead, onClose }) {
  const [amount, setAmount] = useState('')
  const [asset, setAsset] = useState('USDT')
  const [description, setDescription] = useState('')
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState(null) // {url, warn?}
  const [copied, setCopied] = useState(false)

  const validAmount = /^\d+(\.\d+)?$/.test(amount.trim()) && Number(amount) > 0

  const submit = async () => {
    if (busy) return
    setBusy(true)
    try {
      const { url } = await api.postInvoice(lead.id, {
        amount: amount.trim(),
        asset,
        description: description.trim(),
      })
      setResult({ url })
      store.pushAlert('ok', `Счёт на ${amount.trim()} ${asset} выставлен — лид #${lead.id}`, lead.id)
      chat.refresh().catch(() => {})
    } catch (err) {
      if (err instanceof api.ApiError && err.code === 'ERR_TELEGRAM_SEND' && err.data?.url) {
        // Счёт живой, но лид недоступен в Telegram — ссылку пересылаем сами.
        setResult({ url: err.data.url, warn: err.message })
      } else {
        store.pushAlert('error', `Счёт не выставлен: ${err.message}`, lead.id)
      }
    } finally {
      setBusy(false)
      setConfirming(false)
    }
  }

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(result.url)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      /* буфер недоступен (http/права) — ссылку можно выделить руками */
    }
  }

  if (result) {
    return (
      <div className="invoice-form" data-testid="invoice-result">
        {result.warn && <div className="form-error">{result.warn}</div>}
        <div className="invoice-url">
          <a href={result.url} target="_blank" rel="noreferrer">
            {result.url}
          </a>
          <button className="btn" type="button" onClick={copy}>
            {copied ? 'Скопировано ✓' : 'Копировать'}
          </button>
        </div>
        <button className="btn btn-ghost" type="button" onClick={onClose}>
          Закрыть
        </button>
      </div>
    )
  }

  return (
    <div className="invoice-form" data-testid="invoice-form">
      <div className="invoice-fields">
        <input
          aria-label="Сумма счёта"
          placeholder="Сумма, напр. 2000"
          inputMode="decimal"
          value={amount}
          onChange={(e) => setAmount(e.target.value)}
        />
        <select aria-label="Валюта счёта" value={asset} onChange={(e) => setAsset(e.target.value)}>
          <option value="USDT">USDT</option>
          <option value="USDC">USDC</option>
        </select>
        <input
          aria-label="Описание счёта"
          placeholder="Описание (необязательно)"
          maxLength={1024}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
      </div>
      {!confirming && (
        <button
          className="btn btn-primary"
          type="button"
          disabled={!validAmount || busy}
          onClick={() => setConfirming(true)}
        >
          Выставить счёт…
        </button>
      )}
      {confirming && (
        <div className="invoice-confirm">
          <span>
            Счёт на <b>{amount.trim()} {asset}</b> уйдёт лиду в Telegram (действует 24 ч). Подтвердить?
          </span>
          <button className="btn btn-primary" type="button" disabled={busy} onClick={submit}>
            {busy ? 'Выставляем…' : 'Да, выставить'}
          </button>
          <button className="btn btn-ghost" type="button" disabled={busy} onClick={() => setConfirming(false)}>
            Отмена
          </button>
        </div>
      )}
    </div>
  )
}

function fmtTs(ts) {
  return new Date(ts).toLocaleString('ru-RU', { dateStyle: 'short', timeStyle: 'short' })
}
