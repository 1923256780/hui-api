import React from 'react'
import ReactDOM from 'react-dom/client'
import { ConfigProvider } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import App from './App'
import './index.css'

// 管理台入口。M1-wave1 只交付外壳：登录占位 + 空白布局，
// 主题细节（暗色模式、密度）与真实页面在 M4 逐波填充。
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ConfigProvider
      locale={zhCN}
      theme={{
        token: {
          colorPrimary: '#2f54eb',
          borderRadius: 8,
        },
      }}
    >
      <App />
    </ConfigProvider>
  </React.StrictMode>,
)
