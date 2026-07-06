// Toasts — лента алертов store (anti-spam, эскалации, платежи, TTL, ошибки
// API). Автоскрытие через 8 с; клик закрывает сразу.
import { useEffect } from 'react'

export default function Toasts({ alerts, onDismiss }) {
  useEffect(() => {
    if (!alerts.length) return undefined
    const timers = alerts.map((a) => setTimeout(() => onDismiss(a.id), 8000))
    return () => timers.forEach(clearTimeout)
  }, [alerts, onDismiss])

  if (!alerts.length) return null
  return (
    <div className="toasts">
      {alerts.map((a) => (
        <div key={a.id} className={'toast toast-' + a.kind} onClick={() => onDismiss(a.id)}>
          {a.text}
        </div>
      ))}
    </div>
  )
}
