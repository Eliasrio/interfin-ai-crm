// middleware.go — точки съёма метрик: Gin (HTTP) и Asynq (воркер).
package metrics

import (
	"context"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
)

// Gin — middleware gin_request_duration_seconds. Вешается в server.New
// сразу после Recovery: меряет все роуты, включая /webhook/telegram
// (алерт WebhookHighLatency, §14).
func Gin() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		path := c.FullPath()
		if path == "" {
			// Немэтчнутый роут (404): сырой URL в label не пускаем —
			// кардинальность метрики взорвал бы любой сканер.
			path = "unmatched"
		}
		GinRequestDuration.
			WithLabelValues(c.Request.Method, path, strconv.Itoa(c.Writer.Status())).
			Observe(time.Since(start).Seconds())
	}
}

// Asynq — middleware asynq_task_duration_seconds для ServeMux воркера.
// status: ok | error (ошибка = будет ретрай либо dead letter, §6.3).
func Asynq() asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
			start := time.Now()
			err := next.ProcessTask(ctx, t)
			status := "ok"
			if err != nil {
				status = "error"
			}
			AsynqTaskDuration.
				WithLabelValues(t.Type(), status).
				Observe(time.Since(start).Seconds())
			return err
		})
	}
}
