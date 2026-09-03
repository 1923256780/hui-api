// ConsoleLayout 是控制台骨架：左侧导航、顶部会话栏与内容区出口。
// 挂载时以登录态端点 GET /api/user/self 做会话探针——鉴权探测必须用最低
// 权限端点：改用管理端点 /api/option 会把普通用户 403「需要管理员权限」
// 误判为会话失效（docs/11 守卫探针教训）。401 由 api 客户端统一跳转登录页。
// 菜单按角色渲染：渠道/用户/兑换码/系统设置为 root 专属，普通用户仅见
// 自视页面；直访 root 专属路径时回看板（防御 URL 直达）。
// M3-wave2：新增个人中心 /console/profile（全角色可见，docs/05 §5.9）。
import { useEffect, useState } from 'react'
import { App, Avatar, Button, Layout, Menu, Typography } from 'antd'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'
import {
  AppstoreOutlined,
  CloudServerOutlined,
  DashboardOutlined,
  FileTextOutlined,
  GiftOutlined,
  KeyOutlined,
  LogoutOutlined,
  SettingOutlined,
  TeamOutlined,
  UserOutlined,
  WalletOutlined,
} from '@ant-design/icons'
import { api, clearSession, getSession, saveSession, type SessionUser } from '../api/client'

const { Sider, Header, Content } = Layout
const { Text } = Typography

export const menuItems = [
  { key: '/console', icon: <DashboardOutlined />, label: '数据看板' },
  { key: '/console/tokens', icon: <KeyOutlined />, label: '令牌' },
  { key: '/console/topup', icon: <WalletOutlined />, label: '充值' },
  { key: '/console/profile', icon: <UserOutlined />, label: '个人中心' },
  { key: '/console/channels', icon: <CloudServerOutlined />, label: '渠道' },
  { key: '/console/users', icon: <TeamOutlined />, label: '用户' },
  { key: '/console/redemptions', icon: <GiftOutlined />, label: '兑换码' },
  { key: '/console/logs', icon: <FileTextOutlined />, label: '日志' },
  { key: '/console/models', icon: <AppstoreOutlined />, label: '模型广场' },
  { key: '/console/settings', icon: <SettingOutlined />, label: '系统设置' },
]

// root（role=100）专属页面：仅对管理员渲染菜单，普通用户直访时回看板。
const ADMIN_ONLY_KEYS = new Set([
  '/console/channels',
  '/console/users',
  '/console/redemptions',
  '/console/settings',
])

export default function ConsoleLayout() {
  const [self, setSelf] = useState<SessionUser | null>(null)
  const [ready, setReady] = useState(false)
  const navigate = useNavigate()
  const location = useLocation()
  const { message } = App.useApp()
  const cached = getSession()

  useEffect(() => {
    let alive = true
    // 会话探针：GET /api/user/self 任何登录用户可读（自视端点），普通用户
    // 会话不再被 403 误判；响应顺带刷新本地会话元数据（角色以服务端为准）。
    api
      .get<SessionUser>('/api/user/self')
      .then((u) => {
        if (!alive) return
        setSelf(u)
        saveSession(u)
        setReady(true)
      })
      .catch(() => {
        if (alive) navigate('/login', { replace: true })
      })
    return () => {
      alive = false
    }
  }, [navigate])

  const isAdmin = (self?.role ?? cached?.role) === 100

  // 普通用户直访 root 专属路径时回看板（菜单隐藏之外的第二道防线）。
  useEffect(() => {
    if (!ready || isAdmin) return
    if (ADMIN_ONLY_KEYS.has(location.pathname)) navigate('/console', { replace: true })
  }, [ready, isAdmin, location.pathname, navigate])

  if (!ready) return null

  const visibleMenu = menuItems.filter((m) => !ADMIN_ONLY_KEYS.has(m.key) || isAdmin)

  const handleLogout = async () => {
    try {
      await api.post('/api/user/logout')
    } catch {
      // 登出失败也照常回登录页（cookie 清理由服务端尽力完成）。
    }
    clearSession()
    message.success('已退出登录')
    navigate('/login', { replace: true })
  }

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider
        theme="dark"
        width={200}
        style={{ position: 'sticky', top: 0, height: '100vh', overflow: 'auto' }}
      >
        <div className="sider-brand">Hui Api</div>
        <Menu
          theme="dark"
          mode="inline"
          selectedKeys={[location.pathname]}
          items={visibleMenu}
          onClick={(e) => navigate(e.key)}
        />
      </Sider>
      <Layout>
        <Header className="app-header">
          <Text strong style={{ fontSize: 15 }}>
            {menuItems.find((m) => m.key === location.pathname)?.label ?? '管理控制台'}
          </Text>
          <span className="app-header-actions">
            <Avatar size="small" icon={<UserOutlined />} style={{ marginRight: 6 }} />
            <Text style={{ marginRight: 12 }}>
              {self?.display_name || self?.username || cached?.username || '管理员'}
            </Text>
            <Button size="small" icon={<LogoutOutlined />} onClick={handleLogout}>
              退出
            </Button>
          </span>
        </Header>
        <Content style={{ padding: 24 }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  )
}
