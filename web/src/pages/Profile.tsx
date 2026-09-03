// Profile 页是个人中心自服务（M3-wave2，docs/05 §5.9）：
//   - 账号信息：GET /api/user/self（quota 展示沿用原始单位）；
//   - 修改口令：POST /api/user/password（有口令账号必填旧口令；OAuth 建户的
//     无口令账号首次设置免验旧口令——旧口令留空即可）；成功后服务端重签
//     会话，无需重新登录；
//   - 修改邮箱：POST /api/user/email（格式 + 查重 409）；
//   - 两步验证 TOTP：setup → 录入动态码 enable → 需要时验码 disable
//     （三态状态机，docs/05 §5.9）；
//   - 第三方身份绑定：GET /api/user/identities 列表 + DELETE 解绑（服务端
//     防锁死：无口令且仅剩最后一个身份时拒绝 identity_last）；未绑定的
//     provider 走 /api/oauth/:provider/bind（整页跳转，回调后回本页）。
import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { App, Alert, Button, Card, Descriptions, Input, Popconfirm, Space, Table, Tag, Typography } from 'antd'
import { GithubOutlined, GlobalOutlined, SafetyCertificateOutlined, TeamOutlined } from '@ant-design/icons'
import { api } from '../api/client'
import type { SetupData, TOTPSetupData, UserIdentityView, UserInfo } from '../api/types'

const { Title, Text } = Typography

const PROVIDER_LABEL: Record<string, string> = {
  github: 'GitHub',
  linuxdo: 'LinuxDO',
  oidc: 'OIDC',
}

const PROVIDER_ICON: Record<string, ReactNode> = {
  github: <GithubOutlined />,
  linuxdo: <TeamOutlined />,
  oidc: <GlobalOutlined />,
}

interface IdentityListData {
  items: UserIdentityView[]
}

const fmtTime = (unix: number) =>
  unix <= 0 ? '—' : new Date(unix * 1000).toLocaleString('zh-CN', { hour12: false })

export default function ProfilePage() {
  const { message } = App.useApp()
  const [self, setSelf] = useState<UserInfo | null>(null)
  const [setup, setSetup] = useState<SetupData | null>(null)
  const [identities, setIdentities] = useState<UserIdentityView[]>([])

  // ===== 改密 =====
  const [oldPwd, setOldPwd] = useState('')
  const [newPwd, setNewPwd] = useState('')
  const [savingPwd, setSavingPwd] = useState(false)

  // ===== 改邮箱 =====
  const [email, setEmail] = useState('')
  const [savingEmail, setSavingEmail] = useState(false)

  // ===== 2FA =====
  const [totpSetup, setTotpSetup] = useState<TOTPSetupData | null>(null)
  const [totpCode, setTotpCode] = useState('')
  const [totpBusy, setTotpBusy] = useState(false)

  const reloadAll = useCallback(async () => {
    const [u, ids] = await Promise.all([
      api.get<UserInfo>('/api/user/self'),
      api.get<IdentityListData>('/api/user/identities').catch(() => ({ items: [] })),
    ])
    setSelf(u)
    setIdentities(ids.items ?? [])
    setEmail(u.email ?? '')
  }, [])

  useEffect(() => {
    let alive = true
    reloadAll().catch(() => {
      // 401 由 api 客户端统一跳转登录页。
    })
    api
      .get<SetupData>('/api/setup')
      .then((d) => {
        if (alive) setSetup(d)
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [reloadAll])

  const errText = (err: unknown, fallback: string) =>
    err instanceof Error && err.message ? err.message : fallback

  const onChangePassword = async () => {
    if (newPwd.length < 6) {
      message.warning('新口令至少 6 位')
      return
    }
    setSavingPwd(true)
    try {
      await api.post('/api/user/password', { old_password: oldPwd, new_password: newPwd })
      message.success('口令已修改（其他设备的登录已失效）')
      setOldPwd('')
      setNewPwd('')
    } catch (err) {
      message.error(errText(err, '修改口令失败'))
    } finally {
      setSavingPwd(false)
    }
  }

  const onChangeEmail = async () => {
    const trimmed = email.trim()
    if (!trimmed || !trimmed.includes('@')) {
      message.warning('请输入有效邮箱')
      return
    }
    setSavingEmail(true)
    try {
      await api.post<{ email: string }>('/api/user/email', { email: trimmed })
      message.success('邮箱已更新')
      await reloadAll()
    } catch (err) {
      message.error(errText(err, '修改邮箱失败'))
    } finally {
      setSavingEmail(false)
    }
  }

  const onTotpSetup = async () => {
    setTotpBusy(true)
    try {
      setTotpSetup(await api.post<TOTPSetupData>('/api/user/totp/setup'))
    } catch (err) {
      message.error(errText(err, '生成两步验证密钥失败'))
    } finally {
      setTotpBusy(false)
    }
  }

  const onTotpEnable = async () => {
    if (!totpCode.trim()) {
      message.warning('请输入验证器动态码')
      return
    }
    setTotpBusy(true)
    try {
      await api.post('/api/user/totp/enable', { code: totpCode.trim() })
      message.success('两步验证已开启，下次登录需输入动态码')
      setTotpSetup(null)
      setTotpCode('')
      await reloadAll()
    } catch (err) {
      message.error(errText(err, '开启两步验证失败'))
    } finally {
      setTotpBusy(false)
    }
  }

  const onTotpDisable = async () => {
    if (!totpCode.trim()) {
      message.warning('请输入验证器动态码以确认关闭')
      return
    }
    setTotpBusy(true)
    try {
      await api.post('/api/user/totp/disable', { code: totpCode.trim() })
      message.success('两步验证已关闭')
      setTotpCode('')
      await reloadAll()
    } catch (err) {
      message.error(errText(err, '关闭两步验证失败'))
    } finally {
      setTotpBusy(false)
    }
  }

  const onBind = (provider: string) => {
    // 整页跳转发起绑定：服务端 302 authorize，回调成功回 /console/profile。
    window.location.assign(`/api/oauth/${provider}/bind`)
  }

  const onUnbind = async (id: number) => {
    try {
      await api.del(`/api/user/identities/${id}`)
      message.success('已解绑')
      await reloadAll()
    } catch (err) {
      message.error(errText(err, '解绑失败'))
    }
  }

  const boundProviders = new Set(identities.map((i) => i.provider))
  const availableProviders = (['github', 'linuxdo', 'oidc'] as const).filter(
    (p) => setup?.oauth[p] && !boundProviders.has(p),
  )

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Card>
        <Title level={4} style={{ marginTop: 0 }}>
          账号信息
        </Title>
        {self && (
          <Descriptions column={{ xs: 1, sm: 2, lg: 3 }} size="small">
            <Descriptions.Item label="用户名">{self.username}</Descriptions.Item>
            <Descriptions.Item label="角色">
              {self.role === 100 ? <Tag color="gold">管理员</Tag> : <Tag>普通用户</Tag>}
            </Descriptions.Item>
            <Descriptions.Item label="余额（原始单位）">{self.quota}</Descriptions.Item>
            <Descriptions.Item label="邮箱">{self.email || '—'}</Descriptions.Item>
            <Descriptions.Item label="注册时间">{fmtTime(self.created_time)}</Descriptions.Item>
            <Descriptions.Item label="最近登录">{fmtTime(self.last_login_time)}</Descriptions.Item>
          </Descriptions>
        )}
      </Card>

      <Card title="修改口令">
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          <Input.Password
            placeholder="旧口令（无口令账号首次设置留空）"
            value={oldPwd}
            onChange={(e) => setOldPwd(e.target.value)}
            autoComplete="current-password"
          />
          <Input.Password
            placeholder="新口令（至少 6 位）"
            value={newPwd}
            onChange={(e) => setNewPwd(e.target.value)}
            autoComplete="new-password"
          />
          <Button type="primary" loading={savingPwd} onClick={onChangePassword}>
            保存新口令
          </Button>
          <Text type="secondary">
            修改成功后其他设备的会话将全部失效；当前会话自动续用。OAuth 登录且从未设置口令的账号可在此首次设置（旧口令留空）。
          </Text>
        </Space>
      </Card>

      <Card title="修改邮箱">
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          <Input
            placeholder="新邮箱"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="email"
          />
          <Button loading={savingEmail} onClick={onChangeEmail}>
            保存邮箱
          </Button>
          <Text type="secondary">邮箱用于忘记密码找回；被其他账号占用时将提示冲突。</Text>
        </Space>
      </Card>

      <Card title="两步验证（TOTP）">
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          {self?.totp_enabled ? (
            <>
              <Alert
                type="success"
                showIcon
                icon={<SafetyCertificateOutlined />}
                message="已开启两步验证"
                description="关闭需输入验证器当前动态码确认。"
              />
              <Input
                placeholder="动态码"
                maxLength={6}
                value={totpCode}
                onChange={(e) => setTotpCode(e.target.value.replace(/\D/g, ''))}
                style={{ maxWidth: 240 }}
              />
              <Button danger loading={totpBusy} onClick={onTotpDisable}>
                关闭两步验证
              </Button>
            </>
          ) : totpSetup ? (
            <>
              <Alert
                type="info"
                message="第一步：将密钥录入验证器 App"
                description={
                  <>
                    <Text code style={{ wordBreak: 'break-all' }}>
                      {totpSetup.secret}
                    </Text>
                    <br />
                    <Text type="secondary">
                      或直接打开 otpauth 链接：
                      <a href={totpSetup.otpauth_uri}>{totpSetup.otpauth_uri}</a>
                    </Text>
                  </>
                }
              />
              <Input
                placeholder="输入 App 显示的 6 位动态码确认"
                maxLength={6}
                value={totpCode}
                onChange={(e) => setTotpCode(e.target.value.replace(/\D/g, ''))}
                style={{ maxWidth: 240 }}
              />
              <Space>
                <Button type="primary" loading={totpBusy} onClick={onTotpEnable}>
                  确认开启
                </Button>
                <Button
                  onClick={() => {
                    setTotpSetup(null)
                    setTotpCode('')
                  }}
                >
                  取消
                </Button>
              </Space>
            </>
          ) : (
            <>
              <Alert
                type="warning"
                showIcon
                message="未开启两步验证"
                description="开启后登录需额外输入验证器动态码，大幅提升账号安全性。"
              />
              <Button type="primary" loading={totpBusy} onClick={onTotpSetup}>
                启用两步验证
              </Button>
            </>
          )}
        </Space>
      </Card>

      <Card title="第三方身份绑定">
        <Table<UserIdentityView>
          rowKey="id"
          size="small"
          pagination={false}
          dataSource={identities}
          columns={[
            {
              title: '提供方',
              dataIndex: 'provider',
              render: (p: string) => (
                <Space>
                  {PROVIDER_ICON[p] ?? <GlobalOutlined />}
                  {PROVIDER_LABEL[p] ?? p}
                </Space>
              ),
            },
            { title: '外部 ID', dataIndex: 'provider_uid' },
            { title: '绑定时间', dataIndex: 'created_time', render: fmtTime },
            {
              title: '操作',
              key: 'op',
              render: (_, row) => (
                <Popconfirm
                  title="确认解绑该身份？"
                  description="无口令账号解绑最后一个身份后将无法登录。"
                  onConfirm={() => onUnbind(row.id)}
                >
                  <Button size="small" danger>
                    解绑
                  </Button>
                </Popconfirm>
              ),
            },
          ]}
          locale={{ emptyText: '尚未绑定任何第三方身份' }}
        />
        {availableProviders.length > 0 && (
          <Space style={{ marginTop: 12 }} wrap>
            {availableProviders.map((p) => (
              <Button key={p} icon={PROVIDER_ICON[p]} onClick={() => onBind(p)}>
                绑定 {PROVIDER_LABEL[p] ?? p}
              </Button>
            ))}
          </Space>
        )}
        <Text type="secondary" style={{ display: 'block', marginTop: 8 }}>
          绑定后可使用第三方身份一键登录；建议同时设置口令或保留至少一个其他登录方式，避免锁死。
        </Text>
      </Card>
    </Space>
  )
}
