// ConnectionBadge — режим доставки обновлений (§10.1): live WS, polling
// (Redis down или WS недоступен), реконнект.
const MODES = {
  connecting: { label: 'подключение…', cls: 'conn-neutral' },
  live: { label: 'live', cls: 'conn-live' },
  polling: { label: 'polling 5s', cls: 'conn-polling' },
  reconnecting: { label: 'реконнект…', cls: 'conn-warn' },
  offline: { label: 'offline', cls: 'conn-warn' },
}

export default function ConnectionBadge({ mode }) {
  const m = MODES[mode] || MODES.connecting
  return (
    <span className={'conn ' + m.cls} data-connection={mode}>
      ● {m.label}
    </span>
  )
}
