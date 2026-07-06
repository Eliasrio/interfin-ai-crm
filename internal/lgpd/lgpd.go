// Package lgpd — примитивы LGPD-комплаенса (SRS §9, M8): анонимизация
// telegram_user_id при erasure и константы, разделяемые repo/handlers/worker.
//
// Ключевой инвариант (§9.3, AQ²-fix #4): колонка telegram_user_id —
// BIGINT NOT NULL UNIQUE, при erasure она НЕ NULL-ится, а заменяется
// хешем hash(user_id + salt). Требования к хешу:
//   - детерминированность при фиксированной соли (повторный erasure-запрос
//     не плодит новые значения);
//   - необратимость без соли (соль — секрет, LGPD_SALT из env, §4.9);
//   - невозможность коллизии с живым пользователем: Telegram выдаёт только
//     положительные ID, поэтому хеш приводится к строго отрицательному
//     диапазону — стёртый лид никогда не совпадёт с реальным входящим
//     telegram_user_id и не «воскреснет» в ingestion-пайплайне.
package lgpd

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
)

// DeletedContent — значение messages.content после erasure (§9.1).
const DeletedContent = "[DELETED]"

// Действия lgpd_audit (§9.2).
const (
	ActionErase  = "erase"
	ActionExport = "export"
)

// FinancialRecordsNote — явная пометка о сохранении финансовых записей,
// обязательная в ответах erasure и export (§9.3: фискальная retention
// 5 лет превалирует над правом на забвение, LGPD Art. 7, X).
const FinancialRecordsNote = "payment_events сохранены: фискальная retention 5 лет " +
	"(Receita Federal) превалирует над правом на забвение (LGPD Art. 7, X)"

// HashTelegramUserID — анонимизирующая замена telegram_user_id (§9.3):
// первые 8 байт SHA-256(id + salt), приведённые к отрицательному int64.
// 0 исключён (нулевого «пустого» значения не возникает).
func HashTelegramUserID(tgUserID int64, salt string) int64 {
	sum := sha256.Sum256([]byte(strconv.FormatInt(tgUserID, 10) + salt))
	v := int64(binary.BigEndian.Uint64(sum[:8]) & 0x7FFFFFFFFFFFFFFF)
	if v == 0 {
		v = 1
	}
	return -v
}
