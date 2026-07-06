package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKey — одна RSA-пара на весь пакет: генерация 2048 бит не бесплатна.
var (
	testKeyOnce sync.Once
	testKeyVal  *rsa.PrivateKey
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		testKeyVal = k
	})
	return testKeyVal
}

func testPair(t *testing.T, ttl time.Duration) (*Issuer, *Verifier) {
	t.Helper()
	key := testKey(t)
	return NewIssuer(key, ttl), NewVerifier(&key.PublicKey)
}

func TestIssueVerifyRoundtrip(t *testing.T) {
	iss, ver := testPair(t, 15*time.Minute)

	token, err := iss.Issue("42", RoleManager)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := ver.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "42" || claims.Role != RoleManager {
		t.Fatalf("claims: sub=%q role=%q", claims.Subject, claims.Role)
	}
	id, err := claims.ManagerID()
	if err != nil || id != 42 {
		t.Fatalf("ManagerID: %d, %v", id, err)
	}
}

// Payload — ровно { sub, role, iat, exp } (§5.1): ни поля больше, ни меньше.
func TestPayloadShape(t *testing.T) {
	iss, _ := testPair(t, 15*time.Minute)
	token, err := iss.Issue("7", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("не JWT: %d частей", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sub", "role", "iat", "exp"} {
		if _, ok := payload[want]; !ok {
			t.Errorf("в payload нет %q", want)
		}
	}
	if len(payload) != 4 {
		t.Errorf("payload должен быть ровно {sub,role,iat,exp}, получено: %v", payload)
	}
	// TTL access-токена — 15 минут (§5.1).
	if exp, iat := payload["exp"].(float64), payload["iat"].(float64); exp-iat != 900 {
		t.Errorf("exp-iat = %v, ожидалось 900", exp-iat)
	}
}

// AQ²-2: просроченный токен обязан различаться от битого — middleware
// отвечает разными кодами.
func TestVerifyExpired(t *testing.T) {
	iss, ver := testPair(t, -time.Minute) // exp уже в прошлом
	token, err := iss.Issue("42", RoleManager)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ver.Verify(token)
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("ожидался ErrTokenExpired, получено: %v", err)
	}
}

func TestVerifyTampered(t *testing.T) {
	iss, ver := testPair(t, 15*time.Minute)
	token, err := iss.Issue("42", RoleManager)
	if err != nil {
		t.Fatal(err)
	}
	// Подмена payload при живой подписи.
	parts := strings.Split(token, ".")
	forged, _ := json.Marshal(map[string]interface{}{
		"sub": "42", "role": RoleAdmin,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	parts[1] = base64.RawURLEncoding.EncodeToString(forged)
	_, err = ver.Verify(strings.Join(parts, "."))
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("подделанный токен прошёл: %v", err)
	}
}

func TestVerifyWrongKey(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := NewIssuer(otherKey, 15*time.Minute)
	_, ver := testPair(t, 15*time.Minute)

	token, err := iss.Issue("42", RoleManager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ver.Verify(token); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("токен под чужим ключом прошёл: %v", err)
	}
}

// Классическая атака подмены алгоритма: HS256-токен, «подписанный» публичным
// ключом как HMAC-секретом, обязан отклоняться allowlist'ом RS256.
func TestVerifyRejectsHS256(t *testing.T) {
	_, ver := testPair(t, 15*time.Minute)
	claims := Claims{
		Role: RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "42",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte("какой-то-секрет"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ver.Verify(token); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("HS256-токен прошёл RS256-верификацию: %v", err)
	}
}

func TestVerifyGarbage(t *testing.T) {
	_, ver := testPair(t, 15*time.Minute)
	for _, tok := range []string{"", "мусор", "a.b.c"} {
		if _, err := ver.Verify(tok); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("Verify(%q): ожидался ErrTokenInvalid, получено %v", tok, err)
		}
	}
}
