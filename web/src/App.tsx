import { useState } from 'react'
import {
  Avatar,
  Button,
  Card,
  Empty,
  Form,
  Input,
  Layout,
  Menu,
  Typography,
  message,
} from 'antd'
import {
  CloudServerOutlined,
  DashboardOutlined,
  FileTextOutlined,
  GiftOutlined,
  KeyOutlined,
  LockOutlined,
  LogoutOutlined,
  SettingOutlined,
  UserOutlined,
} from '@ant-design/icons'

const { Header, Sider, Content } = Layout
const { Title, Text } = Typography

// 侧边导航占位：各页面在 M3（管理 API）与 M4（管理台）逐波填充。
const menuItems = [
  { key: 'dashboard', icon: <DashboardOutlined />, label: '仪表盘' },
  { key: 'channels', icon: <CloudServerOutlined />, label: '渠道' },
  { key: 'tokens', icon: <KeyOutlined />, label: '令牌' },
  { key: 'redemptions', icon: <GiftOutlined />, label: '兑换码' },
  { key: 'logs', icon: <FileTextOutlined />, label: '日志' },
  { key: 'settings', icon: <SettingOutlined />, label: '设置' },
]

/** 登录页占位：M3 交付真实鉴权（session + 管理面 API）。 */
function LoginPage({ onLogin }: { onLogin: () => void }) {
  return (
    <div className="login-backdrop">
      <Card className="login-card">
        <Title level={3} style={{ textAlign: 'center', marginBottom: 4 }}>
          Hui Api
        </Title>
        <Text type="secondary" style={{ display: 'block', textAlign: 'center', marginBottom: 24 }}>
          轻量 LLM API 网关 · 管理台
        </Text>
        <Form
          layout="vertical"
          onFinish={() => {
            message.info('登录鉴权与管理 API 在 M3 交付，当前为界面外壳')
            onLogin()
          }}
        >
          <Form.Item name="username" rules={[{ required: true, message: '请输入用户名' }]}>
            <Input prefix={<UserOutlined />} placeholder="用户名" size="large" />
          </Form.Item>
          <Form.Item name="password" rules={[{ required: true, message: '请输入密码' }]}>
            <Input.Password prefix={<LockOutlined />} placeholder="密码" size="large" />
          </Form.Item>
          <Button type="primary" htmlType="submit" block size="large">
            登录
          </Button>
        </Form>
      </Card>
    </div>
  )
}

/** 空白主布局：仅导航外壳，内容区统一 Empty 占位。 */
function AdminShell({ onLogout }: { onLogout: () => void }) {
  const [selectedKey, setSelectedKey] = useState('dashboard')
  const activeLabel = menuItems.find((m) => m.key === selectedKey)?.label ?? ''

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider theme="dark" width={200}>
        <div className="sider-brand">Hui Api</div>
        <Menu
          theme="dark"
          mode="inline"
          selectedKeys={[selectedKey]}
          items={menuItems}
          onClick={(e) => setSelectedKey(e.key)}
        />
      </Sider>
      <Layout>
        <Header className="app-header">
          <Text strong style={{ fontSize: 16 }}>
            {activeLabel}
          </Text>
          <span className="app-header-actions">
            <Avatar icon={<UserOutlined />} style={{ marginRight: 12 }} />
            <Button icon={<LogoutOutlined />} size="small" onClick={onLogout}>
              退出
            </Button>
          </span>
        </Header>
        <Content style={{ padding: 24 }}>
          <Empty description={`「${activeLabel}」页面将在后续里程碑交付`} />
        </Content>
      </Layout>
    </Layout>
  )
}

export default function App() {
  const [loggedIn, setLoggedIn] = useState(false)
  return loggedIn ? (
    <AdminShell onLogout={() => setLoggedIn(false)} />
  ) : (
    <LoginPage onLogin={() => setLoggedIn(true)} />
  )
}
