// socket.js — WebSocket-клиент доски (задача M10-3, SRS §10).
//
// Протокол M9: GET /ws/kanban, JWT в Sec-WebSocket-Protocol `Bearer.<token>`;
// истёкший токен → close 4001. Поток — события events.Event плюс два
// служебных сигнала Hub'а: {type:"polling_mode",poll_interval_sec} при
// падении Redis pub/sub и {type:"live_mode"} при восстановлении.
//
// Реконнект — дословно §10.3 (AQ²-fix #5):
//   1) обрыв: last_event_ts уже в store;
//   2) POST /auth/refresh → новый access token;
//   3) GET /api/leads?updated_since=<last_event_ts> ПЕРЕД resubscribe —
//      Redis pub/sub fire-and-forget, окно обрыва добирается только так;
//   4) новый WS, дальше live.
//
// Недоступность WS (бэкенд лежит/сеть): после второй неудачи подряд —
// polling-fallback GET /api/leads раз в 5 с, попытки реконнекта продолжаются
// с экспоненциальной паузой; первый же удачный open гасит polling.

import * as auth from './auth.js'
import * as api from './api.js'
import * as store from './store.js'
import * as chat from './chat.js'
import * as emma from './emma.js'

const FALLBACK_POLL_MS = 5000 // §10.1: polling mode — 5 с
const RECONNECT_BASE_MS = 500
const RECONNECT_MAX_MS = 30_000

export class KanbanSocket {
  constructor(opts = {}) {
    this.wsUrl = opts.wsUrl || defaultWSUrl()
    this.log = opts.log || console
    // Тестовые крючки: e2e придерживает реконнект, чтобы успеть «потерять»
    // событие; фейковый WebSocket подменяется через globalThis.
    this.reconnectBaseMs = opts.reconnectBaseMs ?? RECONNECT_BASE_MS
    this.pollMs = opts.pollMs ?? FALLBACK_POLL_MS

    this.ws = null
    this.stopped = true
    this.attempts = 0 // неудачные попытки подряд — backoff и порог fallback
    this.reconnectTimer = null
    this.pollTimer = null
    this.fetchingLeads = new Set() // дедуп догрузки неизвестных лидов
  }

  // start — начальная загрузка доски + первое подключение.
  async start() {
    this.stopped = false
    store.setConnection('connecting')
    try {
      const leads = await api.fetchAllLeads()
      store.applyLeads(leads, { sliceTs: new Date().toISOString() })
    } catch (err) {
      // Доска пустая не останется: реконнект-цикл сделает catch-up без
      // updated_since (полный срез), когда бэкенд оживёт.
      this.log.warn('kanban: начальная загрузка не удалась', err)
    }
    this.connect()
  }

  stop() {
    this.stopped = true
    clearTimeout(this.reconnectTimer)
    this.stopPolling()
    if (this.ws) {
      const ws = this.ws
      this.ws = null
      ws.close(1000, 'client shutdown')
    }
  }

  connect() {
    if (this.stopped) return
    const token = auth.getToken()
    if (!token) return // сессии нет — App уже на login-форме
    let ws
    try {
      ws = new globalThis.WebSocket(this.wsUrl, 'Bearer.' + token)
    } catch (err) {
      this.log.warn('kanban: WebSocket не создался', err)
      this.scheduleReconnect()
      return
    }
    this.ws = ws

    ws.onopen = () => {
      if (this.ws !== ws) return
      this.attempts = 0
      this.stopPolling()
      store.setConnection('live')
    }

    ws.onmessage = (msg) => {
      if (this.ws !== ws) return
      let ev
      try {
        ev = JSON.parse(msg.data)
      } catch {
        return
      }
      this.handleMessage(ev)
    }

    ws.onclose = () => {
      if (this.ws !== ws) return // устаревший сокет или наш же stop()
      this.ws = null
      this.stopPolling() // hub-polling умер вместе с соединением
      this.scheduleReconnect()
    }

    ws.onerror = () => {
      /* за onerror всегда следует onclose — обрабатываем там */
    }
  }

  handleMessage(ev) {
    switch (ev.type) {
      case 'polling_mode':
        // Redis pub/sub упал (§10.1): WS живой, но событий не будет —
        // опрашиваем REST с интервалом, который назначил Hub.
        this.startPolling((ev.poll_interval_sec || 5) * 1000)
        store.setConnection('polling')
        return
      case 'live_mode':
        // Pub/sub ожил. Окно деградации могло потерять события — добираем
        // catch-up'ом, как при реконнекте (контракт M9).
        this.stopPolling()
        this.catchUp().catch((err) => this.log.warn('kanban: catch-up после live_mode', err))
        store.setConnection('live')
        return
      case 'emma_kb_status':
        // EP-07: финал индексации файла базы знаний — событие панели, не
        // про лида (lead_id нет): доске не отдаём, иначе fetchUnknownLead
        // дёрнул бы /api/leads/undefined.
        emma.applyKbEvent(ev)
        return
      default: {
        // M12: событие message уходит и в чат-панель. Доске оно тоже
        // отдаётся: store игнорирует незнакомый тип, но двигает lastEventTs,
        // а по неизвестному лиду карточка дотягивается по REST.
        if (ev.type === 'message') chat.applyEvent(ev)
        const known = store.applyEvent(ev)
        if (!known) this.fetchUnknownLead(ev.lead_id)
      }
    }
  }

  // fetchUnknownLead — событие по лиду, которого нет на доске (создан после
  // загрузки среза): дотягиваем карточку по REST, один запрос на лид.
  fetchUnknownLead(id) {
    if (this.fetchingLeads.has(id)) return
    this.fetchingLeads.add(id)
    api
      .fetchLead(id)
      .then((data) => store.applyLeads([data.lead]))
      .catch((err) => this.log.warn('kanban: догрузка лида ' + id, err))
      .finally(() => this.fetchingLeads.delete(id))
  }

  scheduleReconnect() {
    if (this.stopped) return
    // 4001 (access-токен истёк, §5.3) и прочие обрывы идут одним путём:
    // §10.3 велит обновлять токен в каждом цикле реконнекта.
    const delay = Math.min(this.reconnectBaseMs * 2 ** this.attempts, RECONNECT_MAX_MS)
    this.attempts++
    // WS недоступен вторую попытку подряд — доска переезжает на polling
    // (задача M10-3), реконнект продолжает стучаться с backoff'ом.
    if (this.attempts >= 2) this.startPolling(this.pollMs)
    store.setConnection(this.pollTimer ? 'polling' : 'reconnecting')
    clearTimeout(this.reconnectTimer)
    this.reconnectTimer = setTimeout(() => {
      this.reconnect().catch((err) => {
        this.log.warn('kanban: реконнект не удался', err)
        this.scheduleReconnect()
      })
    }, delay)
  }

  // reconnect — шаги 2–4 из §10.3.
  async reconnect() {
    if (this.stopped) return
    const ok = await auth.refresh() // шаг 2
    if (!ok) {
      // Refresh-токен погашен/просрочен — сессия кончилась; auth.refresh()
      // уже сбросил токен, App перерисуется в LoginForm.
      this.stop()
      return
    }
    await this.catchUp() // шаг 3 — строго ПЕРЕД resubscribe
    this.connect() // шаг 4
  }

  // catchUp — GET /api/leads?updated_since=<last_event_ts> (§10.3 шаг 3).
  // Нет last_event_ts (первый коннект не удался) — полный срез.
  async catchUp() {
    const since = store.getState().lastEventTs
    const leads = await api.fetchAllLeads(since ? { updatedSince: since } : {})
    store.applyLeads(leads, { sliceTs: since ? undefined : new Date().toISOString() })
  }

  startPolling(intervalMs) {
    if (this.pollTimer) return
    store.setConnection('polling')
    const tick = async () => {
      try {
        await this.catchUp()
      } catch (err) {
        this.log.warn('kanban: polling не удался', err)
        if (err instanceof api.ApiError && err.code === 'ERR_SESSION_EXPIRED') this.stop()
      }
    }
    tick()
    this.pollTimer = setInterval(tick, intervalMs)
  }

  stopPolling() {
    if (!this.pollTimer) return
    clearInterval(this.pollTimer)
    this.pollTimer = null
  }
}

function defaultWSUrl() {
  const { protocol, host } = globalThis.location
  return (protocol === 'https:' ? 'wss://' : 'ws://') + host + '/ws/kanban'
}
