// App 是路由表：/login 与 /register 独立页；/console 挂控制台骨架与
// 十一个子页面（M3-wave3 新增邀请返利 /console/invite）；全部页面组件经
// React.lazy 按需加载（M3-wave4 bundle 优化：主 chunk 仅含路由骨架，各页面
// 独立 chunk，AntD/vendor 由 manualChunks 拆分），Suspense fallback 用轻量
// Spin；未匹配路径一律重定向 /console（未登录时由骨架探针跳回 /login）。
import { lazy, Suspense } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { Spin } from 'antd'
import ConsoleLayout from './components/ConsoleLayout'

const LoginPage = lazy(() => import('./pages/Login'))
const RegisterPage = lazy(() => import('./pages/Register'))
const DashboardPage = lazy(() => import('./pages/Dashboard'))
const ChannelsPage = lazy(() => import('./pages/Channels'))
const TokensPage = lazy(() => import('./pages/Tokens'))
const UsersPage = lazy(() => import('./pages/Users'))
const RedemptionsPage = lazy(() => import('./pages/Redemptions'))
const TopUpPage = lazy(() => import('./pages/TopUp'))
const LogsPage = lazy(() => import('./pages/Logs'))
const ModelsPage = lazy(() => import('./pages/Models'))
const SettingsPage = lazy(() => import('./pages/Settings'))
const ProfilePage = lazy(() => import('./pages/Profile'))
const InvitePage = lazy(() => import('./pages/Invite'))

// PageFallback 路由级加载态：全屏居中 Spin（首屏与代码分割 chunk 加载共用）。
function PageFallback() {
  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
      }}
    >
      <Spin size="large" />
    </div>
  )
}

export default function App() {
  return (
    <Suspense fallback={<PageFallback />}>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/register" element={<RegisterPage />} />
        <Route path="/console" element={<ConsoleLayout />}>
          <Route index element={<DashboardPage />} />
          <Route path="channels" element={<ChannelsPage />} />
          <Route path="tokens" element={<TokensPage />} />
          <Route path="users" element={<UsersPage />} />
          <Route path="redemptions" element={<RedemptionsPage />} />
          <Route path="topup" element={<TopUpPage />} />
          <Route path="invite" element={<InvitePage />} />
          <Route path="logs" element={<LogsPage />} />
          <Route path="models" element={<ModelsPage />} />
          <Route path="settings" element={<SettingsPage />} />
          <Route path="profile" element={<ProfilePage />} />
        </Route>
        <Route path="*" element={<Navigate to="/console" replace />} />
      </Routes>
    </Suspense>
  )
}
