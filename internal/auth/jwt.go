package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Ошибки проверки токена. Middleware по ним различает коды ответа,
// M9 — причину close 4001 (§5.3).
var (
	ErrTokenExpired = errors.New("auth: access token просрочен")
	ErrTokenInvalid = errors.New("auth: access token невалиден")
)

// Claims — payload access-токена, ровно { sub, role, iat, exp } (§5.1).
// sub — ID менеджера строкой (RFC 7519) либо имя сервиса для role=system.
type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// ManagerID разбирает sub как ID менеджера (для role=admin|manager).
func (c *Claims) ManagerID() (int64, error) {
	id, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("auth: sub %q не ID менеджера: %w", c.Subject, err)
	}
	return id, nil
}

// Issuer подписывает access-токены приватным ключом RS256.
type Issuer struct {
	key *rsa.PrivateKey
	ttl time.Duration
}

func NewIssuer(key *rsa.PrivateKey, ttl time.Duration) *Issuer {
	return &Issuer{key: key, ttl: ttl}
}

// TTL — срок жизни access-токена (expires_in в ответах /auth/*).
func (i *Issuer) TTL() time.Duration { return i.ttl }

// Issue выпускает access-токен с payload { sub, role, iat, exp } (§5.1).
func (i *Issuer) Issue(sub, role string) (string, error) {
	now := time.Now()
	claims := Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sub,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(i.key)
	if err != nil {
		return "", fmt.Errorf("auth: подпись access-токена: %w", err)
	}
	return token, nil
}

// Verifier проверяет access-токены публичным ключом.
type Verifier struct {
	key *rsa.PublicKey
}

func NewVerifier(key *rsa.PublicKey) *Verifier {
	return &Verifier{key: key}
}

// Verify валидирует подпись и exp. Просрочка — ErrTokenExpired, всё
// остальное (подпись, формат, чужой алгоритм) — ErrTokenInvalid.
func (v *Verifier) Verify(token string) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(token, &claims,
		func(*jwt.Token) (interface{}, error) { return v.key, nil },
		// Только RS256: без allowlist подписанный HS256-токен с публичным
		// ключом в роли HMAC-секрета прошёл бы проверку (классическая атака).
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithExpirationRequired(),
	)
	switch {
	case err == nil:
		return &claims, nil
	case errors.Is(err, jwt.ErrTokenExpired):
		return nil, fmt.Errorf("%w: %w", ErrTokenExpired, err)
	default:
		return nil, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}
}
