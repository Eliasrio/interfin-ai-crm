package kanban

import "testing"

// TestCanTransition_Table — таблица переходов §3.1 по акторам.
func TestCanTransition_Table(t *testing.T) {
	cases := []struct {
		name  string
		from  int16
		to    int16
		actor Actor
		want  bool
	}{
		// Авто-триггер по счётчику (§3.2): только 1→2.
		{"system 1→2", StageGrey, StageLive, ActorSystem, true},
		{"system 2→3 запрещён", StageLive, StagePaid, ActorSystem, false},
		{"system 1→8 запрещён", StageGrey, StageFailed, ActorSystem, false},

		// TTL (§3.4): только 4→8 и 6→8.
		{"ttl 4→8", StageUnpaid, StageFailed, ActorTTL, true},
		{"ttl 6→8", StageProposal, StageFailed, ActorTTL, true},
		{"ttl 2→8 запрещён", StageLive, StageFailed, ActorTTL, false},
		{"ttl 5→8 запрещён", StageConsultation, StageFailed, ActorTTL, false},

		// Payment (§3.3): success → 3, fail/underpaid → 4, из любой стадии.
		{"payment 1→3", StageGrey, StagePaid, ActorPayment, true},
		{"payment 2→4", StageLive, StageUnpaid, ActorPayment, true},
		{"payment 4→3 (оплатил после fail)", StageUnpaid, StagePaid, ActorPayment, true},
		{"payment 2→7 запрещён", StageLive, StageSold, ActorPayment, false},

		// Manager: ВСЕГДА переопределяет (§3.1) — любые переходы.
		{"manager 1→5", StageGrey, StageConsultation, ActorManager, true},
		{"manager 2→7", StageLive, StageSold, ActorManager, true},
		{"manager 8→2 (возврат из архива)", StageFailed, StageLive, ActorManager, true},
		{"manager 6→8", StageProposal, StageFailed, ActorManager, true},

		// Границы.
		{"same stage — не переход", StageLive, StageLive, ActorManager, false},
		{"стадия 0 не существует", 0, StageLive, ActorManager, false},
		{"стадия 9 не существует", StageLive, 9, ActorManager, false},
		{"неизвестный актор", StageGrey, StageLive, Actor("bot"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CanTransition(c.from, c.to, c.actor); got != c.want {
				t.Errorf("CanTransition(%d, %d, %s) = %v, ожидали %v",
					c.from, c.to, c.actor, got, c.want)
			}
		})
	}
}
