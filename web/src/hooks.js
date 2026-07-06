// hooks.js — мост между модулями lib/ (без React) и компонентами.
import { useSyncExternalStore } from 'react'
import * as store from './lib/store.js'
import * as auth from './lib/auth.js'

// useStore — снапшот доски; store иммутабельный, ссылки стабильны.
export function useStore() {
  return useSyncExternalStore(store.subscribe, store.getState)
}

// useClaims — {sub, role, exp} текущей сессии или null (→ LoginForm).
export function useClaims() {
  return useSyncExternalStore(auth.onAuthChange, auth.getClaims)
}
