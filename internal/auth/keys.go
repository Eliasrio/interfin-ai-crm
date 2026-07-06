// Package auth — JWT RS256 + Refresh Token (SRS §5, M7).
//
// Контракт наружу (task-файл M7):
//   - Middleware/RequireRole — защита REST-роутов (M8);
//   - Verifier.VerifyWSProtocol — auth при WebSocket upgrade (M9, §5.3);
//   - роли RoleAdmin/RoleManager/RoleSystem — гейт эндпоинтов (§5.2).
//
// Ключи RS256 приходят ТОЛЬКО файлами (Docker secrets jwt_private.pem /
// jwt_public.pem, CLAUDE.md §4.9) — ни ключей, ни PEM-литералов в коде.
package auth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// Роли §5.2. admin и manager живут в managers.role (CHECK в БД);
// system — internal-токены сервисов, в таблице не хранится.
const (
	RoleAdmin   = "admin"
	RoleManager = "manager"
	RoleSystem  = "system"
)

// LoadKeys читает пару RSA из PEM-файлов по путям из конфига
// (в проде — /run/secrets/jwt_private, /run/secrets/jwt_public).
func LoadKeys(privatePath, publicPath string) (*rsa.PrivateKey, *rsa.PublicKey, error) {
	priv, err := loadPrivateKey(privatePath)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: private key %s: %w", privatePath, err)
	}
	pub, err := loadPublicKey(publicPath)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: public key %s: %w", publicPath, err)
	}
	return priv, pub, nil
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	block, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	// PKCS#8 (openssl genpkey — так генерирует scripts/gen_jwt_keys.sh),
	// запасной вариант — PKCS#1 (openssl genrsa).
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("не RSA-ключ: %T", key)
		}
		return rsaKey, nil
	}
	rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("не удалось разобрать ни как PKCS#8, ни как PKCS#1: %w", err)
	}
	return rsaKey, nil
}

func loadPublicKey(path string) (*rsa.PublicKey, error) {
	block, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	// PKIX (openssl pkey -pubout), запасной вариант — PKCS#1.
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("не RSA-ключ: %T", key)
		}
		return rsaKey, nil
	}
	rsaKey, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("не удалось разобрать ни как PKIX, ни как PKCS#1: %w", err)
	}
	return rsaKey, nil
}

func readPEM(path string) (*pem.Block, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("файл не содержит PEM-блока")
	}
	return block, nil
}
