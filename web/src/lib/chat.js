// chat.js — состояние чат-панели карточки лида (M12). Без React, по образцу
// store.js: иммутабельный снапшот + useSyncExternalStore в компоненте.
//
// Источники данных:
//   - GET /api/leads/:id/messages при открытии карточки, догрузка вверх
//     по before_id (id старейшего загруженного сообщения);
//   - живые обновления — WS-события `message` (socket.js зовёт applyEvent);
//   - reconnect/переход в polling: Redis pub/sub fire-and-forget, окно
//     обрыва могло потерять событие — история перезапрашивается (§10.3),
//     подписка на смену store.connection ниже.
//
// Живое событие не несёт id строки, поэтому у live-пузырей ключ liveKey.
// Эхо собственного POST (событие о только что отправленной реплике)
// гасится списком pendingSent — сообщение уже добавлено из ответа ручки.
import * as api from './api.js'
import * as store from './store.js'

export const PAGE_SIZE = 50

let state = initialState()
const listeners = new Set()
let liveSeq = 0
let pendingSent = []

function initialState() {
  return {
    leadId: null,
    messages: [], // от старых к новым; {id|null, direction, author, content, created_at, liveKey?}
    hasMore: false, // выше есть ещё история (страница пришла полной)
    loadingOlder: false,
    sending: false,
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

// open — карточка открыта: сбросить панель и загрузить последнюю страницу.
export async function open(leadId) {
  pendingSent = []
  commit({ ...initialState(), leadId })
  await refresh(leadId)
}

export function close() {
  pendingSent = []
  commit(initialState())
}

// refresh — последняя страница истории заново (открытие, reconnect,
// после счёта в polling-режиме). Загруженные выше страницы сбрасываются —
// консистентность дороже позиции прокрутки.
export async function refresh(leadId = state.leadId) {
  if (!leadId) return
  const { messages } = await api.fetchMessages(leadId, { limit: PAGE_SIZE })
  if (state.leadId !== leadId) return // карточку успели закрыть/сменить
  commit({ ...state, messages, hasMore: messages.length === PAGE_SIZE })
}

// loadOlder — прокрутка вверх: страница до старейшего загруженного id.
export async function loadOlder() {
  const { leadId, messages, hasMore, loadingOlder } = state
  if (!leadId || loadingOlder || !hasMore) return
  const oldest = messages.find((m) => m.id != null)
  if (!oldest) return
  commit({ ...state, loadingOlder: true })
  try {
    const { messages: older } = await api.fetchMessages(leadId, {
      limit: PAGE_SIZE,
      beforeId: oldest.id,
    })
    if (state.leadId !== leadId) return
    commit({
      ...state,
      messages: [...older, ...state.messages],
      hasMore: older.length === PAGE_SIZE,
      loadingOlder: false,
    })
  } catch (err) {
    if (state.leadId === leadId) commit({ ...state, loadingOlder: false })
    throw err
  }
}

// send — реплика менеджера. Бэкенд шлёт в Telegram и только при успехе
// пишет строку (502 ERR_TELEGRAM_SEND — в истории её нет); ответ ручки —
// готовое сообщение с id. WS-эхо о том же сообщении гасится в ОБОИХ
// порядках прихода (боевой баг M12: эхо часто обгоняет HTTP-ответ):
//   эхо позже ответа  → pendingSent, applyEvent его глотает;
//   эхо раньше ответа → live-пузырь уже в списке, ответ ручки не
//     добавляет второй, а поднимает live-пузырь до строки с id.
export async function send(text) {
  const { leadId, sending } = state
  if (!leadId || sending) return
  commit({ ...state, sending: true })
  try {
    const { message } = await api.postMessage(leadId, text)
    if (state.leadId !== leadId) return
    const echoAt = state.messages.findLastIndex(
      (m) =>
        m.id == null &&
        m.direction === message.direction &&
        (m.author || null) === (message.author || null) &&
        m.content === message.content,
    )
    if (echoAt >= 0) {
      const messages = [...state.messages]
      messages[echoAt] = message
      commit({ ...state, messages, sending: false })
      return
    }
    pendingSent = [...pendingSent.slice(-19), message.content]
    commit({ ...state, messages: [...state.messages, message], sending: false })
  } catch (err) {
    if (state.leadId === leadId) commit({ ...state, sending: false })
    throw err
  }
}

// applyEvent — WS-событие `message` (зовёт socket.js). Чужие лиды панель
// не касаются; см. дедуп в шапке файла.
export function applyEvent(ev) {
  if (!state.leadId || ev.lead_id !== state.leadId) return
  const msg = {
    id: null,
    direction: ev.direction,
    author: ev.author || null,
    content: ev.content || '',
    created_at: ev.ts,
    liveKey: 'live-' + ++liveSeq,
  }
  // Эхо собственного POST: реплика уже в списке (из ответа ручки).
  if (msg.direction === 'outbound' && msg.author?.startsWith('manager:')) {
    const i = pendingSent.indexOf(msg.content)
    if (i >= 0) {
      pendingSent = [...pendingSent.slice(0, i), ...pendingSent.slice(i + 1)]
      return
    }
  }
  // Дубль против строки, уже пришедшей по REST (refresh обогнал событие):
  // у REST-строк есть id, поэтому повторные ЖИВЫЕ реплики («да», «да»)
  // под дедуп не попадают.
  const tail = state.messages.slice(-10)
  if (
    tail.some(
      (m) =>
        m.id != null &&
        m.direction === msg.direction &&
        (m.author || null) === msg.author &&
        m.content === msg.content,
    )
  ) {
    return
  }
  commit({ ...state, messages: [...state.messages, msg] })
}

// bubbleRole — чей пузырь: клиент / Эмма (bot|NULL — старые строки) /
// менеджер. Используется компонентом и тестами.
export function bubbleRole(m) {
  if (m.direction === 'inbound') return 'client'
  return m.author && m.author.startsWith('manager:') ? 'manager' : 'bot'
}

// Перезапрос истории на смене режима соединения (§10.3): возврат в live
// (reconnect или конец деградации pub/sub) и вход в polling. Первый переход
// connecting → live не считается — open() только что загрузил историю.
let lastConnection = store.getState().connection
store.subscribe(() => {
  const mode = store.getState().connection
  if (mode === lastConnection) return
  const prev = lastConnection
  lastConnection = mode
  if (!state.leadId || prev === 'connecting') return
  if (mode === 'live' || mode === 'polling') {
    refresh().catch(() => {
      /* история придёт со следующим переходом/переоткрытием карточки */
    })
  }
})
