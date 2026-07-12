// emma.js — логика панели Эммы без React (EP-07): fetch-обёртки /api/emma/*,
// счётчик токенов ФОРМУЛОЙ ВОРКЕРА, шина «PIN-сессия истекла» и шина
// WS-событий emma_kb_status. Компоненты вкладок держат только состояние UI.
//
// Все защищённые обёртки завёрнуты в guard(): 401 PIN_REQUIRED от любой
// ручки означает «сессия истекла через 30 мин» — панель возвращается на
// PIN-экран централизованно, вкладки об этом не знают (task §1).
import { apiFetch, ApiError } from './api.js'

// --- Счётчик токенов ---

// estimateTokens — зеркало claude.EstimateTokens (len(bytes)/4, деление
// целочисленное). Считаем БАЙТЫ UTF-8, не символы: кириллица ≈ 2 байта на
// символ, наивный text.length/4 занизил бы оценку вдвое (грабля ТЗ §3) —
// редактор врал бы, а сервер резал бы «влезающий» промпт.
export function estimateTokens(text) {
  return new TextEncoder().encode(text).length >> 2
}

// --- Шина «PIN-сессия истекла» ---

const pinListeners = new Set()

// onPinRequired — подписка панели: любой 401 PIN_REQUIRED из обёрток ниже
// вернёт владельца на PIN-экран. Возвращает отписку.
export function onPinRequired(fn) {
  pinListeners.add(fn)
  return () => pinListeners.delete(fn)
}

// isPinRequired — ручная проверка для мест, где ошибка ловится локально.
export function isPinRequired(err) {
  return err instanceof ApiError && err.code === 'PIN_REQUIRED'
}

// guard — обёртка защищённых ручек: PIN_REQUIRED уведомляет подписчиков
// (панель уходит на PIN-экран) и пробрасывается дальше — вызвавшая вкладка
// просто прекращает свою операцию.
async function guard(promise) {
  try {
    return await promise
  } catch (err) {
    if (isPinRequired(err)) for (const fn of pinListeners) fn()
    throw err
  }
}

// --- Шина WS-событий emma_kb_status ---

const kbListeners = new Set()

// onKbStatus — подписка вкладки БЗ на финалы индексации (indexed/error).
export function onKbStatus(fn) {
  kbListeners.add(fn)
  return () => kbListeners.delete(fn)
}

// applyKbEvent — вызывается из lib/socket.js на событии emma_kb_status:
// {file_id, filename, status: indexed|error, chunks?, error?}. Панель
// закрыта (подписчиков нет) — событие просто пропадает, вкладка при
// открытии всё равно делает refetch (fallback task §4).
export function applyKbEvent(ev) {
  if (ev?.type !== 'emma_kb_status') return
  for (const fn of kbListeners) fn(ev)
}

// --- Тексты ошибок ---

// Понятный русский текст на каждый код контракта (task «Контекст»).
// Сообщения бэкенда тоже русские — маппинг перекрывает только коды, где
// фронту есть что добавить от себя; остальным отдаём err.message как есть.
const ERROR_TEXTS = {
  PIN_REQUIRED: 'Сессия панели истекла — введите PIN ещё раз',
  PIN_INVALID: 'Неверный PIN',
  PIN_LOCKED: 'Слишком много неверных попыток — подождите',
  PIN_ALREADY_SET: 'PIN уже установлен — воспользуйтесь сменой PIN',
  PIN_UNAVAILABLE: 'Сервис временно недоступен',
  CONTACTS_LIMIT: 'Лимит 30 активных контактов, выключите лишние',
  FILE_TOO_LARGE: 'Файл больше 50 МБ — Telegram такой не отправит',
}

export function errorText(err) {
  if (err instanceof ApiError) {
    if (err.code === 'PROMPT_TOO_LONG' && err.data?.estimate != null) {
      return `Промпт длиннее лимита: ≈${err.data.estimate} токенов при лимите ${err.data.limit}`
    }
    return ERROR_TEXTS[err.code] || err.message
  }
  return err?.message || String(err)
}

// --- Форматтеры (общие для вкладок) ---

export function fmtDate(ts) {
  return new Date(ts).toLocaleString('ru-RU', {
    day: '2-digit',
    month: '2-digit',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

export function fmtSize(bytes) {
  if (bytes >= 1 << 20) return (bytes / (1 << 20)).toFixed(1) + ' МБ'
  if (bytes >= 1 << 10) return Math.round(bytes / (1 << 10)) + ' КБ'
  return bytes + ' Б'
}

// --- PIN (§2.2; ручки pin/* — только JWT+admin, guard не нужен) ---

export function pinStatus() {
  return apiFetch('/api/emma/pin/status')
}

export function pinSetup(pin) {
  return apiFetch('/api/emma/pin/setup', { method: 'POST', body: { pin } })
}

export function pinVerify(pin) {
  return apiFetch('/api/emma/pin/verify', { method: 'POST', body: { pin } })
}

export function pinChange(oldPin, newPin) {
  return apiFetch('/api/emma/pin/change', { method: 'POST', body: { old_pin: oldPin, new_pin: newPin } })
}

// pinCloseSession — «выйти из панели» (task §1).
export function pinCloseSession() {
  return apiFetch('/api/emma/pin/session', { method: 'DELETE' })
}

// --- Промпт (вкладка 1) ---

export function fetchPrompt() {
  return guard(apiFetch('/api/emma/prompt'))
}

export function putPrompt({ systemPrompt, forbiddenTopics, style }) {
  return guard(
    apiFetch('/api/emma/prompt', {
      method: 'PUT',
      body: { system_prompt: systemPrompt, forbidden_topics: forbiddenTopics, style },
    }),
  )
}

export function fetchPromptHistory(page = 1) {
  return guard(apiFetch('/api/emma/prompt/history?page=' + page))
}

export function fetchPromptVersion(vid) {
  return guard(apiFetch('/api/emma/prompt/history/' + vid))
}

export function restorePromptVersion(vid) {
  return guard(apiFetch(`/api/emma/prompt/history/${vid}/restore`, { method: 'POST' }))
}

// --- База знаний (вкладка 2) ---

export function fetchKb() {
  return guard(apiFetch('/api/emma/kb'))
}

// uploadKb — multipart (поле file); 202 = строка создана/заменена,
// индексация в очереди, финал придёт WS-событием.
export function uploadKb(file) {
  const form = new FormData()
  form.append('file', file)
  return guard(apiFetch('/api/emma/kb', { method: 'POST', form }))
}

export function reindexKb(id) {
  return guard(apiFetch(`/api/emma/kb/${id}/reindex`, { method: 'POST' }))
}

export function deleteKb(id) {
  return guard(apiFetch('/api/emma/kb/' + id, { method: 'DELETE' }))
}

// --- Файлы для отправки (вкладка 3) ---

export function fetchSendFiles() {
  return guard(apiFetch('/api/emma/files'))
}

export function uploadSendFile({ name, description, file }) {
  const form = new FormData()
  form.append('name', name)
  form.append('description', description)
  form.append('file', file)
  return guard(apiFetch('/api/emma/files', { method: 'POST', form }))
}

export function patchSendFile(id, fields) {
  return guard(apiFetch('/api/emma/files/' + id, { method: 'PATCH', body: fields }))
}

export function deleteSendFile(id) {
  return guard(apiFetch('/api/emma/files/' + id, { method: 'DELETE' }))
}

// --- Контакты (вкладка 4) ---

export function fetchContacts() {
  return guard(apiFetch('/api/emma/contacts'))
}

export function createContact(fields) {
  return guard(apiFetch('/api/emma/contacts', { method: 'POST', body: fields }))
}

export function patchContact(id, fields) {
  return guard(apiFetch('/api/emma/contacts/' + id, { method: 'PATCH', body: fields }))
}

export function deleteContact(id) {
  return guard(apiFetch('/api/emma/contacts/' + id, { method: 'DELETE' }))
}

// --- Сценарий (вкладка 5) ---

export function fetchScenario() {
  return guard(apiFetch('/api/emma/scenario'))
}

export function patchScenario(fields) {
  return guard(apiFetch('/api/emma/scenario', { method: 'PATCH', body: fields }))
}

// --- Статистика и алерты (вкладка 6) ---

export function fetchStats(period) {
  return guard(apiFetch('/api/emma/stats?period=' + period))
}

export function fetchStatsErrors({ type = '', page = 1, period = 'all' } = {}) {
  const q = new URLSearchParams({ page: String(page), period })
  if (type) q.set('type', type)
  return guard(apiFetch('/api/emma/stats/errors?' + q))
}

export function fetchAlerts() {
  return guard(apiFetch('/api/emma/alerts'))
}

export function patchAlerts(chatId) {
  return guard(apiFetch('/api/emma/alerts', { method: 'PATCH', body: { chat_id: chatId } }))
}

export function sendTestAlert() {
  return guard(apiFetch('/api/emma/alerts/test', { method: 'POST' }))
}
