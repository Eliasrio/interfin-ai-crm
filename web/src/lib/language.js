// language.js — язык клиента (M14). Ровно три языка (решение владельца);
// тройка зашита constant'ой — управление списком из CRM появится в M15.
// Без React, чистые данные: компоненты и тесты дёргают напрямую.

export const LANGUAGES = [
  { code: 'ru', badge: '🇷🇺 RU' },
  { code: 'en', badge: '🇬🇧 EN' },
  { code: 'es', badge: '🇪🇸 ES' },
]

// languageBadge — бейдж карточки/модалки; null (язык не определён) и любой
// код вне тройки → null: бейдж не показывается.
export function languageBadge(code) {
  return LANGUAGES.find((l) => l.code === code)?.badge ?? null
}
