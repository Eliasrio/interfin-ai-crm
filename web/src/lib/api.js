// api.js — обёртка над fetch для /api/* (задача M10-6): Authorization из
// памяти, ошибки в формате {"error","code"} (§4.2), 401 → тихий refresh и
// ровно один повтор; refresh не помог → сессия кончилась, App уводит на login.
import { getToken, refresh } from './auth.js'

export class ApiError extends Error {
  constructor(message, code, status) {
    super(message)
    this.code = code
    this.status = status
  }
}

async function rawFetch(path, { method = 'GET', body } = {}) {
  const headers = { Authorization: 'Bearer ' + getToken() }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  return globalThis.fetch(path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
}

export async function apiFetch(path, opts = {}) {
  let res = await rawFetch(path, opts)
  if (res.status === 401) {
    // Тихий refresh (задача M10-6): access живёт 15 мин (§5.1), протухание —
    // штатный ритм, а не ошибка. Не продлилось — logout внутри refresh()
    // уже уронил токен, App перерисуется в LoginForm.
    const ok = await refresh()
    if (!ok) throw new ApiError('сессия истекла', 'ERR_SESSION_EXPIRED', 401)
    res = await rawFetch(path, opts)
  }
  let data = null
  try {
    data = await res.json()
  } catch {
    /* тело не JSON — для ok-ответов бэкенд M8 так не делает */
  }
  if (!res.ok) {
    throw new ApiError(data?.error || `HTTP ${res.status}`, data?.code || 'ERR_HTTP_' + res.status, res.status)
  }
  return data
}

// --- Ручки M8 (§4.1) ---

export function fetchLeadsPage({ updatedSince, limit = 200, offset = 0 } = {}) {
  const q = new URLSearchParams({ limit: String(limit), offset: String(offset) })
  if (updatedSince) q.set('updated_since', updatedSince)
  return apiFetch('/api/leads?' + q)
}

// fetchAllLeads — выбирает все страницы (limit капнут бэкендом на 200).
// safetyCap — защита от бесконечного цикла при рассинхроне total.
export async function fetchAllLeads({ updatedSince } = {}, safetyCap = 50) {
  const all = []
  for (let page = 0; page < safetyCap; page++) {
    const { leads, total } = await fetchLeadsPage({ updatedSince, offset: all.length })
    all.push(...leads)
    if (all.length >= total || leads.length === 0) break
  }
  return all
}

export function fetchLead(id) {
  return apiFetch('/api/leads/' + id)
}

export function patchStage(id, stageId) {
  return apiFetch(`/api/leads/${id}/stage`, { method: 'PATCH', body: { stage_id: stageId } })
}

export function lgpdErase(id) {
  return apiFetch(`/api/lgpd/leads/${id}/erase`, { method: 'DELETE' })
}

export function lgpdExport(id) {
  return apiFetch(`/api/lgpd/leads/${id}/export`)
}
