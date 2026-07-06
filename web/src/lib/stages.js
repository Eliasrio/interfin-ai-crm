// stages.js — 8 стадий доски, зеркало SRS §3.1 и internal/kanban/stages.go.
// ttlMs — клиентская подсказка дедлайна (§3.4: 48ч у стадии 4, 5 дней у 6);
// боевой отсчёт ведёт бэкенд от last_activity_at (CLAUDE.md §4.7), здесь —
// только индикатор на карточке.

export const STAGES = [
  { id: 1, title: 'Серые лиды', hint: '≤ 5 inbound' },
  { id: 2, title: 'Живые лиды', hint: '6+ inbound' },
  { id: 3, title: 'Оплачено: ждут ссылку', hint: 'payment success' },
  { id: 4, title: 'Не оплачено', hint: 'TTL 48 ч → 8', ttlMs: 48 * 3600 * 1000 },
  { id: 5, title: 'Консультация назначена', hint: 'manual' },
  { id: 6, title: 'Отправлено предложение', hint: 'TTL 5 дн → 8', ttlMs: 5 * 24 * 3600 * 1000 },
  { id: 7, title: 'Продано', hint: 'manual' },
  { id: 8, title: 'Не удалось', hint: 'архив' },
]

export function stageById(id) {
  return STAGES.find((s) => s.id === id)
}

// Кнопки ручных переходов на карточке лида (задача M10-4: Stage 5/6/7).
// Сам drag-and-drop не ограничен: у актора manager по §3.1 любой переход.
export const MANUAL_TARGETS = [5, 6, 7]
