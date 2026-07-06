# M3 — Воркер + Claude

**Ветка:** `feat/m3-worker`
**Зависимости:** M0, M1, M2
**Разделы SRS:** §6.2 (Worker Pipeline), §6.3 (Ошибки+дедуп), §6.4 (TTL tasks),
§7.2 (Token Budget + гибридный подсчёт)

## Цель
Asynq-воркер обрабатывает `process:inbound`: собирает контекст, вызывает Claude,
отправляет ответ. Здесь появляется первый живой диалог бота (пока без RAG — M4).

## Задачи
1. Asynq worker + handler для `process:inbound` (§6.2):
   - `bot.SendChatAction(chat, telebot.Typing)`;
   - собрать контекст: system prompt + история диалога (token budget);
   - вызвать Anthropic `/v1/messages` (`claude-3-5-sonnet-20241022`,
     `claude_reply_tokens: 1000`);
   - сохранить ответ (`direction='outbound'`);
   - `bot.Send(chat, responseText)`.
2. **Гибридный подсчёт токенов** (§7.2, жёсткое правило §4.6):
   - оценка `len(text)/4`;
   - если > `count_tokens_threshold` (7500) → точный `count_tokens` (Anthropic API);
   - если точный > 8000 → усечь history (oldest-first).
   - Никакого tiktoken.
   - Бюджет: system 2000 + summary 1000 + history 4000 + buffer 1000 = 9000.
3. **Дедупликация** (§6.3): подтвердить, что enqueue из M2 использует
   `TaskID + Unique`; handler идемпотентен.
4. **Обработка ошибок** (§6.3): retry 3× (backoff 2/8/32с), dead letter →
   `asynq:dead` → алерт менеджеру в Telegram.
5. **TTL task management helper** (§6.4): функции schedule/cancel TTL-задач через
   `inspector.DeleteTask` + `client.Enqueue(ProcessIn(...))`. Сохранять `info.ID`
   в `leads.ttl_task_id`. (Использоваться будет в M5, но helper делаем здесь.)

## Контракт наружу
- Рабочий диалог: лид пишет → бот отвечает через Claude.
- TTL-helper (schedule/cancel) — для M5 Kanban.
- Токен-бюджетер — переиспользуется в M4 (добавит RAG в контекст).

## Критерии приёмки
- [ ] Лид пишет в Telegram → получает осмысленный ответ Claude.
- [ ] Запрос к Claude никогда не превышает 9000 токенов (IQ-6).
- [ ] `count_tokens` вызывается только у границы, не на каждое сообщение (AQ²-7).
- [ ] 1000 дублей задачи → 1 выполнение (AQ²-6 stress test).
- [ ] Падение Claude API → retry, затем dead letter + алерт.
