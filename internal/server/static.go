// static.go — раздача собранного React-фронта (M10, web/dist) тем же
// процессом, что и API: один origin — refresh-cookie (Path=/auth,
// SameSite=Strict) и относительные /api-/ws-URL работают без CORS-магии.
//
// Каталог опционален: dev гоняет Vite с прокси, а бэкенд без собранного
// фронта остаётся чистым API (M0–M9 ничего не теряют).
package server

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// ServeFrontend вешает SPA-раздачу каталога dir на NoRoute роутера.
// Существующие роуты (API, вебхуки, WS) всегда в приоритете: NoRoute
// срабатывает только там, где Gin не нашёл зарегистрированной ручки.
func ServeFrontend(r *gin.Engine, dir string, log *slog.Logger) {
	index := filepath.Join(dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		log.Info("frontend: web/dist не собран, статика выключена", "dir", dir)
		return
	}
	fileServer := http.FileServer(http.Dir(dir))

	r.NoRoute(func(c *gin.Context) {
		// Не-GET и служебные префиксы — это промах мимо API, а не переход
		// по ссылке SPA: отвечаем 404 в формате §4.2, index.html не отдаём.
		p := c.Request.URL.Path
		if (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) ||
			hasAPIPrefix(p) {
			c.JSON(http.StatusNotFound, gin.H{"error": "нет такой ручки", "code": "ERR_NOT_FOUND"})
			return
		}
		// Реальный файл сборки (js/css/иконки) — как есть; любой другой
		// путь — index.html (SPA-роутинг на клиенте).
		full := filepath.Join(dir, filepath.Clean("/"+p))
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			fileServer.ServeHTTP(c.Writer, c.Request)
			return
		}
		c.File(index)
	})
	log.Info("frontend: статика включена", "dir", dir)
}

// hasAPIPrefix — пути бэкенда, куда SPA-fallback не должен дотягиваться.
func hasAPIPrefix(p string) bool {
	for _, pre := range []string{"/api", "/auth", "/ws", "/webhook", "/health", "/ready"} {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}
