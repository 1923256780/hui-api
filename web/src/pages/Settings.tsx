// Settings 系统设置：管理面可写键按分组编辑（计费/网关/限流/观测/注册/邮件/
// 人机校验/OAuth/支付/邀请/充值），保存走 PUT /api/option（键白名单校验、
// 任一非法整体拒绝），成功后展示新配置版本号。敏感键（password/secret）
// 出口已脱敏为 ******，保存时原样回写后端视为「保持旧值」跳过不覆盖。
// 注意：键白名单不含换皮类键（如 SystemName），后端未支持的键不在此提供，
// 避免 option_forbidden 报错（docs/05 第五节）。
import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  App,
  Button,
  Card,
  Input,
  Space,
  Spin,
  Tag,
  Typography,
} from 'antd'
import { ReloadOutlined, SaveOutlined } from '@ant-design/icons'
import { ApiError, api } from '../api/client'
import type { OptionListData } from '../api/types'

const { Text, Paragraph } = Typography

type ValueKind = 'json' | 'int' | 'bool' | 'text'

interface KeyDef {
  key: string
  label: string
  kind: ValueKind
  multiline?: boolean
  placeholder?: string
  /** 敏感键（后端出口脱敏为 ******，回写哨兵保持旧值） */
  sensitive?: boolean
}

interface GroupDef {
  title: string
  description?: string
  keys: KeyDef[]
}

// KEY_GROUPS 与后端 allowedOptionKey 白名单对齐（internal/api/option.go）。
const KEY_GROUPS: GroupDef[] = [
  {
    title: '计费配置',
    description: '三种计费模式（classic_ratio / tiered_expr / per_call）与组倍率；写后热生效。',
    keys: [
      {
        key: 'billing_setting.billing_mode',
        label: '计费模式声明',
        kind: 'json',
        multiline: true,
        placeholder: '{"model-a":"tiered_expr"}',
      },
      {
        key: 'billing_setting.billing_expr',
        label: '计费表达式（tiered_expr 用）',
        kind: 'json',
        multiline: true,
        placeholder: '{"model-a":"tier(\\"base\\", p * 0.15 + c * 0.5 + cr * 0.014)"}',
      },
      {
        key: 'billing_setting.billing_price',
        label: '按次单价（per_call 用，美元）',
        kind: 'json',
        multiline: true,
        placeholder: '{"model-b":0.02}',
      },
      {
        key: 'ModelRatio',
        label: 'ModelRatio（每百万输入 tokens 美元价，classic 回退价）',
        kind: 'json',
        multiline: true,
        placeholder: '{"model-a":0.15}',
      },
      {
        key: 'CompletionRatio',
        label: 'CompletionRatio（输出价相对输入价的倍数）',
        kind: 'json',
        multiline: true,
        placeholder: '{"model-a":3.0}',
      },
      {
        key: 'GroupRatio',
        label: 'GroupRatio（组倍率；未配置组回退 default）',
        kind: 'json',
        multiline: true,
        placeholder: '{"vip":2.0,"default":1.0}',
      },
    ],
  },
  {
    title: '网关参数',
    keys: [
      {
        key: 'relay.max_body_bytes',
        label: '请求体上限（字节）',
        kind: 'int',
        placeholder: '33554432',
      },
      {
        key: 'relay.virtual_model_groups',
        label: '虚拟模型组（/v1/models 只返回组名，成员不展开）',
        kind: 'json',
        multiline: true,
        placeholder: '{"team-llm":["model-a","model-b"]}',
      },
    ],
  },
  {
    title: '请求限流',
    description: '滑动窗口（按分钟）；分组 JSON 覆盖全局并共用窗口周期；被拒请求不消耗配额。',
    keys: [
      {
        key: 'ModelRequestRateLimitEnabled',
        label: '全局限流开关',
        kind: 'bool',
        placeholder: 'true / false',
      },
      {
        key: 'ModelRequestRateLimitDurationMinutes',
        label: '窗口时长（分钟）',
        kind: 'int',
        placeholder: '1',
      },
      {
        key: 'ModelRequestRateLimitCount',
        label: '窗口内最大请求数（0=不限）',
        kind: 'int',
        placeholder: '0',
      },
      {
        key: 'ModelRequestRateLimitSuccessCount',
        label: '窗口内最大成功数（0=不限）',
        kind: 'int',
        placeholder: '0',
      },
      {
        key: 'ModelRequestRateLimitGroup',
        label: '分组覆盖（按令牌 group 生效）',
        kind: 'json',
        multiline: true,
        placeholder: '{"vip":[5,3]}',
      },
    ],
  },
  {
    title: '观测 hooks',
    description: '异步旁路事件投递（不阻塞请求）：OTLP/HTTP JSON 指标导出与 webhook 事件推送，失败自动降级丢弃并计数（docs/05）。',
    keys: [
      {
        key: 'hooks.enabled',
        label: 'hooks 总开关',
        kind: 'bool',
        placeholder: 'true / false',
      },
      {
        key: 'hooks.otlp.endpoint',
        label: 'OTLP 导出端点（OTLP/HTTP JSON，POST <endpoint>/v1/metrics）',
        kind: 'text',
        placeholder: 'http://127.0.0.1:4318',
      },
      {
        key: 'hooks.webhook.url',
        label: 'webhook 事件推送地址（超时 3s）',
        kind: 'text',
        placeholder: 'https://example.com/hui-webhook',
      },
    ],
  },
  {
    title: '注册开关（register.*）',
    description: 'M3-wave1 公开注册体系；关闭后 POST /api/user/register 返回 403 register_disabled。',
    keys: [
      {
        key: 'register.enabled',
        label: '开放注册开关',
        kind: 'bool',
        placeholder: 'true / false',
      },
      {
        key: 'register.email_verification',
        label: '邮箱验证码校验（需先配置 SMTP）',
        kind: 'bool',
        placeholder: 'true / false',
      },
      {
        key: 'register.quota_for_new_user',
        label: '新用户初始配额',
        kind: 'int',
        placeholder: '0',
      },
    ],
  },
  {
    title: '邮件 SMTP（smtp.*）',
    description: '验证码邮件发送通道（465 端口隐式 TLS）；关闭时发码端点返回 503 smtp_not_configured。',
    keys: [
      {
        key: 'smtp.enabled',
        label: 'SMTP 总开关',
        kind: 'bool',
        placeholder: 'true / false',
      },
      {
        key: 'smtp.host',
        label: 'SMTP 主机（TLS SNI 用域名）',
        kind: 'text',
        placeholder: 'smtp.example.com',
      },
      {
        key: 'smtp.port',
        label: '端口（当前仅支持 465 隐式 TLS）',
        kind: 'int',
        placeholder: '465',
      },
      {
        key: 'smtp.username',
        label: '认证用户名（兼缺省发件人）',
        kind: 'text',
        placeholder: 'no-reply@example.com',
      },
      {
        key: 'smtp.password',
        label: '认证密码',
        kind: 'text',
        sensitive: true,
        placeholder: '已脱敏，回写 ****** 表示保持不变',
      },
      {
        key: 'smtp.from',
        label: '发件人地址（留空回退 username）',
        kind: 'text',
        placeholder: 'no-reply@example.com',
      },
    ],
  },
  {
    title: '人机校验 Turnstile（turnstile.*）',
    description: '开启后注册需通过 Cloudflare siteverify 服务端校验（5s 超时）；site_key 会随 /api/setup 下发给前端。',
    keys: [
      {
        key: 'turnstile.enabled',
        label: 'Turnstile 开关',
        kind: 'bool',
        placeholder: 'true / false',
      },
      {
        key: 'turnstile.site_key',
        label: '站点公钥（下发前端）',
        kind: 'text',
        placeholder: '0x4AAAAAAA...',
      },
      {
        key: 'turnstile.secret_key',
        label: '站点密钥（仅服务端）',
        kind: 'text',
        sensitive: true,
        placeholder: '已脱敏，回写 ****** 表示保持不变',
      },
    ],
  },
  {
    title: 'OAuth 登录（oauth.*）',
    description: 'M3-wave2 对接；键位先行预留，配置不影响当前 /api/setup 的 oauth 可用性（恒 false）。',
    keys: [
      {
        key: 'oauth.github_client_id',
        label: 'GitHub Client ID',
        kind: 'text',
        placeholder: 'Iv1.xxxx',
      },
      {
        key: 'oauth.github_client_secret',
        label: 'GitHub Client Secret',
        kind: 'text',
        sensitive: true,
        placeholder: '已脱敏，回写 ****** 表示保持不变',
      },
      {
        key: 'oauth.linuxdo_client_id',
        label: 'LINUX DO Client ID',
        kind: 'text',
        placeholder: 'xxxx',
      },
      {
        key: 'oauth.linuxdo_client_secret',
        label: 'LINUX DO Client Secret',
        kind: 'text',
        sensitive: true,
        placeholder: '已脱敏，回写 ****** 表示保持不变',
      },
      {
        key: 'oauth.oidc_issuer',
        label: 'OIDC Issuer',
        kind: 'text',
        placeholder: 'https://idp.example.com',
      },
      {
        key: 'oauth.oidc_client_id',
        label: 'OIDC Client ID',
        kind: 'text',
        placeholder: 'xxxx',
      },
      {
        key: 'oauth.oidc_client_secret',
        label: 'OIDC Client Secret',
        kind: 'text',
        sensitive: true,
        placeholder: '已脱敏，回写 ****** 表示保持不变',
      },
    ],
  },
  {
    title: '支付 EPay（epay.*）',
    description: '易支付网关参数（M3-wave2 对接，键位先行预留）。',
    keys: [
      {
        key: 'epay.enabled',
        label: 'EPay 开关',
        kind: 'bool',
        placeholder: 'true / false',
      },
      {
        key: 'epay.gateway',
        label: '网关提交地址',
        kind: 'text',
        placeholder: 'https://pay.example.com',
      },
      {
        key: 'epay.partner_id',
        label: '商户 ID',
        kind: 'text',
        placeholder: '1000',
      },
      {
        key: 'epay.partner_key',
        label: '商户密钥',
        kind: 'text',
        sensitive: true,
        placeholder: '已脱敏，回写 ****** 表示保持不变',
      },
      {
        key: 'epay.notify_url',
        label: '异步通知地址',
        kind: 'text',
        placeholder: 'https://api.example.com/api/topup/epay/notify',
      },
    ],
  },
  {
    title: '支付 Stripe（stripe.*）',
    description: 'Stripe 网关参数（M3-wave2 对接，键位先行预留）。',
    keys: [
      {
        key: 'stripe.enabled',
        label: 'Stripe 开关',
        kind: 'bool',
        placeholder: 'true / false',
      },
      {
        key: 'stripe.secret_key',
        label: 'API Secret Key',
        kind: 'text',
        sensitive: true,
        placeholder: '已脱敏，回写 ****** 表示保持不变',
      },
      {
        key: 'stripe.webhook_secret',
        label: 'Webhook 签名密钥',
        kind: 'text',
        sensitive: true,
        placeholder: '已脱敏，回写 ****** 表示保持不变',
      },
    ],
  },
  {
    title: '邀请奖励（aff.*）',
    description: '注册时经邀请码绑定的双向奖励（quota 整数）；两条 protocol=aff 日志入账，邀请码无效不阻断注册。',
    keys: [
      {
        key: 'aff.register_reward_inviter',
        label: '邀请人奖励配额',
        kind: 'int',
        placeholder: '0',
      },
      {
        key: 'aff.register_reward_invitee',
        label: '被邀请人奖励配额',
        kind: 'int',
        placeholder: '0',
      },
    ],
  },
  {
    title: '充值（topup.*）',
    description: '在线充值参数（M3-wave2 对接，键位先行预留）；充值订单落 topup_orders 表。',
    keys: [
      {
        key: 'topup.rate',
        label: '兑换比率（每 1 货币单位兑换的 quota）',
        kind: 'int',
        placeholder: '500000',
      },
      {
        key: 'topup.min_amount',
        label: '单笔最低充值金额（货币单位）',
        kind: 'int',
        placeholder: '1',
      },
    ],
  },
]

function validateValue(def: KeyDef, value: string): string | null {
  const v = value.trim()
  if (!v) return null // 留空 = 保持现有值
  switch (def.kind) {
    case 'json':
      try {
        JSON.parse(v)
        return null
      } catch {
        return '必须为合法 JSON'
      }
    case 'int': {
      const n = Number(v)
      if (!Number.isInteger(n)) return '必须为整数'
      return null
    }
    case 'bool':
      return v === 'true' || v === 'false' ? null : '必须为 true / false'
    default:
      return null
  }
}

export default function SettingsPage() {
  const { message } = App.useApp()
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [version, setVersion] = useState<number | null>(null)
  const [savedVersion, setSavedVersion] = useState<number | null>(null)
  // values 为整页编辑态；initial 为服务端当前值，用于重置与变更对比。
  const [values, setValues] = useState<Record<string, string>>({})
  const [initial, setInitial] = useState<Record<string, string>>({})

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const d = await api.get<OptionListData>('/api/option')
      const map: Record<string, string> = {}
      for (const row of d.items) {
        map[row.key] = row.value
      }
      setValues(map)
      setInitial(map)
      setVersion(d.version)
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    } finally {
      setLoading(false)
    }
  }, [message])

  useEffect(() => {
    void load()
  }, [load])

  const save = async () => {
    // 逐键校验（含值语义），任一非法整体不提交（与后端语义一致）。
    for (const group of KEY_GROUPS) {
      for (const def of group.keys) {
        const err = validateValue(def, values[def.key] ?? '')
        if (err) {
          message.error(`「${def.label}」${err}`)
          return
        }
      }
    }
    // 只提交有值的键：留空 = 保持现有值（删除键需服务端支持，当前不提供）。
    const payload: Record<string, string> = {}
    for (const group of KEY_GROUPS) {
      for (const def of group.keys) {
        const v = (values[def.key] ?? '').trim()
        if (v) payload[def.key] = v
      }
    }
    if (Object.keys(payload).length === 0) {
      message.info('没有需要保存的配置（留空表示保持现有值）')
      return
    }
    setSaving(true)
    try {
      const resp = await api.put<{ version: number; updated: number }>('/api/option', {
        options: payload,
      })
      setVersion(resp.version)
      setSavedVersion(resp.version)
      message.success(`已保存 ${resp.updated} 项，配置版本 ${resp.version}（已热生效）`)
      void load()
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    } finally {
      setSaving(false)
    }
  }

  const renderInput = (def: KeyDef) => {
    const value = values[def.key] ?? ''
    const onChange = (v: string) => setValues((prev) => ({ ...prev, [def.key]: v }))
    return def.multiline ? (
      <Input.TextArea
        rows={3}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={def.placeholder}
      />
    ) : (
      <Input value={value} onChange={(e) => onChange(e.target.value)} placeholder={def.placeholder} />
    )
  }

  return (
    <Spin spinning={loading}>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message={`配置版本：${version ?? '-'}${savedVersion !== null ? `（本次会话已保存至 v${savedVersion}）` : ''}`}
        description="写后立即热生效，无需重启；键白名单之外（如换皮展示名）暂不支持在此编辑。留空的键保持现有值；敏感键回写 ****** 表示保持不变。"
      />
      {KEY_GROUPS.map((group) => (
        <Card
          key={group.title}
          title={group.title}
          style={{ marginBottom: 16 }}
          extra={<Tag>{group.keys.length} 键</Tag>}
        >
          {group.description && <Paragraph type="secondary">{group.description}</Paragraph>}
          <Space direction="vertical" style={{ width: '100%' }} size={12}>
            {group.keys.map((def) => {
              const err = validateValue(def, values[def.key] ?? '')
              const changed = (initial[def.key] ?? '') !== (values[def.key] ?? '')
              return (
                <div key={def.key}>
                  <div style={{ marginBottom: 4 }}>
                    <Text strong>{def.label}</Text>{' '}
                    <Text code style={{ fontSize: 12 }}>
                      {def.key}
                    </Text>{' '}
                    {changed && <Tag color="orange">未保存</Tag>}
                    {def.sensitive && <Tag color="purple">已脱敏</Tag>}
                    {err && <Tag color="red">{err}</Tag>}
                  </div>
                  {renderInput(def)}
                </div>
              )
            })}
          </Space>
        </Card>
      ))}
      <Space>
        <Button type="primary" icon={<SaveOutlined />} loading={saving} onClick={() => void save()}>
          保存全部
        </Button>
        <Button icon={<ReloadOutlined />} onClick={() => void load()}>
          重新加载
        </Button>
      </Space>
    </Spin>
  )
}
