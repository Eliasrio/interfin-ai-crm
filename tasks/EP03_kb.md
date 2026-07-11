# EP-03 — Панель Эммы: база знаний (загрузка файлов → RAG-индексация)

**Ветка:** `feat/ep03-kb` (от `feat/m11-ops`, включает EP-01/EP-02)
**Зависимости:** EP-01 (таблица emma_kb_files, emmaProtected, Audit),
M4 (rag.Indexer/chunker, KnowledgeRepo.ReplaceSource, Voyage-клиент),
M2/M3 (паттерн asynq-задач: queue.Type* + mux.HandleFunc),
M9 (events.Event — клиенты игнорируют незнакомые типы).
**Разделы ТЗ:** `docs/EMMA_PANEL_TZ_v2.md` §3 «Вкладка 2 — База знаний»,
§2.3 (хранение файлов, лимиты, nginx, бэкапы), §6, §9 п.4.

## Цель
Владелец загружает TXT/MD/PDF через API панели — файл сохраняется на диск,
текст извлекается и асинхронно индексируется в существующий pgvector-индекс
(тот самый, из которого Эмма берёт RAG-контекст). Статус индексации виден
(pending → indexed(N чанков) / error), файл можно переиндексировать и
удалить (с вычисткой чанков). Объём базы не ограничен.

## Контекст (что уже есть — переиспользовать, не дублировать)
- `rag.Indexer.IndexDocument(ctx, source, text)` — режет на чанки (1200
  символов), эмбеддит Voyage'ем, атомарно заменяет чанки source
  (ReplaceSource). Пустой текст → чанки source вычищаются. Ничего в rag
  менять не нужно.
- `cmd/index-kb` индексирует `docs/kb` со своими source — НЕ трогать;
  source панели = `panel:<file_id>` — пространства не пересекаются.
- Voyage free tier: 3 RPM / 10K TPM (грабля M4) — индексация ТОЛЬКО через
  asynq (ретраи с backoff прикрывают 429); большой файл может потребовать
  батчей эмбеддинга — смотри, как Embed зовётся в Indexer, при
  необходимости батчить чанки пачками.
- Asynq: TaskID + Unique обязательны (CLAUDE.md §4.5); unique-замок НЕ
  снимается DeleteTask (грабля M3) — TaskID делай версионным:
  `emma:kb:index:<file_id>:<unix_updated>`, Unique(1h) — reindex не
  упрётся в замок старой задачи.
- Регистрация обработчика — `internal/worker/server.go` (mux.HandleFunc,
  образец TypeProcessInbound).
- WS-события: `events.Event{Type: "..."}` → Redis → Hub; фронт игнорирует
  незнакомые типы (проверено M12) — новый тип безопасен.
- Хранение вне web-root: `data/emma/kb/<uuid>` (ТЗ §2.3), имя на диске —
  UUID, оригинальное имя в БД (уже так в emma_kb_files EP-01).
- Upsert по filename: повторная загрузка = ТА ЖЕ строка, id стабилен
  (ТЗ §3, QA-фикс — иначе старые чанки живут вечно).

## Задачи

1. **Конфиг:** `emma.data_dir` (env `EMMA_DATA_DIR`, дефолт `./data/emma`)
   в internal/config; при старте — MkdirAll `<dir>/kb` и `<dir>/files`
   (files — задел под EP-04). `data/` — в .gitignore.

2. **Репозиторий `EmmaKBRepo`** (internal/repo): List (новые→старые),
   GetByID, `UpsertByFilename(ctx, f)` (INSERT ... ON CONFLICT (filename)
   DO UPDATE: mime/path/size/status='pending'/error=NULL; RETURNING id +
   старый file_path — чтобы удалить с диска заменённый оригинал),
   `SetStatus(ctx, id, status, chunks, errText)`, Delete.

3. **Извлечение текста** (internal/emma или internal/kbtext):
   - TXT/MD — читаем как есть (валидный UTF-8, иначе error);
   - PDF — pure-Go библиотека (выбрать в эпике: кандидаты
     github.com/ledongthuc/pdf, rsc.io/pdf; совместимость с go 1.22,
     зависимость пришпилить); PDF без текстового слоя (скан) → пустой
     текст → status `error` «PDF без текстового слоя, OCR не
     поддерживается» (ТЗ §3);
   - функция чистая, unit-тесты с фикстурами (мини-PDF в testdata).

4. **Загрузка `POST /api/emma/kb`** (multipart, на emmaProtected):
   - лимит 50 МБ (gin/http MaxBytesReader → 413 `{"code":"FILE_TOO_LARGE"}`);
   - allowlist: .txt/.md/.pdf + сигнатура (%PDF для pdf, валидный UTF-8
     для текста), несоответствие → 400 `{"code":"FILE_TYPE_UNSUPPORTED"}`;
   - сохранить на диск `<data_dir>/kb/<uuid>`, upsert строки по filename
     (заменённый старый оригинал — удалить с диска), status pending;
   - enqueue `emma:kb:index` payload `{file_id}`;
   - ответ 202 с строкой файла (фронт ждёт WS).

5. **Воркер `HandleEmmaKBIndex`** (internal/worker, регистрация в
   server.go): прочитать строку и файл → извлечь текст →
   `IndexDocument("panel:<id>", text)` → SetStatus indexed+chunks;
   ошибка извлечения — status error БЕЗ ретрая (asynq.SkipRetry);
   ошибка Voyage/БД — вернуть err (ретрай, MaxRetry 5, статус остаётся
   pending до финала; после исчерпания ретраев — status error).
   По финалу (indexed/error) — WS-событие `emma_kb_status`
   `{file_id, filename, status, chunks, error}` (новый конструктор в
   internal/events).

6. **`POST /api/emma/kb/:id/reindex`** — status pending + enqueue (тот же
   контур; текст заново извлекается из оригинала на диске). 404 на нет id.

7. **`GET /api/emma/kb`** — список: id, filename, mime, size, status,
   chunks_count, index_error, created_at.

8. **`DELETE /api/emma/kb/:id`** — транзакция: `ReplaceSource("panel:<id>",
   nil)` + удаление строки; после коммита — файл с диска (best-effort,
   ошибку в лог). 404 на нет id.

9. **Ops (без деплоя, только конфиги и runbook):**
   - docker-compose.yml и docker-compose.prod.yml: volume
     `emma_data:/app/data/emma` (+ EMMA_DATA_DIR);
   - docs/ops/deploy_steps.md — новый шаг: nginx `client_max_body_size
     50m` (иначе загрузка умрёт на 1 МБ до приложения) и включение
     тома emma_data в B2-бэкап (сейчас бэкапится только БД/WAL);
   - prod-деплой ЭТОГО эпика не делается — только документация.

## Вне скоупа (не делать в EP-03)
- Файлы для отправки клиентам (`<data_dir>/files`) — EP-04 (каталог
  создаём, код не пишем).
- UI вкладки — EP-07 (фронту уже будет: список, статусы, WS-событие).
- OCR сканов, DOCX в базе знаний — вне v1.
- Индексация docs/kb и калибровка порога RAG — как было (cmd/index-kb,
  scripts/rag_calibrate).

## Контракт наружу
- WS-тип `emma_kb_status` — фронт EP-07 (старые клиенты игнорируют).
- `queue.TypeEmmaKBIndex = "emma:kb:index"`, payload `{file_id}`.
- `emma.data_dir` / EMMA_DATA_DIR + каталог `<dir>/files` — EP-04.
- Ручки `/api/emma/kb*` — фронт EP-07.
- Соглашение source `panel:<file_id>` — навсегда (смена = потеря связи
  файл↔чанки).

## Критерии приёмки
- [ ] Живой смоук на dev (нужны VOYAGE_API_KEY и запущенный воркер):
      загрузка TXT с уникальным фактом → status indexed, chunks_count>0 →
      вопрос Эмме по факту → ответ содержит факт (контур ТЗ §9 п.4).
- [ ] Повторная загрузка того же filename: id тот же, чанки заменены —
      `SELECT count(*) FROM knowledge_chunks WHERE source='panel:<id>'`
      соответствует новой версии, осиротевших source нет
      (интеграционный тест с фейком Embedder — без Voyage).
- [ ] PDF с текстовым слоем → indexed; PDF-скан (фикстура) → error с
      понятным текстом, задача НЕ ретраится (unit + интеграционный).
- [ ] Файл 51 МБ → 413; .docx / переименованный .exe → 400 (сигнатура).
- [ ] DELETE: строка удалена, чанков source нет, файла на диске нет;
      reindex после error переводит в indexed (после починки фикстуры).
- [ ] WS: на финале индексации приходит emma_kb_status (тест по образцу
      ws_test с Redis).
- [ ] Ошибка Voyage (фейк, возвращающий 429 дважды) → задача ретраится
      и завершается indexed (интеграционный тест воркера).
- [ ] manager → 403, admin без PIN → 401 на всех ручках kb.
- [ ] docker-compose поднимается с новым volume; deploy_steps.md дополнен
      (nginx 50m, B2 + emma_data).
- [ ] `go build`, `go vet`, `go test -p 1` зелёные; schema-lint зелёный.
