package auth

import (
	"errors"
	"testing"
	"time"
)

func TestVerifyWSProtocol(t *testing.T) {
	iss, ver := testPair(t, 15*time.Minute)
	token, _ := iss.Issue("42", RoleManager)

	// Одиночный субпротокол и список через запятую (§5.3).
	for _, header := range []string{
		"Bearer." + token,
		"chat, Bearer." + token,
	} {
		claims, proto, err := ver.VerifyWSProtocol(header)
		if err != nil {
			t.Fatalf("%q: %v", header, err)
		}
		if claims.Subject != "42" {
			t.Fatalf("sub = %q", claims.Subject)
		}
		// Сервер обязан эхом вернуть именно этот субпротокол в upgrade-ответе.
		if proto != "Bearer."+token {
			t.Fatalf("proto = %q", proto)
		}
	}
}

// §5.3: просрочка при upgrade — M9 маппит её в close 4001, поэтому
// ошибка обязана быть различимой.
func TestVerifyWSProtocolExpired(t *testing.T) {
	iss, _ := testPair(t, -time.Minute)
	_, ver := testPair(t, 15*time.Minute)
	token, _ := iss.Issue("42", RoleManager)

	_, _, err := ver.VerifyWSProtocol("Bearer." + token)
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("ожидался ErrTokenExpired, получено: %v", err)
	}
}

func TestVerifyWSProtocolMissing(t *testing.T) {
	_, ver := testPair(t, 15*time.Minute)
	for _, header := range []string{"", "chat", "Bearer <не-тот-формат>"} {
		if _, _, err := ver.VerifyWSProtocol(header); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%q: ожидался ErrTokenInvalid, получено %v", header, err)
		}
	}
}
