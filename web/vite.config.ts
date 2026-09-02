import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// hui-api 管理台构建配置：产物输出 dist/，经 go:embed 嵌入单二进制。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    // 本地开发时把管理 API 代理到后端进程（go run ./cmd/hui-api -addr :3100）。
    proxy: {
      '/api': 'http://127.0.0.1:3100',
    },
  },
})
