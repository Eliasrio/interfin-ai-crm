package auth

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestKeys пишет пару PEM в форматах скрипта gen_jwt_keys.sh
// (PKCS#8 приватный, PKIX публичный).
func writeTestKeys(t *testing.T) (privPath, pubPath string) {
	t.Helper()
	key := testKey(t)
	dir := t.TempDir()

	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privPath = filepath.Join(dir, "jwt_private.pem")
	writePEM(t, privPath, "PRIVATE KEY", privDER)

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPath = filepath.Join(dir, "jwt_public.pem")
	writePEM(t, pubPath, "PUBLIC KEY", pubDER)
	return privPath, pubPath
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatal(err)
	}
}

// Ключи из файлов (Docker secrets, задача M7-1): загрузка + рабочий
// roundtrip выпуск→проверка.
func TestLoadKeysRoundtrip(t *testing.T) {
	privPath, pubPath := writeTestKeys(t)
	priv, pub, err := LoadKeys(privPath, pubPath)
	if err != nil {
		t.Fatal(err)
	}

	iss := NewIssuer(priv, 15*time.Minute)
	ver := NewVerifier(pub)
	token, err := iss.Issue("1", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := ver.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Role != RoleAdmin {
		t.Fatalf("role = %q", claims.Role)
	}
}

// PKCS#1 (openssl genrsa / -RSAPublicKey_out) — запасной формат.
func TestLoadKeysPKCS1(t *testing.T) {
	key := testKey(t)
	dir := t.TempDir()
	privPath := filepath.Join(dir, "private.pem")
	pubPath := filepath.Join(dir, "public.pem")
	writePEM(t, privPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	writePEM(t, pubPath, "RSA PUBLIC KEY", x509.MarshalPKCS1PublicKey(&key.PublicKey))

	if _, _, err := LoadKeys(privPath, pubPath); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeysErrors(t *testing.T) {
	privPath, pubPath := writeTestKeys(t)
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("не pem"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string][2]string{
		"нет приватного файла": {filepath.Join(t.TempDir(), "нет.pem"), pubPath},
		"нет публичного файла": {privPath, filepath.Join(t.TempDir(), "нет.pem")},
		"мусор вместо PEM":     {garbage, pubPath},
		"ключи перепутаны":     {pubPath, privPath},
	}
	for name, paths := range cases {
		if _, _, err := LoadKeys(paths[0], paths[1]); err == nil {
			t.Errorf("%s: LoadKeys не вернул ошибку", name)
		}
	}
}
