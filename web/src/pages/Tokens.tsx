// Tokens 令牌管理：分页列表（额度进度条/分组/限流/过期）、新建/编辑抽屉、
// 创建后明文密钥一次性弹窗（sk- 仅此一次，docs/05 契约）。
import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  App,
  Button,
  DatePicker,
  Drawer,
  Form,
  Input,
  InputNumber,
  Popconfirm,
  Progress,
  Radio,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { FileAddOutlined, ReloadOutlined } from '@ant-design/icons'
import dayjs, { type Dayjs } from 'dayjs'
import { ApiError, api } from '../api/client'
import type { Paged, Token, UserInfo } from '../api/types'
import { COMMON_STATUS, formatExpiry, quotaToDollar, safeParse, statusTag } from '../api/format'

const { Text, Paragraph } = Typography

const BUDGET_OPTIONS = [
  { value: '', label: '不限周期' },
  { value: '24h', label: '每 24 小时' },
  { value: '7d', label: '每 7 天' },
  { value: '30d', label: '每 30 天' },
  { value: 'monthly', label: '每自然月' },
]

interface TokenFormValues {
  user_id: number
  name: string
  unlimited_quota: boolean
  quota?: number
  remain_quota?: number
  budget_duration: string
  expiredMode: 'forever' | 'custom'
  expired_at?: Dayjs | null
  group?: string
  model_limits?: string[]
  tpm?: number | null
  rpm?: number | null
  tags?: string[]
  allow_ips?: string
  status: boolean
}

function rateCell(text: string) {
  const parsed = safeParse<{ tpm?: number; rpm?: number }>(text)
  if (!parsed || (!parsed.tpm && !parsed.rpm)) return <Text type="secondary">-</Text>
  const parts: string[] = []
  if (parsed.tpm) parts.push(`TPM ${parsed.tpm}`)
  if (parsed.rpm) parts.push(`RPM ${parsed.rpm}`)
  return <Tag>{parts.join(' · ')}</Tag>
}

export default function TokensPage() {
  const { message, modal } = App.useApp()
  const [data, setData] = useState<Paged<Token> | null>(null)
  const [users, setUsers] = useState<UserInfo[]>([])
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const [drawerOpen, setDrawerOpen] = useState(false)
  const [editing, setEditing] = useState<Token | null>(null)
  const [saving, setSaving] = useState(false)
  const [form] = Form.useForm<TokenFormValues>()

  const load = useCallback(
    async (p = page, ps = pageSize) => {
      setLoading(true)
      try {
        const d = await api.get<Paged<Token>>('/api/token', { page: p, page_size: ps })
        setData(d)
      } catch (err) {
        if (err instanceof ApiError && err.status !== 401) {
          message.error(err.message)
        }
      } finally {
        setLoading(false)
      }
    },
    [page, pageSize, message],
  )

  useEffect(() => {
    void load()
    // 用户下拉数据（归属选择），一次拉取即可。
    api
      .get<Paged<UserInfo>>('/api/user', { page: 1, page_size: 100 })
      .then((d) => setUsers(d.items))
      .catch(() => undefined)
  }, [load])

  const openCreate = () => {
    setEditing(null)
    form.resetFields()
    form.setFieldsValue({
      unlimited_quota: false,
      budget_duration: '',
      expiredMode: 'forever',
      status: true,
      model_limits: [],
      tags: [],
    })
    setDrawerOpen(true)
  }

  const openEdit = (t: Token) => {
    setEditing(t)
    form.resetFields()
    const rate = safeParse<{ tpm?: number; rpm?: number }>(t.tpm_rpm)
    form.setFieldsValue({
      user_id: t.user_id,
      name: t.name,
      unlimited_quota: t.unlimited_quota,
      quota: t.quota,
      remain_quota: t.remain_quota,
      budget_duration: t.budget_duration ?? '',
      expiredMode: t.expired_time === -1 ? 'forever' : 'custom',
      expired_at: t.expired_time > 0 ? dayjs(t.expired_time * 1000) : null,
      group: t.group,
      model_limits: t.model_limits ? t.model_limits.split(',').filter(Boolean) : [],
      tpm: rate?.tpm ?? null,
      rpm: rate?.rpm ?? null,
      tags: safeParse<string[]>(t.tags) ?? [],
      allow_ips: t.allow_ips,
      status: t.status !== 2,
    })
    setDrawerOpen(true)
  }

  const submit = async () => {
    const values = await form.validateFields()
    setSaving(true)
    try {
      const rate: Record<string, number> = {}
      if (values.tpm) rate.tpm = values.tpm
      if (values.rpm) rate.rpm = values.rpm
      const payload = {
        user_id: values.user_id,
        name: values.name.trim(),
        status: values.status ? 1 : 2,
        quota: values.unlimited_quota ? 0 : (values.quota ?? 0),
        remain_quota: values.unlimited_quota ? 0 : (values.remain_quota ?? 0),
        unlimited_quota: values.unlimited_quota,
        budget_duration: values.budget_duration ?? '',
        expired_time:
          values.expiredMode === 'custom' && values.expired_at
            ? values.expired_at.unix()
            : -1,
        group: (values.group ?? '').trim(),
        model_limits: (values.model_limits ?? []).join(','),
        tpm_rpm: Object.keys(rate).length ? JSON.stringify(rate) : '',
        tags: JSON.stringify(values.tags ?? []),
        allow_ips: (values.allow_ips ?? '').trim(),
      }
      if (editing) {
        // 整对象写：remain_quota 显式携带表单值，避免编辑其它字段时误重置剩余。
        await api.put(`/api/token/${editing.id}`, payload)
        message.success('令牌已更新（鉴权缓存已失效）')
        setDrawerOpen(false)
        void load()
      } else {
        // 创建不传 remain（后端缺省 = quota）；明文密钥仅响应一次。
        const created = await api.post<{ token: Token; key: string }>('/api/token', payload)
        setDrawerOpen(false)
        void load()
        showKey(created.key)
      }
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      } else if (!(err instanceof ApiError)) {
        message.error('保存失败，请重试')
      }
    } finally {
      setSaving(false)
    }
  }

  // showKey 创建成功后的明文密钥弹窗：仅此一次，含复制与强警示。
  const showKey = (key: string) => {
    modal.success({
      title: '令牌创建成功',
      width: 520,
      content: (
        <div>
          <Alert
            type="warning"
            showIcon
            message="明文密钥仅显示这一次"
            description="关闭弹窗后无法再次查看（库内只存哈希）。请立即复制并保存到安全位置。"
            style={{ marginBottom: 16 }}
          />
          <Paragraph code copyable style={{ marginBottom: 0, wordBreak: 'break-all' }}>
            {key}
          </Paragraph>
        </div>
      ),
      okText: '我已保存',
    })
  }

  const remove = async (id: number) => {
    try {
      await api.del(`/api/token/${id}`)
      message.success('令牌已删除')
      void load()
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    }
  }

  const userName = (id: number) => users.find((u) => u.id === id)?.username ?? `#${id}`

  const columns: ColumnsType<Token> = [
    { title: 'ID', dataIndex: 'id', width: 64 },
    { title: '名称', dataIndex: 'name', width: 140, ellipsis: true },
    {
      title: '归属用户',
      dataIndex: 'user_id',
      width: 110,
      render: (v: number) => userName(v),
    },
    {
      title: '分组',
      dataIndex: 'group',
      width: 96,
      render: (v: string) => <Tag color="blue">{v || 'default'}</Tag>,
    },
    {
      title: '剩余额度',
      key: 'quota',
      width: 170,
      render: (_, record) => {
        if (record.unlimited_quota) return <Tag color="gold">无限额度</Tag>
        if (record.quota <= 0) return <Text type="secondary">0</Text>
        const percent = Math.max(0, Math.min(100, (record.remain_quota / record.quota) * 100))
        return (
          <Tooltip_quota remain={record.remain_quota} quota={record.quota} percent={percent} />
        )
      },
    },
    {
      title: '预算周期',
      dataIndex: 'budget_duration',
      width: 100,
      render: (v: string) => BUDGET_OPTIONS.find((o) => o.value === v)?.label ?? (v || '-'),
    },
    { title: '限流', dataIndex: 'tpm_rpm', width: 170, render: (v: string) => rateCell(v) },
    {
      title: '过期时间',
      dataIndex: 'expired_time',
      width: 160,
      render: (v: number) => formatExpiry(v),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 84,
      render: (v: number) => {
        const tag = statusTag(v, COMMON_STATUS)
        return <Tag color={tag.color}>{tag.text}</Tag>
      },
    },
    {
      title: '操作',
      key: 'action',
      width: 140,
      render: (_, record) => (
        <Space size="small">
          <Button size="small" onClick={() => openEdit(record)}>
            编辑
          </Button>
          <Popconfirm title="删除令牌" description="删除后该密钥立即失效，确认？" onConfirm={() => void remove(record.id)}>
            <Button size="small" danger>
              删除
            </Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <div className="page-toolbar">
        <Button icon={<ReloadOutlined />} onClick={() => void load()}>
          刷新
        </Button>
        <Button type="primary" icon={<FileAddOutlined />} onClick={openCreate}>
          新建令牌
        </Button>
      </div>
      <Table<Token>
        rowKey="id"
        loading={loading}
        columns={columns}
        dataSource={data?.items ?? []}
        pagination={{
          current: page,
          pageSize,
          total: data?.total ?? 0,
          showSizeChanger: true,
          showTotal: (t) => `共 ${t} 条`,
          onChange: (p, ps) => {
            setPage(p)
            setPageSize(ps)
          },
        }}
        scroll={{ x: 1300 }}
      />

      <Drawer
        title={editing ? `编辑令牌 #${editing.id}` : '新建令牌'}
        width={560}
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        destroyOnClose
        extra={
          <Space>
            <Button onClick={() => setDrawerOpen(false)}>取消</Button>
            <Button type="primary" loading={saving} onClick={() => void submit()}>
              保存
            </Button>
          </Space>
        }
      >
        <Form<TokenFormValues> form={form} layout="vertical">
          <Form.Item name="user_id" label="归属用户" rules={[{ required: true, message: '请选择归属用户' }]}>
            <Select
              disabled={!!editing}
              showSearch
              optionFilterProp="label"
              options={users.map((u) => ({ value: u.id, label: `${u.username}（#${u.id}）` }))}
              placeholder="选择归属用户"
            />
          </Form.Item>
          <Form.Item name="name" label="名称">
            <Input placeholder="如：team-a 网关密钥" />
          </Form.Item>
          <Form.Item name="status" label="状态" valuePropName="checked">
            <Switch checkedChildren="启用" unCheckedChildren="禁用" />
          </Form.Item>
          <Form.Item name="unlimited_quota" label="无限额度" valuePropName="checked">
            <Switch />
          </Form.Item>
          <Form.Item
            noStyle
            shouldUpdate={(prev, cur) => prev.unlimited_quota !== cur.unlimited_quota}
          >
            {({ getFieldValue }) =>
              getFieldValue('unlimited_quota') ? null : (
                <>
                  <Form.Item name="quota" label="总额度（quota）" tooltip={`500000 quota = $1`}>
                    <InputNumber min={0} style={{ width: '100%' }} placeholder="预算周期内可用额度" />
                  </Form.Item>
                  {editing && (
                    <Form.Item
                      name="remain_quota"
                      label="剩余额度（quota）"
                      extra="编辑可调整剩余；新建时后端自动置为总额度"
                    >
                      <InputNumber min={0} style={{ width: '100%' }} />
                    </Form.Item>
                  )}
                </>
              )
            }
          </Form.Item>
          <Form.Item name="budget_duration" label="预算周期">
            <Select options={BUDGET_OPTIONS} />
          </Form.Item>
          <Form.Item name="expiredMode" label="过期时间" initialValue="forever">
            <Radio.Group
              options={[
                { value: 'forever', label: '永不过期' },
                { value: 'custom', label: '自定义' },
              ]}
              optionType="button"
            />
          </Form.Item>
          <Form.Item noStyle shouldUpdate={(prev, cur) => prev.expiredMode !== cur.expiredMode}>
            {({ getFieldValue }) =>
              getFieldValue('expiredMode') === 'custom' ? (
                <Form.Item name="expired_at" label="过期时刻">
                  <DatePicker showTime style={{ width: '100%' }} />
                </Form.Item>
              ) : null
            }
          </Form.Item>
          <Form.Item name="group" label="分组" extra="留空 = 归属用户分组，再退 default；分组决定 GroupRatio 倍率与分组限流">
            <Input placeholder="default" />
          </Form.Item>
          <Form.Item name="model_limits" label="模型白名单" extra="留空不限；输入模型名回车添加">
            <Select mode="tags" open={false} tokenSeparators={[',']} placeholder="如 gpt-4o-mini" />
          </Form.Item>
          <Space size="large">
            <Form.Item name="tpm" label="TPM" tooltip="每分钟最大 tokens（0/空=不限）">
              <InputNumber min={0} style={{ width: 120 }} />
            </Form.Item>
            <Form.Item name="rpm" label="RPM" tooltip="每分钟最大请求数（0/空=不限）">
              <InputNumber min={0} style={{ width: 120 }} />
            </Form.Item>
          </Space>
          <Form.Item name="tags" label="标签">
            <Select mode="tags" open={false} tokenSeparators={[',']} placeholder="如 team-a" />
          </Form.Item>
          <Form.Item name="allow_ips" label="IP 白名单" extra="逗号分隔 IP/CIDR；留空不限">
            <Input placeholder="如 10.0.0.1,192.168.1.0/24" />
          </Form.Item>
        </Form>
      </Drawer>
    </div>
  )
}

// Tooltip_quota 额度进度条单元（拆出以保持列渲染可读）。
function Tooltip_quota({ remain, quota, percent }: { remain: number; quota: number; percent: number }) {
  return (
    <div>
      <Progress percent={percent} size="small" strokeColor={percent < 20 ? '#ff4d4f' : undefined} />
      <Text type="secondary" style={{ fontSize: 12 }}>
        {quotaToDollar(remain)} / {quotaToDollar(quota)}
      </Text>
    </div>
  )
}
