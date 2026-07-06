// Vite: dev-прокси на Go-бэкенд (один origin — refresh-cookie Path=/auth
// и относительные URL работают одинаково в dev и prod) + конфиг vitest.
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

const backend = process.env.BACKEND_URL || 'http://localhost:8080'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': backend,
      '/auth': backend,
      '/ws': { target: backend, ws: true },
    },
  },
  build: { outDir: 'dist' },
  test: {
    environment: 'jsdom',
    setupFiles: './src/test/setup.js',
    include: ['src/**/*.test.{js,jsx}'],
    globals: false,
  },
})
