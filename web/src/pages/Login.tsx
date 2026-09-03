// Login 页对接 POST /api/user/login：成功写入本地会话元数据并进入控制台。
// 失败（401 invalid_credentials / 403 禁用）原样展示服务端文案。
import { useState } from 'react'
import { App, Button, Card, Form, Input, Typography } from 'antd'
import { LockOutlined, UserOutlined } from '@ant-design/icons'
import { useNavigate } from 'react-router-dom'
import { ApiError, api, saveSession } from '../api/client'
import type { SessionUser } from '../api/client'

const { Title, Text } = Typography

interface LoginForm {
  username: string
  password: string
}

export default function LoginPage() {
  const navigate = useNavigate()
  const { message } = App.useApp()
  const [submitting, setSubmitting] = useState(false)

  const onFinish = async (values: LoginForm) => {
    setSubmitting(true)
    try {
      const user = await api.post<SessionUser>('/api/user/login', values)
      saveSession(user)
      message.success(`欢迎回来，${user.display_name || user.username}`)
      navigate('/console', { replace: true })
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '登录失败，请稍后重试')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="login-backdrop">
      <Card className="login-card">
        <Title level={3} style={{ textAlign: 'center', marginBottom: 4 }}>
          Hui Api
        </Title>
        <Text type="secondary" style={{ display: 'block', textAlign: 'center', marginBottom: 24 }}>
          轻量 LLM API 网关 · 管理控制台
        </Text>
        <Form<LoginForm> layout="vertical" onFinish={onFinish} requiredMark={false}>
          <Form.Item name="username" rules={[{ required: true, message: '请输入用户名' }]}>
            <Input
              prefix={<UserOutlined />}
              placeholder="用户名"
              size="large"
              autoComplete="username"
            />
          </Form.Item>
          <Form.Item name="password" rules={[{ required: true, message: '请输入密码' }]}>
            <Input.Password
              prefix={<LockOutlined />}
              placeholder="密码"
              size="large"
              autoComplete="current-password"
            />
          </Form.Item>
          <Button type="primary" htmlType="submit" block size="large" loading={submitting}>
            登录
          </Button>
        </Form>
      </Card>
    </div>
  )
}
