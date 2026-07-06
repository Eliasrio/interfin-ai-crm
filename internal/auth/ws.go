package auth

import (
	"fmt"
	"strings"
)

// wsProtocolPrefix — субпротокол-носитель токена (§5.3): браузерный
// WebSocket API не умеет ставить Authorization, поэтому JWT едет в
// Sec-WebSocket-Protocol: Bearer.<token>.
const wsProtocolPrefix = "Bearer."

// VerifyWSProtocol валидирует JWT из заголовка Sec-WebSocket-Protocol при
// upgrade (§5.3, использует M9). Заголовок — список субпротоколов через
// запятую; берётся первый вида "Bearer.<token>".
//
// Возвращает claims и сам субпротокол: сервер ОБЯЗАН эхом вернуть его в
// Sec-WebSocket-Protocol ответа, иначе браузер разрывает соединение.
// Просрочка — ErrTokenExpired (M9 маппит в close 4001 → reconnect §10.3),
// отсутствие/битость — ErrTokenInvalid.
func (v *Verifier) VerifyWSProtocol(header string) (*Claims, string, error) {
	for _, proto := range strings.Split(header, ",") {
		proto = strings.TrimSpace(proto)
		if !strings.HasPrefix(proto, wsProtocolPrefix) {
			continue
		}
		claims, err := v.Verify(proto[len(wsProtocolPrefix):])
		if err != nil {
			return nil, "", err
		}
		return claims, proto, nil
	}
	return nil, "", fmt.Errorf("%w: в Sec-WebSocket-Protocol нет %s<token>",
		ErrTokenInvalid, wsProtocolPrefix)
}
