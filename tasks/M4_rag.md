# M4 — RAG (Retrieval-Augmented Generation)

**Ветка:** `feat/m4-rag`
**Зависимости:** M1, M3
**Разделы SRS:** §7.1 (RAG Architecture — Voyage), §7.3 (Conversation Summary)

## Цель
Обогатить контекст Claude релевантными знаниями из базы через векторный поиск.
Voyage-эмбеддинги + pgvector. Плюс фоновая генерация summary с защитой от гонки.

## Задачи
1. **Voyage AI эмбеддинги** (§7.1, жёсткое правило §4.2 — НЕ OpenAI):
   клиент для `voyage-3`, `dimensions: 1024`, ключ `${VOYAGE_API_KEY}`.
2. Индексация базы знаний: чанкинг документов → эмбеддинг → запись в pgvector
   (`vector(1024)`, HNSW index: ef_construction=200, m=16, ef_search=100).
3. **Retrieval** в пайплайне воркера (интеграция с M3):
   - эмбеддинг запроса → top-K=5 при cosine ≥ 0.78 (`rag.cosine_threshold`);
   - инжект найденных чанков в system prompt перед историей.
4. **Fallback** (§7.1): при 0 результатов → отвечать без RAG, писать запись
   `rag_miss` в `rag_audit` (results=0).
5. **Conversation Summary** (§7.3): каждые 15 inbound-сообщений — фоновая задача
   генерации summary. **Distributed lock** против гонки:
   ```
   SET summary:{lead_id} 1 EX 60 NX   → nil = занято, skip
   ... запись summary в БД ...
   DEL summary:{lead_id}
   ```
6. Инжект summary в контекст (укладывается в бюджет 1000 токенов из M3).
7. Скрипт калибровки порога `scripts/rag_calibrate` (§7.1): считает recall@5
   на тестовой выборке. Если < 90% — рекомендует снизить threshold до 0.72.

## Контракт наружу
- Обогащённый RAG-контекст в диалоге.
- Механизм summary — сжимает длинные диалоги.

## Критерии приёмки
- [ ] RAG работает на Voyage; `OPENAI_API_KEY` нигде не требуется (AQ²-2).
- [ ] 0 результатов retrieval → fallback без RAG + запись в rag_audit (AQ²-11 доп.).
- [ ] 1000 конкурентных воркеров на 15-м сообщении → ровно 1 summary (IQ-5).
- [ ] Cosine threshold и top_k читаются из конфига.
