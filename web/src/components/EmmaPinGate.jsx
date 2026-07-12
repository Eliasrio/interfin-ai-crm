// EmmaPinGate — PIN-экраны панели Эммы (EP-07 задача 2, ТЗ §2.2): установка
// (bootstrap, два поля), ввод, блокировка брутфорса с таймером из retry_after.
// Здесь же — модалка смены PIN (старый + новый дважды, «настройки панели»).
import { useEffect, useState } from 'react'
import { ApiError } from '../lib/api.js'
import * as emma from '../lib/emma.js'
import * as store from '../lib/store.js'

const PIN_RE = /^\d{6}$/

// useLockTimer — обратный отсчёт 429 PIN_LOCKED: поля выключены, пока не
// дотикает retry_after секунд (сервер всё равно ответит 429 раньше срока).
function useLockTimer() {
  const [left, setLeft] = useState(0)
  const ticking = left > 0
  useEffect(() => {
    if (!ticking) return undefined
    const t = setInterval(() => setLeft((v) => (v > 1 ? v - 1 : 0)), 1000)
    return () => clearInterval(t)
  }, [ticking])
  return { left, start: setLeft }
}

function fmtLeft(sec) {
  return `${Math.floor(sec / 60)}:${String(sec % 60).padStart(2, '0')}`
}

// submitPIN — общий обработчик verify/setup/change: 429 взводит таймер,
// прочие ошибки — в текст. Возвращает true при успехе.
async function submitPIN(call, startLock, setError) {
  try {
    await call()
    return true
  } catch (err) {
    if (err instanceof ApiError && err.code === 'PIN_LOCKED') {
      startLock(err.data?.retry_after || 15 * 60)
      setError(null)
    } else {
      setError(emma.errorText(err))
    }
    return false
  }
}

// mode: 'setup' (PIN не задан — bootstrap) | 'enter' (задан, сессии нет).
export default function EmmaPinGate({ mode, onSuccess }) {
  const [pin, setPin] = useState('')
  const [pin2, setPin2] = useState('')
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const { left, start } = useLockTimer()
  const locked = left > 0
  const isSetup = mode === 'setup'
  const mismatch = isSetup && pin2 !== '' && pin !== pin2
  const valid = PIN_RE.test(pin) && (!isSetup || pin === pin2)

  const submit = async (e) => {
    e.preventDefault()
    if (!valid || busy || locked) return
    setBusy(true)
    setError(null)
    const ok = await submitPIN(() => (isSetup ? emma.pinSetup(pin) : emma.pinVerify(pin)), start, setError)
    setBusy(false)
    if (ok) {
      onSuccess()
      return
    }
    setPin('')
    setPin2('')
  }

  return (
    <div className="emma-gate">
      <form className="login-form" onSubmit={submit} aria-label={isSetup ? 'Установка PIN' : 'Ввод PIN'}>
        <h1>{isSetup ? 'Установите PIN' : 'Введите PIN'}</h1>
        {isSetup && <div className="muted">Панель Эммы защищена PIN-кодом из 6 цифр. Задайте его один раз.</div>}
        <label>
          PIN (6 цифр)
          <input
            aria-label="PIN"
            type="password"
            inputMode="numeric"
            maxLength={6}
            autoComplete="off"
            disabled={locked || busy}
            value={pin}
            onChange={(e) => setPin(e.target.value.replace(/\D/g, ''))}
          />
        </label>
        {isSetup && (
          <label>
            Повторите PIN
            <input
              aria-label="Повторите PIN"
              type="password"
              inputMode="numeric"
              maxLength={6}
              autoComplete="off"
              disabled={locked || busy}
              value={pin2}
              onChange={(e) => setPin2(e.target.value.replace(/\D/g, ''))}
            />
          </label>
        )}
        {mismatch && <div className="form-error">PIN-коды не совпадают.</div>}
        {locked && (
          <div className="form-error" data-testid="pin-lock-timer">
            Слишком много неверных попыток. Ввод заблокирован ещё {fmtLeft(left)}.
          </div>
        )}
        {error && !locked && <div className="form-error">{error}</div>}
        <button className="btn btn-primary" type="submit" disabled={!valid || busy || locked}>
          {busy ? 'Проверяем…' : isSetup ? 'Установить PIN' : 'Войти'}
        </button>
      </form>
    </div>
  )
}

// EmmaPinChangeModal — смена PIN из настроек панели: старый + новый дважды.
// Активная сессия при смене не рвётся (контракт EP-01).
export function EmmaPinChangeModal({ onClose }) {
  const [oldPin, setOldPin] = useState('')
  const [pin, setPin] = useState('')
  const [pin2, setPin2] = useState('')
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const { left, start } = useLockTimer()
  const locked = left > 0
  const mismatch = pin2 !== '' && pin !== pin2
  const valid = PIN_RE.test(oldPin) && PIN_RE.test(pin) && pin === pin2

  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const submit = async (e) => {
    e.preventDefault()
    if (!valid || busy || locked) return
    setBusy(true)
    setError(null)
    const ok = await submitPIN(() => emma.pinChange(oldPin, pin), start, setError)
    setBusy(false)
    if (ok) {
      store.pushAlert('ok', 'PIN изменён')
      onClose()
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal modal-settings" role="dialog" aria-label="Смена PIN" onClick={(e) => e.stopPropagation()}>
        <header className="modal-head">
          <h2>Смена PIN</h2>
          <button className="btn btn-ghost" onClick={onClose} aria-label="Закрыть">
            ✕
          </button>
        </header>
        <form className="modal-section emma-pin-change" onSubmit={submit}>
          <label className="settings-field">
            <span>Старый PIN</span>
            <input
              aria-label="Старый PIN"
              type="password"
              inputMode="numeric"
              maxLength={6}
              autoComplete="off"
              disabled={locked || busy}
              value={oldPin}
              onChange={(e) => setOldPin(e.target.value.replace(/\D/g, ''))}
            />
          </label>
          <label className="settings-field">
            <span>Новый PIN (6 цифр)</span>
            <input
              aria-label="Новый PIN"
              type="password"
              inputMode="numeric"
              maxLength={6}
              autoComplete="off"
              disabled={locked || busy}
              value={pin}
              onChange={(e) => setPin(e.target.value.replace(/\D/g, ''))}
            />
          </label>
          <label className="settings-field">
            <span>Новый PIN ещё раз</span>
            <input
              aria-label="Новый PIN ещё раз"
              type="password"
              inputMode="numeric"
              maxLength={6}
              autoComplete="off"
              disabled={locked || busy}
              value={pin2}
              onChange={(e) => setPin2(e.target.value.replace(/\D/g, ''))}
            />
          </label>
          {mismatch && <div className="form-error">Новые PIN-коды не совпадают.</div>}
          {locked && (
            <div className="form-error" data-testid="pin-lock-timer">
              Слишком много неверных попыток. Подождите {fmtLeft(left)}.
            </div>
          )}
          {error && !locked && <div className="form-error">{error}</div>}
          <button className="btn btn-primary" type="submit" disabled={!valid || busy || locked}>
            {busy ? 'Меняем…' : 'Сменить PIN'}
          </button>
        </form>
      </div>
    </div>
  )
}
