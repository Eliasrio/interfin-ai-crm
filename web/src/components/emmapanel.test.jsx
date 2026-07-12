// emmapanel.test.jsx — EP-07 задача 10: кнопка «🤖 Эмма» в шапке видна
// только admin (менеджер не должен ВИДЕТЬ раздел, не только получать 403).
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import App from '../App.jsx'
import * as store from '../lib/store.js'
import * as auth from '../lib/auth.js'
import { FakeWebSocket, makeJWT, mockFetch } from '../test/helpers.js'

beforeEach(() => {
  FakeWebSocket.reset()
  globalThis.WebSocket = FakeWebSocket
})

afterEach(() => {
  cleanup()
  store.reset()
  auth.logout()
  delete globalThis.WebSocket
})

async function loginAndRender(role) {
  mockFetch([
    { match: (u) => u === '/auth/login', reply: { body: { access_token: makeJWT(role) } } },
    { match: (u) => u.startsWith('/api/leads'), reply: { body: { leads: [], total: 0 } } },
    { match: (u) => u === '/api/emma/pin/status', reply: { body: { pin_set: true, session_active: false } } },
  ])
  await auth.login('u@x', 'pw')
  render(<App />)
  await screen.findByText('Выйти') // шапка доски отрисована
}

describe('App: раздел Эммы по ролям', () => {
  it('менеджер НЕ видит кнопку «🤖 Эмма» в шапке', async () => {
    await loginAndRender('manager')
    expect(screen.queryByText('🤖 Эмма')).toBeNull()
  })

  it('admin видит кнопку; клик открывает PIN-гейт панели', async () => {
    await loginAndRender('admin')
    fireEvent.click(screen.getByText('🤖 Эмма'))
    await screen.findByText('Введите PIN')
    // Возврат: кнопка превратилась в «📋 Доска».
    fireEvent.click(screen.getByText('📋 Доска'))
    expect(screen.queryByText('Введите PIN')).toBeNull()
  })
})
