// Register 页对接 POST /api/user/register（M3-wave2，docs/05 §5.6）：
//   - 按 GET /api/setup 能力发现条件渲染：邮箱验证码（email_verification）、
//     Cloudflare Turnstile 人机校验（turnstile_site_key）；
//   - 验证码发送走 POST /api/verification_code（purpose=register），60s 冷却
//     倒计时（服务端另有同邮箱 60s 限频与每日上限，429 原样提示）；
//   - aff_code 支持 /register?aff=XXXX 邀请链接预填。
// 注册成功（201）跳登录页；409 查重冲突等服务端文案原样展示。
import { useEffect, useRef, useState } from 'react'
import { App, Button, Card, Form, Input, Typography } from 'antd'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { ApiError, api } from '../api/client'
import type { SetupData } from '../api/types'

const { Title, Text } = Typography

interface RegisterForm {
  username: string
  password: string
  confirm: string
  email: string
  code?: string
  aff_code?: string
}

declare global {
  interface Window {
    turnstile?: {
      render: (el: HTMLElement, opts: Record<string, unknown>) => string
      remove: (id: string) => void
    }
  }
}

// TurnstileWidget 轻量封装：显式注入 Cloudflare 脚本并渲染 challenge，
// token 经 onToken 回传（过期置空强制重试）。未启用人机校验时不渲染。
function TurnstileWidget({ siteKey, onToken }: { siteKey: string; onToken: (t: string) => void }) {
  const holder = useRef<HTMLDivElement>(null)
  const wid = useRef('')
  useEffect(() => {
    let cancelled = false
    const render = () => {
      if (cancelled || !holder.current || !window.turnstile || wid.current) return
      wid.current = window.turnstile.render(holder.current, {
        sitekey: siteKey,
        callback: (t: string) => onToken(t),
        'expired-callback': () => onToken(''),
      })
    }
    if (window.turnstile) {
      render()
    } else {
      const s = document.createElement('script')
      s.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit'
      s.async = true
      s.onload = render
      document.head.appendChild(s)
    }
    return () => {
      cancelled = true
      if (wid.current && window.turnstile) {
        window.turnstile.remove(wid.current)
        wid.current = ''
      }
    }
  }, [siteKey, onToken])
  return <div ref={holder} />
}

export default function RegisterPage() {
  const navigate = useNavigate()
  const { message } = App.useApp()
  const [search] = useSearchParams()
  const [form] = Form.useForm<RegisterForm>()
  const [submitting, setSubmitting] = useState(false)
  const [setup, setSetup] = useState<SetupData | null>(null)
  const [cooldown, setCooldown] = useState(0)
  const [sending, setSending] = useState(false)
  const [turnstileToken, setTurnstileToken] = useState('')

  useEffect(() => {
    let alive = true
    api
      .get<SetupData>('/api/setup')
      .then((d) => {
        if (alive) setSetup(d)
      })
      .catch(() => {
        // setup 不可达时仍可提交基础字段，服务端会给出明确错误。
      })
    return () => {
      alive = false
    }
  }, [])

  // 冷却倒计时（与服务端同邮箱 60s 限频对齐）。
  useEffect(() => {
    if (cooldown <= 0) return
    const t = setInterval(() => setCooldown((c) => c - 1), 1000)
    return () => clearInterval(t)
  }, [cooldown])

  const sendCode = async () => {
    const email = (form.getFieldValue('email') as string | undefined)?.trim() ?? ''
    if (!email || !email.includes('@')) {
      message.warning('请先填写有效邮箱')
      return
    }
    setSending(true)
    try {
      await api.post('/api/verification_code', { email, purpose: 'register' })
      message.success('验证码已发送，请查收邮箱（10 分钟内有效）')
      setCooldown(60)
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '发送失败，请稍后重试')
    } finally {
      setSending(false)
    }
  }

  const onFinish = async (v: RegisterForm) => {
    setSubmitting(true)
    try {
      await api.post<{ id: number; username: string; aff_code: string }>('/api/user/register', {
        username: v.username,
        password: v.password,
        email: v.email,
        code: v.code ?? '',
        aff_code: v.aff_code ?? '',
        turnstile_token: turnstileToken,
      })
      message.success('注册成功，请登录')
      navigate('/login', { replace: true })
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '注册失败，请稍后重试')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="login-backdrop">
      <Card className="login-card" styles={{ body: { padding: '32px 32px 24px' } }}>
        <Title level={3} style={{ textAlign: 'center', marginBottom: 4 }}>
          注册新账号
        </Title>
        <Text type="secondary" style={{ display: 'block', textAlign: 'center', marginBottom: 20 }}>
          Hui Api · 开放注册
        </Text>
        <Form<RegisterForm>
          form={form}
          layout="vertical"
          onFinish={onFinish}
          requiredMark={false}
          initialValues={{ aff_code: search.get('aff') ?? '' }}
        >
          <Form.Item name="username" rules={[{ required: true, message: '请输入用户名' }]}>
            <Input placeholder="用户名" autoComplete="username" />
          </Form.Item>
          <Form.Item
            name="password"
            rules={[
              { required: true, message: '请输入密码' },
              { min: 6, message: '密码至少 6 位' },
            ]}
          >
            <Input.Password placeholder="密码（至少 6 位）" autoComplete="new-password" />
          </Form.Item>
          <Form.Item
            name="confirm"
            dependencies={['password']}
            rules={[
              { required: true, message: '请再次输入密码' },
              ({ getFieldValue }) => ({
                validator(_, value) {
                  if (!value || getFieldValue('password') === value) return Promise.resolve()
                  return Promise.reject(new Error('两次输入的密码不一致'))
                },
              }),
            ]}
          >
            <Input.Password placeholder="确认密码" autoComplete="new-password" />
          </Form.Item>
          <Form.Item
            name="email"
            rules={[
              { required: true, message: '请输入邮箱' },
              { type: 'email', message: '邮箱格式不正确' },
            ]}
          >
            <Input placeholder="邮箱" autoComplete="email" />
          </Form.Item>
          {setup?.email_verification && (
            <Form.Item
              name="code"
              rules={[{ required: true, message: '请输入邮箱验证码' }]}
              extra="验证码已发送至邮箱，10 分钟内有效"
            >
              <Input
                placeholder="邮箱验证码"
                maxLength={8}
                addonAfter={
                  <Button
                    type="link"
                    size="small"
                    style={{ padding: 0 }}
                    disabled={cooldown > 0}
                    loading={sending}
                    onClick={sendCode}
                  >
                    {cooldown > 0 ? `${cooldown}s 后重发` : '发送验证码'}
                  </Button>
                }
              />
            </Form.Item>
          )}
          <Form.Item name="aff_code">
            <Input placeholder="邀请码（选填）" maxLength={8} />
          </Form.Item>
          {setup?.turnstile_site_key && (
            <Form.Item>
              <TurnstileWidget siteKey={setup.turnstile_site_key} onToken={setTurnstileToken} />
            </Form.Item>
          )}
          <Button type="primary" htmlType="submit" block size="large" loading={submitting}>
            注册
          </Button>
        </Form>
        <Text type="secondary" style={{ display: 'block', textAlign: 'center', marginTop: 12 }}>
          已有账号？<a href="/login">返回登录</a>
        </Text>
      </Card>
    </div>
  )
}
