// auth.js — access-токен ТОЛЬКО в памяти модуля (задача M10-1): ни
// localStorage, ни sessionStorage — XSS-читаемых хранилищ у токена нет.
// Продление сессии — refresh-cookie (HttpOnly, Path=/auth, ставит бэкенд M7),
// JS её не видит и не трогает, браузер шлёт сам на POST /auth/refresh.

let accessToken = null
let claims = null // {sub, role, exp} из payload JWT

// listeners — App подписывается, чтобы перерисоваться на login/logout.
const listeners = new Set()

export function onAuthChange(fn) {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

function notify() {
  for (const fn of listeners) fn()
}

export function getToken() {
  return accessToken
}

// getClaims — роль нужна UI для гейта LGPD-панели (задача M10-5). Это
// косметика: настоящая проверка ролей — RequireRole на бэкенде (M7/M8).
export function getClaims() {
  return claims
}

// decodeClaims — payload JWT без проверки подписи: подпись проверяет только
// бэкенд, клиенту из токена нужны лишь role/exp для UI.
export function decodeClaims(token) {
  const part = token.split('.')[1]
  const b64 = part.replace(/-/g, '+').replace(/_/g, '/')
  const json = decodeURIComponent(
    Array.from(atob(b64), (ch) => '%' + ch.charCodeAt(0).toString(16).padStart(2, '0')).join(''),
  )
  return JSON.parse(json)
}

function setToken(token) {
  accessToken = token
  claims = token ? decodeClaims(token) : null
  notify()
}

export class AuthError extends Error {
  constructor(message, code) {
    super(message)
    this.code = code
  }
}

async function postAuth(path, body) {
  const res = await globalThis.fetch(path, {
    method: 'POST',
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
    // refresh-cookie ходит только на /auth/* (Path-scope M7); include — на
    // случай dev-раздачи фронта с другого origin, same-origin не мешает.
    credentials: 'include',
  })
  let data = null
  try {
    data = await res.json()
  } catch {
    /* не-JSON (например, 502 прокси) — ниже уйдёт как AuthError без кода */
  }
  if (!res.ok) {
    throw new AuthError(data?.error || `HTTP ${res.status}`, data?.code || 'ERR_HTTP_' + res.status)
  }
  return data
}

export async function login(email, password) {
  const data = await postAuth('/auth/login', { email, password })
  setToken(data.access_token)
  return claims
}

// refreshInFlight — single-flight: пачка одновременных 401 (доска + карточка)
// не должна жечь несколько refresh-токенов подряд (ротация M7: каждый refresh
// гасит предыдущий, параллельные запросы взаимно инвалидировались бы).
let refreshInFlight = null

// refresh — POST /auth/refresh по cookie. true = новый access-токен получен;
// false = сессия кончилась (refresh просрочен/украден/погашен) → на login.
export async function refresh() {
  if (!refreshInFlight) {
    refreshInFlight = (async () => {
      try {
        const data = await postAuth('/auth/refresh')
        setToken(data.access_token)
        return true
      } catch (err) {
        if (err instanceof AuthError && err.code?.startsWith('ERR_REFRESH')) {
          setToken(null)
          return false
        }
        throw err // сеть/5xx — не конец сессии, пусть решает вызывающий
      } finally {
        refreshInFlight = null
      }
    })()
  }
  return refreshInFlight
}

// logout — на бэкенде M7 ручки logout нет: бросаем токен из памяти, refresh-
// cookie дотлеет по TTL (или будет погашена ротацией при следующем login).
export function logout() {
  setToken(null)
}
