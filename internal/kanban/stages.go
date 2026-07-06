// Package kanban — state machine переходов лида по 8 стадиям (SRS §3, M5).
//
// stages.go — статическая часть: стадии, акторы и таблица допустимых
// переходов §3.1. Динамика (CAS-переход, side effects) — в machine.go.
package kanban

// Стадии Kanban-доски (SRS §3.1). Значения зеркалят leads.stage_id
// (SMALLINT, DEFAULT 1 — миграция 0003_leads).
const (
	StageGrey         int16 = 1 // Серые лиды: ≤5 inbound
	StageLive         int16 = 2 // Живые лиды: 6+ inbound
	StagePaid         int16 = 3 // Оплачено: ждут ссылку
	StageUnpaid       int16 = 4 // Не оплачено (error/timeout/underpaid), TTL 48ч → 8
	StageConsultation int16 = 5 // Консультация назначена
	StageProposal     int16 = 6 // Отправлено предложение, TTL 5 дн → 8
	StageSold         int16 = 7 // Продано
	StageFailed       int16 = 8 // Не удалось (архив)
)

// AutoAdvanceInbound — §3.2: с 6-го inbound-сообщения лид «живой» (1→2).
// message_count считает ТОЛЬКО inbound (CLAUDE.md §4.3), поэтому сравнение
// идёт по нему без поправок.
const AutoAdvanceInbound = 6

// Actor — кто инициирует переход. Приоритет §3.1: Manual Manager и Payment
// Webhook ВСЕГДА переопределяют авто-триггеры. В таблице переходов это
// широкие права Manager/Payment; в рантайме — CAS в repo.TransitionStage:
// авто-переход, посчитанный по устаревшей стадии, промахивается мимо
// guard'а WHERE stage_id=from и отбрасывается, а не перетирает ручной.
type Actor string

const (
	ActorSystem  Actor = "system"  // авто-триггер по счётчику (§3.2)
	ActorTTL     Actor = "ttl"     // истечение TTL (§3.4)
	ActorManager Actor = "manager" // ручной перевод, PATCH /api/leads/:id/stage (M8)
	ActorPayment Actor = "payment" // платёжный вебхук (M6)
)

// ValidStage — stage_id в пределах доски §3.1.
func ValidStage(s int16) bool { return s >= StageGrey && s <= StageFailed }

// CanTransition — таблица допустимых переходов §3.1 (from != to; равенство
// стадий — не переход, machine разбирает его отдельно как touch/ретрай).
//
//   - Manager: любой переход, включая возврат из архива (8→N) — «Manual
//     Manager ВСЕГДА переопределяет», финальное слово за человеком.
//   - Payment: success → 3, error/timeout/underpaid → 4 (§3.3), из любой
//     стадии — деньги могут прийти когда угодно.
//   - System: только 1→2 по счётчику (§3.2).
//   - TTL: только 4→8 и 6→8 (§3.4).
func CanTransition(from, to int16, actor Actor) bool {
	if !ValidStage(from) || !ValidStage(to) || from == to {
		return false
	}
	switch actor {
	case ActorManager:
		return true
	case ActorPayment:
		return to == StagePaid || to == StageUnpaid
	case ActorSystem:
		return from == StageGrey && to == StageLive
	case ActorTTL:
		return (from == StageUnpaid || from == StageProposal) && to == StageFailed
	default:
		return false
	}
}
