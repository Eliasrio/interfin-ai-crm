# EP-05 — Панель Эммы: контакты, приветствие /start, кнопка менеджера, handoff

**Ветка:** `feat/ep05-contacts-scenario` (от `feat/m11-ops`, включает EP-01…EP-04)
**Зависимости:** EP-01 (emma_contacts, settings-ключи emma_panel.welcome_text /
manager_button_enabled / manager_button_text / handoff_confirm_text),
EP-02 (сборка секций system-блока), EP-04 (ParseMarkers → флаг handoff;
расширенный Sender), M13 (dialog_mode, Leads.UpdateFields, контур
takeover:reminder, DialogModeEvent), M12 (publishMessage).
**Разделы ТЗ:** `docs/EMMA_PANEL_TZ_v2.md` §3 «Вкладка 4 — Контакты» и
«Вкладка 5 — Приветствие и вызов менеджера», сводная схема шаг 3, §4
(handoff), §6, §9 п.6.

## Цель
Справочник контактов попадает в контекст Эммы; `/start` отвечает
настраиваемым приветствием без Claude; постоянная кнопка «Связаться с
менеджером» и распознавание просьбы в свободной форме переводят диалог
менеджеру через механизм M13 — с уведомлением, напоминаниями и следом
в статистике.

## Контекст (что уже есть — переиспользовать, не дублировать)
- `ParseMarkers(text) (clean, fileIDs, handoff, unknown)` — EP-04; сейчас
  handoff=true только логируется — ЭТОТ эпик включает обработку.
- Режим M13: `Leads.UpdateFields(ctx, id, fields)` — так PATCH /mode
  ставит dialog_mode/taken_by (internal/handlers/takeover.go:54);
  `events.DialogModeEvent(lead, reason)` — WS-событие; контур напоминаний —
  задачи `takeover:reminder`/`takeover:pickup` (TaskID lead+message_count,
  Unique; обработчики в internal/worker/takeover.go). Автоподхват Эммы
  10+10 сохраняется и для client-handoff (решение владельца, ТЗ §10 п.7).
- Уведомление менеджерам в Telegram — существующий канал
  `cfg.Telegram.ManagerChatID` (M13 reminder). **Уточнение ТЗ §4 п.4:**
  уведомление о client-handoff шлём в ManagerChatID (канал менеджеров,
  как M13), а НЕ в emma_panel.alert_chat_id — тот зарезервирован под
  алерты о сбоях (EP-06).
- Ранний выход панели — сводная схема ТЗ: строго ПОСЛЕ Kanban.OnInbound
  и проверки режима M13 (QA-фикс: ветка до очереди обошла бы анти-спам
  и сброс TTL). Вебхук/диспетчер НЕ трогаем.
- settings.String с кэшем 30 с — ключи вкладки 5 уже существуют (EP-01);
  ручка scenario — просто обёртка над ними.
- Язык: welcome_text — один текст без локализации в v1 (лид на /start
  ещё без языка, гард M14); это осознанно.
- Расширение Sender = правка всех тестовых фейков (грабля M14 №3).

## Задачи

1. **Репозиторий `EmmaContactsRepo`** (internal/repo): List (sort_order,
   новые в конец), GetByID, Create, Update (все поля + is_active),
   Delete, `ListActive(ctx)`. **Лимит 30 активных** (ТЗ §3): Create с
   is_active=true и PATCH, включающий is_active, при 30 уже активных →
   ошибка → 400 `{"code":"CONTACTS_LIMIT","limit":30}` (проверка в
   транзакции — гонка двух PATCH не даёт 31).

2. **Ручки контактов на emmaProtected** (ТЗ §6): `GET/POST /api/emma/
   contacts`, `PATCH/DELETE /api/emma/contacts/:id`. Валидация: type из
   enum БД, name/value непустые.

3. **Секция контактов в system-блоке** (prompt.go, после стиля, перед
   файлами — порядок ТЗ §3): только активные, по sort_order:
   «Контакты и ссылки (упоминай ТОЛЬКО из этого списка, к месту):
   — Менеджер Анна (телефон +7 999…): давай, когда клиент готов к
   консультации». Кэш 30 с (паттерн filesprovider EP-04). Пустой
   список → секции нет.

4. **Ручка сценария**: `GET /api/emma/scenario` → `{welcome_text,
   manager_button_enabled, manager_button_text, handoff_confirm_text}`;
   `PATCH` — частичное обновление тех же ключей settings; кнопка вкл
   при пустом тексте кнопки → 400.

5. **Ранний выход воркера** (processor, после OnInbound и проверки
   режима — ровно шаг 3 сводной схемы ТЗ):
   - inbound-текст начинается с `/start` И welcome_text непуст →
     CreateOutbound(welcome, author=bot) + publishMessage + Send
     (с клавиатурой, задача 7) → СТОП (Claude не вызывается);
   - inbound-текст == manager_button_text (TrimSpace, кнопка включена) →
     ветка handoff (задача 6) с текстом handoff_confirm_text → СТОП.

6. **Handoff-ветка** (общая для кнопки и маркера):
   - отправить клиенту текст: для кнопки — handoff_confirm_text
     (CreateOutbound + publishMessage + Send); для маркера — собственный
     ответ Эммы уже отправлен штатно, ничего не дублировать (ТЗ §4 п.2);
   - `Leads.UpdateFields`: dialog_mode='human', bot_silenced_until=NULL,
     taken_by=NULL (менеджер ещё не взял);
   - WS: `events.DialogModeEvent` с reason `client_handoff` (новая
     константа reason в internal/events);
   - Telegram-уведомление в ManagerChatID: «Клиент <имя/username> просит
     менеджера (лид #N)»;
   - взвести `takeover:reminder` НЕМЕДЛЕННО по образцу M13 (тот же
     TaskID-паттерн lead+message_count — менеджер молчит 10 мин →
     напоминание, ещё 10 → Эмма подхватывает штатным M13-контуром);
   - emma_events: event_type='handoff', lead_id.
   - Повторный запрос менеджера при уже-human — не дублирует уведомление
     (ранний выход по режиму на шаге 2 сработает раньше — просто
     зафиксировать это contract-тестом).

7. **Обработка `{{handoff}}` от Claude** (processor, после Send текста —
   симметрично файлам EP-04): handoff=true → handoff-ветка (задача 6,
   без повторного текста клиенту). Инструкция в system-блок (код, рядом
   с инструкцией файлов EP-04): «Если клиент просит живого человека/
   менеджера — добавь в конец ответа маркер {{handoff}} и сообщи, что
   зовёшь менеджера».

8. **Reply-клавиатура** (Sender): `SendWithKeyboard(chatID, text,
   buttonText string) error` (telebot ReplyMarkup, resize, persistent) и
   `SendRemoveKeyboard(chatID, text string) error` (ReplyKeyboardRemove).
   Логика в processor: кнопка включена → welcome и КАЖДЫЙ ответ Эммы
   уходят с клавиатурой; кнопку выключили → первый следующий ответ
   уходит с RemoveKeyboard (иначе кнопка останется у клиента навсегда);
   состояние «надо снять» — по settings (enabled=false), без новых полей
   в БД. Обновить TelebotSender + все фейки.

## Вне скоупа (не делать в EP-05)
- Алерты о сбоях и статистика (alert_chat_id, счётчики) — EP-06
  (event handoff уже пишется здесь — EP-06 только читает).
- UI вкладок 4–5 — EP-07.
- Локализация welcome/подтверждения по языку лида — v1.1 (лид на /start
  ещё без языка).
- Жёсткий список фраз-триггеров handoff — осознанно нет (ТЗ §10 п.1).

## Контракт наружу
- WS reason `client_handoff` у события dialog_mode — фронт EP-07
  (существующий обработчик dialog_mode M13 его уже понимает, reason —
  доп. поле).
- emma_events handoff — статистика EP-06.
- Ручки `/api/emma/contacts*`, `/api/emma/scenario` — фронт EP-07.
- `Sender.SendWithKeyboard/SendRemoveKeyboard` — общие.
- Поведение по умолчанию НЕ меняется: welcome пуст и кнопка выключена →
  Эмма работает ровно как до эпика (e2e-контур не трогается).

## Критерии приёмки
- [ ] Живой смоук на dev (§9 п.6): фраза «позовите живого человека» →
      ответ Эммы без маркера в тексте, режим human, карточка в CRM
      подсвечена, менеджеру пришло TG-уведомление; кнопка «Связаться с
      менеджером» делает то же с настроенным текстом подтверждения.
- [ ] Contract-тест: /start при welcome_text непустом → Send(welcome с
      клавиатурой), Claude НЕ вызван, в messages есть outbound welcome,
      Kanban.OnInbound вызван (порядок раннего выхода).
- [ ] Contract-тест: /start при пустом welcome → штатный ответ Эммы
      (поведение как до эпика).
- [ ] Contract-тест: {{handoff}} в ответе Claude → текст ушёл чистым,
      mode=human, DialogModeEvent(client_handoff) опубликован,
      takeover:reminder взведён, emma_events handoff записан,
      confirm-текст НЕ отправлен вторым сообщением.
- [ ] Кнопка при уже-human режиме → ранний выход M13, дубля уведомления
      нет.
- [ ] Контакты: 31-й активный → 400 CONTACTS_LIMIT (включая гонку двух
      PATCH — интеграционный тест); PATCH is_active=false убирает
      контакт из секции промпта ≤30 с (unit сборки секции).
- [ ] Секция контактов в system-блоке при непустом списке и отсутствует
      при пустом; Эмма в живом смоуке даёт контакт из списка на прямой
      вопрос (можно объединить со смоуком выше).
- [ ] Выключение кнопки: следующий ответ уходит с RemoveKeyboard (unit
      по ветвлению Sender-вызовов).
- [ ] manager → 403, admin без PIN → 401 на contacts и scenario.
- [ ] `go build`, `go vet`, `go test -p 1` зелёные (фейки Sender
      обновлены); e2e старого контура (ждущие автоответа) зелёные —
      welcome по умолчанию пуст.
