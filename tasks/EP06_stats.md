# EP-06 — Панель Эммы: статистика, учёт расходов Claude, журнал ошибок, алерты

**Ветка:** `feat/ep06-stats` (от `feat/m11-ops`, включает EP-01…EP-05)
**Зависимости:** EP-01 (emma_events, settings-ключ emma_panel.alert_chat_id),
EP-04 (recordEvent в processor, события file_sent/file_not_found,
Sender), EP-05 (событие handoff), EP-03 (worker emmakb — ошибки индексации),
M3 (Claude-клиент возвращает usage), M8 (паттерн пагинации API).
**Разделы ТЗ:** `docs/EMMA_PANEL_TZ_v2.md` §3 «Вкладка 6 — Статистика»,
§5 (emma_events), §6 (stats/alerts), §0 (решения: стоимость Claude,
алерты шлёт сама Эмма).

## Цель
Полный журнал работы Эммы: каждый ответ пишется с временем и токенами,
каждая ошибка — с типом; поверх — сводная статистика по периодам
(диалоги, сообщения, скорость, файлы, handoff, токены и примерная
стоимость в $) и алерты в Telegram-группу владельца при сериях сбоев.

## Контекст (что уже есть — переиспользовать, не дублировать)
- `emma_events` (0020): event_type CHECK reply/file_sent/handoff/error;
  error_kind llm_api/telegram_api/timeout/file_not_found/kb_index;
  tokens_in/tokens_out, response_time_ms, send_file_id, lead_id;
  индексы (type, created_at) и (created_at).
- `processor.recordEvent(ctx, ev)` — уже пишет handoff (EP-05),
  file_sent/file_not_found/telegram_api-на-файле (EP-04). Хелпер
  best-effort (ошибка записи не валит задачу) — сохранить дисциплину.
- Claude-клиент (M3) парсит usage (InputTokens/OutputTokens) — токены
  уже доступны в processor после Complete.
- Метрика времени ответа: от получения задачи воркером до успешного
  Send текста (не включает очередь Telegram→вебхук).
- Redis-паттерны: INCR+ExpireNX (rate-limit M8), SETNX-замки. Алерты —
  fail-open (Redis умер → алертов нет, Эмма работает; ЗДЕСЬ наоборот
  относительно PIN: деградация без алертов допустима).
- Отправка в Telegram произвольному chat_id — `Sender.Send` (алерты шлёт
  сама Эмма, решение владельца; ManagerChatID-контур M13 НЕ трогать —
  он для уведомлений менеджерам).
- Существующие данные для метрик: `leads.created_at` (новые диалоги),
  `messages` direction/created_at (сообщения, активные диалоги).
- Пагинация/фильтры — образец GET /api/leads (M8).

## Задачи

1. **Запись недостающих событий** (processor):
   - `reply` после успешного Send текста: lead_id, response_time_ms,
     tokens_in/tokens_out из usage;
   - `error`/`llm_api` — ошибка Anthropic (после исчерпания ретраев
     клиента; в detail — краткий текст без промпта!);
   - `error`/`timeout` — контекст/таймаут Claude или Telegram
     (различать по errors.Is);
   - `error`/`telegram_api` — падение Send текста (ветка переотправки
     при повторе НЕ плодит второй reply-event — проверить);
   - welcome и handoff-подтверждение (детерминированные ответы EP-05)
     reply-событие НЕ пишут (Claude не вызывался, токенов нет) —
     осознанно.
   В `internal/worker/emmakb.go`: финальный error индексации → event
   `error`/`kb_index` (detail — имя файла + причина).

2. **Расширение `EmmaEventsRepo`**: `Stats(ctx, from, to)` — одна-две
   агрегатные выборки: count reply, avg/p95 response_time_ms
   (percentile_cont), sum tokens_in/out, count handoff, count file_sent
   (+ разбивка по send_file_id с именами через JOIN), count error по
   error_kind; `ListErrors(ctx, kind, from, to, page)` — 50 на страницу,
   новые сверху, total.

3. **Сводная статистика `GET /api/emma/stats?period=day|week|month|all`**
   (emmaProtected):
   - из emma_events: ответы, среднее/p95 время, токены, стоимость,
     handoff, файлы (всего и по каждому);
   - из leads/messages: новых лидов за период, активных диалогов
     (distinct lead_id по inbound за последние 24 ч — всегда 24 ч,
     независимо от period), сообщений входящих/исходящих за период;
   - **стоимость**: константы цен в internal/emma (за 1M токенов,
     input и output отдельно, для `claude-sonnet-5`; актуальный прайс
     Anthropic взять на момент реализации, рядом комментарий с датой);
     ответ содержит `cost_usd_estimate` и `pricing_note: "примерная
     оценка; точный счёт — в консоли Anthropic"`.

4. **Журнал ошибок `GET /api/emma/stats/errors?type=&page=`** —
   dата/время, error_kind, detail, lead_id; фильтр по kind, пагинация.

5. **Алерты** (internal/emma/alerts.go, вызовы из recordEvent-точек):
   - настройка: `GET /api/emma/alerts` → {chat_id, enabled: chat_id!=""},
     `PATCH` — chat_id (валидация: int64, может быть отрицательным —
     группы; пусто = выключить), `POST /api/emma/alerts/test` —
     пробный алерт «✅ Тестовый алерт панели Эммы» → 502 если Telegram
     отказал (владелец сразу видит, что chat_id кривой);
   - условия (счётчики-серии в Redis, сбрасываются успешным reply):
     ≥3 llm_api подряд; ≥3 telegram_api подряд; каждая ошибка kb_index;
   - анти-шум: не чаще одного алерта одного типа в 15 минут
     (Redis SET NX EX 900);
   - формат: «⚠️ Эмма: <тип проблемы>\nВремя: <UTC>\nПоследняя ошибка:
     <detail>»;
   - отправка Sender.Send в chat_id из settings; chat_id пуст —
     алерты молча выключены; ошибки самого алертинга — только slog
     (никаких каскадов).

## Вне скоупа (не делать в EP-06)
- UI вкладки 6 — EP-07 (API отдаёт всё готовое, включая pricing_note).
- Инфраструктурные алерты (процесс/Redis/Postgres упали) — Prometheus/
  Alertmanager M11, не дублировать.
- Prometheus-метрики панели — существующего /metrics хватает.
- Исторические данные до EP-06: reply-события начнутся с деплоя эпика,
  статистика «за всё время» честно считает с этого момента (пометки
  не нужны).

## Контракт наружу
- `GET /api/emma/stats`, `/stats/errors`, `/alerts*` — фронт EP-07
  (структуры ответов зафиксировать в contract-тесте — EP-07 будет
  писать по ним).
- Событие reply с токенами — единственный источник учёта расходов;
  менять семантику полей нельзя без правки stats.
- Redis-ключи `emma:alert:*` — приватные для alerts.go.

## Критерии приёмки
- [ ] Contract-тест processor: успешный ответ → ровно один reply-event
      с tokens_in/out из usage и response_time_ms > 0; ретрай упавшего
      Send (ветка переотправки) второй reply НЕ пишет.
- [ ] Ошибка Claude (фейк) → error/llm_api; таймаут → error/timeout;
      detail не содержит текста промпта.
- [ ] Три llm_api подряд (фейк Claude) → ровно ОДИН алерт в chat_id
      (фейк Sender); четвёртая ошибка в течение 15 мин — без второго
      алерта; успешный ответ между ошибками сбрасывает серию
      (интеграционный тест с REDIS_TEST_ADDR).
- [ ] Ошибка индексации КБ → error/kb_index + алерт (фикстура scan.pdf
      из EP-03).
- [ ] GET /stats на фикстурах: числа сходятся (посчитаны руками в
      тесте) — ответы, p95, токены, стоимость (= токены × константы),
      handoff, файлы по каждому, активные диалоги за 24 ч.
- [ ] GET /stats/errors: фильтр по kind работает, пагинация 50, total
      корректен.
- [ ] PATCH /alerts с мусором («abc») → 400; POST /alerts/test при
      пустом chat_id → 400, при фейковом отказе Telegram → 502.
- [ ] Redis недоступен → Эмма отвечает как обычно, алертов нет, в логе
      warning (fail-open, контраст с PIN отмечен в коде комментарием).
- [ ] manager → 403, admin без PIN → 401 на stats и alerts.
- [ ] Живой смоук на dev: пара сообщений Эмме → GET /stats показывает
      ответы/токены/стоимость; POST /alerts/test с вашим chat_id —
      сообщение пришло в Telegram.
- [ ] `go build`, `go vet`, `go test -p 1` зелёные; schema-lint зелёный.
