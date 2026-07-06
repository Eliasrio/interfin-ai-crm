package auth

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/google/uuid"
)

// NewRefreshToken — opaque UUID (§5.1). Клиенту уходит в HttpOnly cookie,
// сервер его нигде не сохраняет — только хеш (HashRefreshToken).
func NewRefreshToken() string {
	return uuid.NewString()
}

// HashRefreshToken — hex(SHA-256) для refresh_tokens.token_hash: утечка
// таблицы не даёт предъявимых токенов. bcrypt здесь не нужен — вход
// высокоэнтропийный UUID, перебор бессмыслен, а SHA-256 дешёв на каждый
// /auth/refresh.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
