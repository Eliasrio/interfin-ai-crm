// takeover.js — состояние режима диалога лида (M13). Без React, чистые
// функции: компоненты и тесты дёргают напрямую.
//
// Модель (двухпольная, hybrid): dialog_mode 'bot'|'human' +
// bot_silenced_until (пауза автопилота). UI-состояния из task M13:
//   human                      → «✋ ведёт менеджер»;
//   bot + пауза в будущем      → «⏸ Эмма на паузе до HH:MM»;
//   иначе                      → «🤖 ведёт Эмма».

// dialogState — вычислимое UI-состояние карточки: {state, icon, label}.
// now — инъекция времени для тестов (по умолчанию — настоящее).
export function dialogState(lead, now = new Date()) {
  if (lead.dialog_mode === 'human') {
    const who = lead.taken_by != null ? ` (менеджер #${lead.taken_by})` : ''
    return { state: 'human', icon: '✋', label: `ведёт менеджер${who}` }
  }
  const until = lead.bot_silenced_until ? new Date(lead.bot_silenced_until) : null
  if (until && until > now) {
    const hhmm = until.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' })
    return { state: 'paused', icon: '⏸', label: `Эмма на паузе до ${hhmm}` }
  }
  return { state: 'bot', icon: '🤖', label: 'ведёт Эмма' }
}
