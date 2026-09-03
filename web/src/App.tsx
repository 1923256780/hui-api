// App 是路由表：/login 独立页；/console 挂控制台骨架与八个子页面；
// 未匹配路径一律重定向 /console（未登录时由骨架探针跳回 /login）。
import { Navigate, Route, Routes } from 'react-router-dom'
import ConsoleLayout from './components/ConsoleLayout'
import LoginPage from './pages/Login'
import DashboardPage from './pages/Dashboard'
import ChannelsPage from './pages/Channels'
import TokensPage from './pages/Tokens'
import UsersPage from './pages/Users'
import RedemptionsPage from './pages/Redemptions'
import TopUpPage from './pages/TopUp'
import LogsPage from './pages/Logs'
import ModelsPage from './pages/Models'
import SettingsPage from './pages/Settings'

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/console" element={<ConsoleLayout />}>
        <Route index element={<DashboardPage />} />
        <Route path="channels" element={<ChannelsPage />} />
        <Route path="tokens" element={<TokensPage />} />
        <Route path="users" element={<UsersPage />} />
        <Route path="redemptions" element={<RedemptionsPage />} />
        <Route path="topup" element={<TopUpPage />} />
        <Route path="logs" element={<LogsPage />} />
        <Route path="models" element={<ModelsPage />} />
        <Route path="settings" element={<SettingsPage />} />
      </Route>
      <Route path="*" element={<Navigate to="/console" replace />} />
    </Routes>
  )
}
