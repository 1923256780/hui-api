// Login 页对接 POST /api/user/login（M3-wave2 二段式）：
//   - 密码登录：TOTP 启用用户返回 {require_2fa:true} 并携带 stage1 会话
//     cookie → 切换到验证码输入步，POST /api/user/login/2fa 换取完整会话；
//   - OAuth 登录：按 GET /api/setup 能力发现渲染按钮，整页跳转
//     /api/oauth/:provider（服务端 302 authorize，回调成功落 /console）；
//   - 注册入口：register_enabled 时展示「注册新账号」链接。
// 失败（401 invalid_credentials / 403 禁用）原样展示服务端文案；OAuth 回传
// 的 /login?oauth_failed=1 顶部提示（docs/05 §5.8）。
import { useEffect, useState, type ReactNode } from 'react'
import { App, Alert, Button, Card, Divider, Form, Input, Typography } from 'antd'
import {
  GithubOutlined,
  GlobalOutlined,
  LockOutlined,
  SafetyOutlined,
  TeamOutlined,
  UserOutlined,
} from '@ant-design/icons'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { ApiError, api, saveSession } from '../api/client'
import type { SessionUser } from '../api/client'
import type { SetupData } from '../api/types'

const { Title, Text } = Typography

interface LoginForm {
  username: string
  password: string
}

// 登录响应二态：完整会话返回用户对象；待两步验证返回 require_2fa 标记。
type LoginData = SessionUser & { require_2fa?: boolean }

const OAUTH_PROVIDERS: Array<{ key: keyof SetupData['oauth']; label: string; icon: ReactNode }> = [
  { key: 'github', label: '使用 GitHub 登录', icon: <GithubOutlined /> },
  { key: 'linuxdo', label: '使用 LinuxDO 登录', icon: <TeamOutlined /> },
  { key: 'oidc', label: '使用 OIDC 登录', icon: <GlobalOutlined /> },
]

export default function LoginPage() {
  const navigate = useNavigate()
  const { message } = App.useApp()
  const [search] = useSearchParams()
  const [submitting, setSubmitting] = useState(false)
  const [setup, setSetup] = useState<SetupData | null>(null)
  // stage: 密码步 → 待两步验证步（持有 stage1 会话 cookie，5 分钟有效）。
  const [await2FA, setAwait2FA] = useState(false)
  const [code, setCode] = useState('')
  const [verifying, setVerifying] = useState(false)
  const oauthFailed = search.get('oauth_failed') === '1'

  useEffect(() => {
    let alive = true
    api
      .get<SetupData>('/api/setup')
      .then((d) => {
        if (alive) setSetup(d)
      })
      .catch(() => {
        // setup 不可达不阻断密码登录，仅缺失 OAuth/注册入口。
      })
    return () => {
      alive = false
    }
  }, [])

  const onFinish = async (values: LoginForm) => {
    setSubmitting(true)
    try {
      const data = await api.post<LoginData>('/api/user/login', values)
      if (data.require_2fa) {
        // 服务端已下发 stage1 会话 cookie；切验证码步，不写入本地会话元数据。
        setAwait2FA(true)
        message.info('该账号已开启两步验证，请输入验证器动态码')
        return
      }
      saveSession(data)
      message.success(`欢迎回来，${data.display_name || data.username}`)
      navigate('/console', { replace: true })
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '登录失败，请稍后重试')
    } finally {
      setSubmitting(false)
    }
  }

  const onVerify2FA = async () => {
    const trimmed = code.trim()
    if (!trimmed) {
      message.warning('请输入 6 位动态码')
      return
    }
    setVerifying(true)
    try {
      const user = await api.post<SessionUser>('/api/user/login/2fa', { code: trimmed })
      saveSession(user)
      message.success(`欢迎回来，${user.display_name || user.username}`)
      navigate('/console', { replace: true })
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '验证失败，请稍后重试')
    } finally {
      setVerifying(false)
    }
  }

  const oauthEnabled = (setup?.oauth ?? {}).github || setup?.oauth.linuxdo || setup?.oauth.oidc

  return (
    <div className="login-backdrop">
      <Card className="login-card">
        <Title level={3} style={{ textAlign: 'center', marginBottom: 4 }}>
          Hui Api
        </Title>
        <Text type="secondary" style={{ display: 'block', textAlign: 'center', marginBottom: 16 }}>
          轻量 LLM API 网关 · 管理控制台
        </Text>
        {oauthFailed && (
          <Alert
            type="error"
            showIcon
            message="第三方登录未完成"
            description="授权被取消或该身份未绑定账号且注册未开放。"
            style={{ marginBottom: 16 }}
          />
        )}
        {await2FA ? (
          <>
            <Alert
              type="info"
              showIcon
              icon={<SafetyOutlined />}
              message="两步验证"
              description="请输入验证器 App 当前动态码（6 位）。"
              style={{ marginBottom: 16 }}
            />
            <Input
              size="large"
              placeholder="动态码"
              maxLength={6}
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
              onPressEnter={onVerify2FA}
              style={{ marginBottom: 12 }}
            />
            <Button type="primary" block size="large" loading={verifying} onClick={onVerify2FA}>
              验证并登录
            </Button>
          </>
        ) : (
          <>
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
            {oauthEnabled && (
              <>
                <Divider plain style={{ margin: '16px 0 12px' }}>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    或使用第三方登录
                  </Text>
                </Divider>
                {OAUTH_PROVIDERS.filter((p) => setup?.oauth[p.key]).map((p) => (
                  <Button key={p.key} block size="large" icon={p.icon} href={`/api/oauth/${p.key}`} style={{ marginBottom: 8 }}>
                    {p.label}
                  </Button>
                ))}
              </>
            )}
            {setup?.register_enabled && (
              <Text type="secondary" style={{ display: 'block', textAlign: 'center', marginTop: 8 }}>
                还没有账号？<Link to="/register">注册新账号</Link>
              </Text>
            )}
          </>
        )}
      </Card>
    </div>
  )
}
