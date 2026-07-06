// failover_probe — зонд учений Redis failover (M11, критерии IQ-7 и §11.2).
//
// Подключается к Redis ЧЕРЕЗ Sentinel (как prod: RedisFailoverClientOpt) и
// параллельно гоняет два боевых пути:
//
//  1. очередь: enqueue задачи каждые interval (путь вебхука M2);
//  2. pub/sub: publish+subscribe канала (путь crm:events → WS Hub M9).
//
// Оператор убивает master (docker kill drill-redis-m) — зонд печатает:
//   - когда путь упал и когда ожил (длительность okна недоступности);
//   - для pub/sub — время от последнего успешного приёма до обнаружения
//     обрыва: столько WS Hub идёт к polling_mode (IQ-7: < 5 с);
//   - сколько enqueue ушло в ошибку (эти сообщения в проде НЕ теряются:
//     вебхук ставит pending_task, recovery-cron перевыставляет — §11.2).
//
// Запуск — scripts/failover_drill.sh (внутри docker-сети стенда).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

func main() {
	sentinels := flag.String("sentinels", "sentinel-1:26379,sentinel-2:26380,sentinel-3:26381",
		"адреса Sentinel через запятую")
	master := flag.String("master", "mymaster", "имя мастера у Sentinel")
	duration := flag.Duration("duration", 60*time.Second, "длительность зондирования")
	interval := flag.Duration("interval", 200*time.Millisecond, "период enqueue/publish")
	flag.Parse()

	addrs := strings.Split(*sentinels, ",")
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	fmt.Printf("probe: sentinels=%v master=%s duration=%s\n", addrs, *master, *duration)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); queueLoop(ctx, addrs, *master, *interval) }()

	sub := redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName: *master, SentinelAddrs: addrs,
	})
	defer sub.Close()
	pub := redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName: *master, SentinelAddrs: addrs,
	})
	defer pub.Close()
	go func() { defer wg.Done(); publishLoop(ctx, pub, *interval) }()
	go func() { defer wg.Done(); subscribeLoop(ctx, sub) }()
	wg.Wait()
}

// queueLoop — путь вебхука: enqueue с уникальным TaskID каждые interval.
func queueLoop(ctx context.Context, sentinels []string, master string, interval time.Duration) {
	client := asynq.NewClient(asynq.RedisFailoverClientOpt{
		MasterName:    master,
		SentinelAddrs: sentinels,
	})
	defer client.Close()

	var (
		seq       int
		okCount   int
		failCount int
		downSince time.Time
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("queue: итог — ok=%d fail=%d (упавшие enqueue в проде закрывает recovery-cron §11.2)\n",
				okCount, failCount)
			return
		case <-ticker.C:
			seq++
			task := asynq.NewTask("drill:probe", []byte(fmt.Sprintf(`{"seq":%d}`, seq)))
			_, err := client.EnqueueContext(ctx, task,
				asynq.TaskID(fmt.Sprintf("drill-%d-%d", time.Now().UnixNano(), seq)))
			switch {
			case err != nil && downSince.IsZero():
				downSince = time.Now()
				failCount++
				fmt.Printf("queue: ПУТЬ УПАЛ (seq=%d): %v\n", seq, err)
			case err != nil:
				failCount++
			case err == nil && !downSince.IsZero():
				fmt.Printf("queue: ПУТЬ ОЖИЛ через %s (failover завершён, enqueue снова работает)\n",
					time.Since(downSince).Round(100*time.Millisecond))
				downSince = time.Time{}
				okCount++
			default:
				okCount++
			}
		}
	}
}

// publishLoop — источник событий (боевой аналог: kanban.Machine → crm:events).
func publishLoop(ctx context.Context, rdb *redis.Client, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	seq := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			seq++
			// Ошибки publish штатны во время failover: pub/sub fire-and-forget
			// (§10.1), клиент добирает пропуски catch-up'ом §10.3.
			_ = rdb.Publish(ctx, "drill:events", fmt.Sprintf("%d", seq)).Err()
		}
	}
}

// subscribeLoop повторяет логику WS Hub (M9): подписка, на ошибке —
// «polling mode» + ресабскрайб с ретраями, после восстановления — «live mode».
func subscribeLoop(ctx context.Context, rdb *redis.Client) {
	for {
		if ctx.Err() != nil {
			return
		}
		pubsub := rdb.Subscribe(ctx, "drill:events")
		if _, err := pubsub.Receive(ctx); err != nil {
			_ = pubsub.Close()
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second) // resubscribeDelay Hub'а
			continue
		}
		fmt.Println("pubsub: подписка активна (Hub был бы в live_mode)")

		lastMsg := time.Now()
		for {
			msg, err := pubsub.ReceiveMessage(ctx)
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
					// Ровно здесь Hub рассылает polling_mode: Kanban продолжает
					// работу опросом REST. Время обнаружения — критерий IQ-7.
					fmt.Printf("pubsub: ОБРЫВ обнаружен через %s после последнего события — Hub ушёл бы в polling_mode (IQ-7: < 5с)\n",
						time.Since(lastMsg).Round(100*time.Millisecond))
				}
				break
			}
			lastMsg = time.Now()
			_ = msg
		}
		_ = pubsub.Close()
	}
}
