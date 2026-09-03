import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// hui-api 管理台构建配置：产物输出 dist/，经 go:embed 嵌入单二进制。
// manualChunks（M3-wave4）：依赖库与业务代码分离——react/react-dom/
// react-router-dom/dayjs 归 vendor，antd 与图标归 antd；页面组件经
// React.lazy 按路由自动切分，主 chunk 仅含路由骨架与布局（嵌入产物为
// assets/ 下带 hash 的任意文件名，go:embed all:dist 与 SPA 回退兼容）。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        manualChunks: {
          vendor: ['react', 'react-dom', 'react-router-dom', 'dayjs'],
          antd: ['antd', '@ant-design/icons'],
        },
      },
    },
  },
  server: {
    // 本地开发时把管理 API 与转发面代理到后端进程（go run ./cmd/hui-api -addr :3100）。
    proxy: {
      '/api': 'http://127.0.0.1:3100',
      '/v1': 'http://127.0.0.1:3100',
    },
  },
})
