// Конфиг e2e-прогона (критерии приёмки M10) — живой бэкенд обязателен:
//   docker compose up -d && ./scripts/m10_e2e_setup.sh
//   cd web && E2E_BASE=http://localhost:8080 npm run test:e2e
// Без E2E_BASE весь прогон скипается (конвенция проекта — как
// POSTGRES_TEST_DSN/REDIS_TEST_ADDR у Go-тестов).
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

const base = process.env.E2E_BASE || 'http://localhost:8080'

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    // location jsdom = базовый URL бэкенда: App строит ws://<host>/ws/kanban
    // от location, как в проде.
    environmentOptions: { jsdom: { url: base } },
    setupFiles: './src/test/setup.js', // относительно корня web/, не папки конфига
    include: ['**/*.e2e.test.{js,jsx}'],
    testTimeout: 30_000,
    hookTimeout: 30_000,
    // Тесты делят одного лида и идут строго по порядку (erasure — последним).
    fileParallelism: false,
  },
})
