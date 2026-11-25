import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'node:path'

// AX_PROXY: node 1 HTTP address (mesh/loopback listener, no auth).
const target = process.env.AX_PROXY ?? 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { '@': path.resolve(import.meta.dirname, 'src') } },
  server: {
    port: 5173,
    proxy: {
      '/aetherix.': { target, changeOrigin: true },
      '/healthz': { target, changeOrigin: true },
      '/mcp': { target, changeOrigin: true },
      '/api': { target, changeOrigin: true, ws: true },
      '/oauth': { target, changeOrigin: true },
      '/.well-known': { target, changeOrigin: true },
    },
  },
  build: { outDir: 'dist', sourcemap: false, chunkSizeWarningLimit: 900 },
  test: { environment: 'jsdom', globals: true, setupFiles: ['./src/test-setup.ts'], include: ['src/**/*.test.{ts,tsx}'] },
})
