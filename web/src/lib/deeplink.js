// deeplink — карточка лида по ссылке из Telegram-уведомления менеджеру:
// воркер шлёт «Открыть диалог: <crm>/?lead=<id>» (leadCardURL, processor.go),
// App.jsx после входа открывает карточку и стирает параметр из адреса,
// чтобы закрытие карточки и перезагрузка не открывали её заново.
export function consumeLeadParam(loc = window.location, hist = window.history) {
  const params = new URLSearchParams(loc.search)
  const id = Number(params.get('lead'))
  if (!Number.isInteger(id) || id <= 0) return null
  params.delete('lead')
  const rest = params.toString()
  hist.replaceState(null, '', loc.pathname + (rest ? `?${rest}` : ''))
  return id
}
