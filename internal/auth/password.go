package auth

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword — bcrypt (задача M7-7). В managers.password_hash кладётся
// только результат этой функции: пароли в открытом виде не хранятся
// (критерий приёмки M7).
func HashPassword(plain string) (string, error) {
	// bcrypt молча обрезает пароль на 72 байтах — два разных длинных пароля
	// стали бы «одинаковыми». Отказ честнее тихой потери энтропии.
	if len(plain) > 72 {
		return "", fmt.Errorf("auth: пароль длиннее 72 байт (ограничение bcrypt)")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: bcrypt: %w", err)
	}
	return string(hash), nil
}

// CheckPassword сверяет пароль с bcrypt-хешем.
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// dummyHash — bcrypt от случайной строки; логин по несуществующему email
// прогоняет сверку с ним (EqualizeUnknownUser), чтобы время ответа не
// выдавало, существует ли учётка (timing-атака перебора email).
var dummyHash = func() string {
	h, err := HashPassword("interfin-timing-equalizer")
	if err != nil {
		panic(err) // константный вход, не падает
	}
	return h
}()

// EqualizeUnknownUser сжигает столько же CPU, сколько честная bcrypt-сверка.
func EqualizeUnknownUser(plain string) {
	_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(plain))
}
