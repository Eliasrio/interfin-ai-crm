# EP-04 — Панель Эммы: файлы для отправки + маркер-протокол

**Ветка:** `feat/ep04-files` (от `feat/m11-ops`, включает EP-01…EP-03)
**Зависимости:** EP-01 (таблица emma_send_files, emmaProtected),
EP-02 (точка сборки секций system-блока в prompt.go, PromptProvider),
EP-03 (config.EmmaConfig.FilesDir(), паттерн загрузки/валидации из kb.go),
M3 (processor: CreateOutbound → Send, ветка переотправки), M12
(publishMessage — след в чате менеджера).
**Разделы ТЗ:** `docs/EMMA_PANEL_TZ_v2.md` §3 «Вкладка 3 — Файлы для
отправки», сводная схема шагов 5–6, §6, §7, §9 п.5.

## Цель
Библиотека файлов (PDF/JPG/PNG) с описаниями-подсказками; Эмма сама решает,
когда отправить файл клиенту: ставит в ответ маркер `{{file:N}}`, воркер
вырезает маркер, шлёт файл в Telegram и оставляет след в чате менеджера.
Плюс общий парсер маркеров — фундамент для `{{handoff}}` (EP-05).

## Контекст (что уже есть — переиспользовать, не дублировать)
- Загрузка/валидация файлов — образец `internal/emma/kb.go` (EP-03):
  MaxBytesReader 50 МБ → 413, сигнатуры, UUID-имя на диске. Каталог —
  `cfg.Emma.FilesDir()` (`<data_dir>/files`, уже создаётся при старте).
- Сигнатуры: %PDF (есть в EP-03), JPEG `FF D8 FF`, PNG `89 50 4E 47`.
- `internal/worker/processor.go`: ответ Эммы сохраняется ДО Send
  (строки ~245–252: CreateOutbound → publishMessage → Send). Маркеры
  вырезать ДО CreateOutbound — в БД и в событие M12 уходит чистый текст;
  ветка переотправки (~строка 190) шлёт сохранённый текст — вложения
  при ретрае не восстанавливаются (осознанное упрощение v1, ТЗ §3).
- `Sender` (internal/worker) — только Typing/Send(text). Расширяется —
  правятся ВСЕ фейки Sender в тестах (грабля M14 №3: worker, kanban,
  handlers, ws, e2e).
- Отправка файлов telebot'ом: `bot.Send(tele.ChatID, &tele.Document{File:
  tele.FromDisk(path), FileName: ...})` / `&tele.Photo{...}`; bot.Start
  НЕ нужен (как TelebotSender).
- Таблица `emma_send_files` и `emma_events` (event_type='file_sent',
  send_file_id, error_kind='file_not_found') — созданы в EP-01.
- Секции system-блока — расширяемая сборка из EP-02 (prompt.go).

## Задачи

1. **Репозиторий `EmmaSendFilesRepo`** (internal/repo): List, GetByID,
   Create, Update (name/description/is_active), Delete (RETURNING
   file_path — удалить с диска после коммита), `ListActive(ctx)` —
   для секции промпта и валидации маркеров.

2. **Ручки на emmaProtected** (ТЗ §6):
   - `GET /api/emma/files` — список (id, name, description, mime, size,
     is_active, created_at);
   - `POST /api/emma/files` — multipart: `name` (обязателен),
     `description` (обязателен — без него Эмме не решить, когда слать),
     `file` (PDF/JPG/PNG, ≤50 МБ, сигнатура) → 201;
   - `PATCH /api/emma/files/:id` — name/description/is_active;
   - `DELETE /api/emma/files/:id` — строка + файл с диска;
     `emma_events.send_file_id` обнуляется сам (ON DELETE SET NULL).

3. **Секция файлов в system-блоке** (prompt.go, точка сборки EP-02):
   только при непустом ListActive:
   «Тебе доступны файлы для отправки клиенту:
   [id=3] Прайс 2026 — отправь, когда клиент спрашивает подробные цены.
   Чтобы отправить файл, добавь В КОНЕЦ ответа маркер {{file:3}}.
   Можно несколько маркеров. Не упоминай маркеры и номера файлов в
   тексте ответа.»
   Кэш списка — 30 с (паттерн PromptProvider).

4. **Общий парсер маркеров** (internal/emma или internal/worker,
   отдельный файл + unit):
   `ParseMarkers(text) (clean string, fileIDs []int64, handoff bool)` —
   regex `\{\{file:(\d+)\}\}` и `\{\{handoff\}\}`, вырезает все маркеры,
   схлопывает оставшиеся двойные пробелы/пустые хвосты. Битые/чужие
   маркеры (`{{file:abc}}`) — вырезать и логировать. `handoff` в EP-04
   только логируется («handoff-маркер получен, обработка — EP-05») —
   поведение Эммы не меняется до EP-05.

5. **Processor: отправка файлов** (после успешного Send текста):
   - для каждого fileID из маркеров: GetByID + is_active; неактивный/
     несуществующий → emma_events (event_type='error',
     error_kind='file_not_found', detail=id) + slog, файл пропускается;
   - PDF → SendDocument, JPG/PNG → SendPhoto; по одному сообщению на
     файл (sendMediaGroup не используем — ТЗ §3);
   - после успешной отправки файла: CreateOutbound служебной записи
     `[файл: <name>]` (author=bot) + publishMessage (след в чате M12) +
     emma_events (event_type='file_sent', send_file_id, lead_id);
   - ошибка Telegram при отправке файла: emma_events error
     (telegram_api) + slog, НЕ ретраить задачу (текст уже ушёл — повтор
     задачи продублировал бы ответ клиенту).

6. **Sender** (internal/worker/sender.go): `SendDocument(chatID int64,
   path, fileName string) error`, `SendPhoto(chatID int64, path string)
   error` + реализация в TelebotSender; обновить все тестовые фейки.

## Вне скоупа (не делать в EP-04)
- Обработка `{{handoff}}` (перевод режима, подтверждение, WS) — EP-05;
  здесь маркер только вырезается и логируется.
- Reply-клавиатура у Send — EP-05 (кнопка менеджера).
- Запись event_type='reply' в emma_events и статистика — EP-06.
- UI вкладки — EP-07.
- Восстановление вложений в ветке переотправки — осознанно нет (ТЗ §3).

## Контракт наружу
- `ParseMarkers` — EP-05 использует флаг handoff (сигнатура не меняется).
- Ручки `/api/emma/files*` — фронт EP-07.
- `Sender.SendDocument/SendPhoto` — EP-05/EP-06 (алерты не трогают,
  но фейки уже расширены).
- Записи emma_events file_sent/file_not_found — статистика EP-06 читает
  как есть.
- В `messages` появляются служебные записи `[файл: …]` (author=bot) —
  фронт чата M12 отображает их как обычные пузыри (ничего менять не надо).

## Критерии приёмки
- [ ] Живой смоук на dev (§9 п.5): загружен PDF с description «отправь,
      когда клиент спрашивает цены» → вопрос «сколько стоит?» → Эмме
      уходит секция файлов, клиенту приходит текст + документ; в чате
      карточки виден `[файл: …]`; в emma_events — file_sent.
- [ ] Contract-тест processor (фейк Claude отвечает
      «Вот прайс {{file:N}}»): Send получил текст БЕЗ маркера; в
      messages — чистый текст и `[файл: …]`; SendDocument вызван с путём
      файла N; события file_sent записано.
- [ ] Маркер с неактивным/несуществующим id: маркер вырезан, файл не
      отправлен, задача НЕ упала, в emma_events — error file_not_found.
- [ ] `{{handoff}}` в ответе: вырезан, залогирован, поведение без
      изменений (contract-тест — задел EP-05).
- [ ] JPG уходит через SendPhoto, PDF — через SendDocument (unit по
      mime-ветвлению).
- [ ] PATCH is_active=false: файл исчезает из секции промпта (≤30 с,
      unit на сборку секции) и его маркер перестаёт проходить валидацию.
- [ ] DELETE: строки нет, файла на диске нет, старые emma_events
      с этим send_file_id живы с NULL (интеграционный тест).
- [ ] POST без description → 400; 51 МБ → 413; .docx/.exe → 400.
- [ ] Ошибка Telegram на отправке файла (фейк Sender) → error в
      emma_events, задача завершена без ретрая, текст не задублирован.
- [ ] manager → 403, admin без PIN → 401 на всех ручках files.
- [ ] `go build`, `go vet`, `go test -p 1` зелёные (все фейки Sender
      обновлены); schema-lint зелёный.
