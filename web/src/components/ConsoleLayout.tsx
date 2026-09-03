// ConsoleLayout 是控制台骨架：左侧导航、顶部会话栏与内容区出口。
// 挂载时以管理端点做会话探针，401 由 api 客户端统一跳转登录页。
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
import { api, clearSession, getSession } from '../api/client'

const { Sider, Header, Content } = Layout
const { Text } = Typography

export const menuItems = [
  { key: '/console', icon: <DashboardOutlined />, label: '数据看板' },
  { key: '/console/channels', icon: <CloudServerOutlined />, label: '渠道' },
  { key: '/console/tokens', icon: <KeyOutlined />, label: '令牌' },
  { key: '/console/users', icon: <TeamOutlined />, label: '用户' },
  { key: '/console/redemptions', icon: <GiftOutlined />, label: '兑换码' },
  { key: '/console/topup', icon: <WalletOutlined />, label: '充值' },
  { key: '/console/logs', icon: <FileTextOutlined />, label: '日志' },
  { key: '/console/models', icon: <AppstoreOutlined />, label: '模型广场' },
  { key: '/console/settings', icon: <SettingOutlined />, label: '系统设置' },
]

export default function ConsoleLayout() {
  const [ready, setReady] = useState(false)
  const navigate = useNavigate()
  const location = useLocation()
  const { message } = App.useApp()
  const user = getSession()

  useEffect(() => {
    let alive = true
    // 会话探针：管理端点可读则视为已登录；401 由客户端统一跳 /login。
    api
      .get('/api/option')
      .then(() => {
        if (alive) setReady(true)
      })
      .catch(() => {
        if (alive) navigate('/login', { replace: true })
      })
    return () => {
      alive = false
    }
  }, [navigate])

  if (!ready) return null

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
          items={menuItems}
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
              {user?.display_name || user?.username || '管理员'}
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
