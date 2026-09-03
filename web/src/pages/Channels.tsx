// Channels 渠道管理：分页列表、连通测试（test/:id，结果展示 status_code 与
// time_ms）、新建/编辑抽屉（整对象幂等写）、删除确认。
// 编辑回显的 key 为脱敏值，提交留空 = 保留旧密钥（docs/05 幂等写语义）。
import { useCallback, useEffect, useState } from 'react'
import {
  App,
  Button,
  Drawer,
  Form,
  Input,
  InputNumber,
  Popconfirm,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  FileAddOutlined,
  PlayCircleOutlined,
  ReloadOutlined,
} from '@ant-design/icons'
import { ApiError, api } from '../api/client'
import type { ChannelTestResult, ChannelView, Paged } from '../api/types'
import { COMMON_STATUS, formatTime, statusTag } from '../api/format'

const { Text } = Typography

const TYPE_OPTIONS = [
  { value: 1, label: 'OpenAI 兼容' },
  { value: 2, label: 'Anthropic' },
]

function typeTag(type: number) {
  const item = TYPE_OPTIONS.find((t) => t.value === type)
  return <Tag color={type === 2 ? 'purple' : 'geekblue'}>{item?.label ?? `类型${type}`}</Tag>
}

interface ChannelFormValues {
  name: string
  type: number
  base_url: string
  key?: string
  models?: string[]
  priority?: number
  weight?: number
  status: boolean
  param_override?: string
}

export default function ChannelsPage() {
  const { message } = App.useApp()
  const [data, setData] = useState<Paged<ChannelView> | null>(null)
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const [drawerOpen, setDrawerOpen] = useState(false)
  const [editing, setEditing] = useState<ChannelView | null>(null)
  const [saving, setSaving] = useState(false)
  const [form] = Form.useForm<ChannelFormValues>()

  const [testingId, setTestingId] = useState<number | null>(null)
  const [testResults, setTestResults] = useState<Record<number, ChannelTestResult>>({})

  const load = useCallback(
    async (p = page, ps = pageSize) => {
      setLoading(true)
      try {
        const d = await api.get<Paged<ChannelView>>('/api/channel', { page: p, page_size: ps })
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
  }, [load])

  const openCreate = () => {
    setEditing(null)
    form.resetFields()
    form.setFieldsValue({ type: 1, status: true, models: [], priority: 0, weight: 0 })
    setDrawerOpen(true)
  }

  const openEdit = (ch: ChannelView) => {
    setEditing(ch)
    form.resetFields()
    form.setFieldsValue({
      name: ch.name,
      type: ch.type,
      base_url: ch.base_url,
      key: '',
      models: ch.models ? ch.models.split(',').filter(Boolean) : [],
      priority: ch.priority,
      weight: ch.weight,
      status: ch.status !== 2,
      param_override: ch.param_override,
    })
    setDrawerOpen(true)
  }

  const submit = async () => {
    const values = await form.validateFields()
    setSaving(true)
    try {
      const payload = {
        name: values.name.trim(),
        type: values.type,
        base_url: values.base_url.trim(),
        key: values.key ?? '',
        models: (values.models ?? []).join(','),
        priority: values.priority ?? 0,
        weight: values.weight ?? 0,
        status: values.status ? 1 : 2,
        param_override: values.param_override ?? '',
      }
      if (editing) {
        await api.put(`/api/channel/${editing.id}`, payload)
        message.success('渠道已更新（熔断状态已复位）')
      } else {
        await api.post('/api/channel', payload)
        message.success('渠道已创建')
      }
      setDrawerOpen(false)
      void load()
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

  const remove = async (id: number) => {
    try {
      await api.del(`/api/channel/${id}`)
      message.success('渠道已删除')
      void load()
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    }
  }

  const runTest = async (id: number) => {
    setTestingId(id)
    try {
      const r = await api.post<ChannelTestResult>(`/api/channel/test/${id}`)
      setTestResults((prev) => ({ ...prev, [id]: r }))
      if (r.success) {
        message.success(`测试通过：HTTP ${r.status_code}，耗时 ${r.time_ms}ms`)
      } else {
        message.warning(`测试失败：${r.message || `HTTP ${r.status_code}`}`)
      }
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    } finally {
      setTestingId(null)
    }
  }

  const columns: ColumnsType<ChannelView> = [
    { title: 'ID', dataIndex: 'id', width: 64 },
    { title: '名称', dataIndex: 'name', width: 160, ellipsis: true },
    { title: '类型', dataIndex: 'type', width: 120, render: (v: number) => typeTag(v) },
    { title: 'Base URL', dataIndex: 'base_url', ellipsis: true },
    {
      title: '模型',
      dataIndex: 'models',
      width: 160,
      render: (v: string) => {
        if (!v) return <Text type="secondary">-</Text>
        const list = v.split(',').filter(Boolean)
        return (
          <Tooltip title={v} placement="topLeft">
            <span>
              <Tag>{list.length === 1 ? list[0] : `${list.length} 个模型`}</Tag>
            </span>
          </Tooltip>
        )
      },
    },
    { title: '优先级', dataIndex: 'priority', width: 80 },
    { title: '权重', dataIndex: 'weight', width: 72 },
    {
      title: '状态',
      dataIndex: 'status',
      width: 88,
      render: (v: number) => {
        const tag = statusTag(v, COMMON_STATUS)
        return <Tag color={tag.color}>{tag.text}</Tag>
      },
    },
    {
      title: '最近测试',
      key: 'test',
      width: 150,
      render: (_, record) => {
        const r = testResults[record.id]
        if (!r) return <Text type="secondary">未测试</Text>
        return r.success ? (
          <Tooltip title={`HTTP ${r.status_code}`}>
            <Tag color="green">{r.time_ms}ms</Tag>
          </Tooltip>
        ) : (
          <Tooltip title={r.message || `HTTP ${r.status_code}`}>
            <Tag color="red">失败</Tag>
          </Tooltip>
        )
      },
    },
    { title: '更新时间', dataIndex: 'updated_time', width: 160, render: (v: number) => formatTime(v) },
    {
      title: '操作',
      key: 'action',
      width: 210,
      render: (_, record) => (
        <Space size="small">
          <Button
            size="small"
            icon={<PlayCircleOutlined />}
            loading={testingId === record.id}
            onClick={() => void runTest(record.id)}
          >
            测试
          </Button>
          <Button size="small" onClick={() => openEdit(record)}>
            编辑
          </Button>
          <Popconfirm
            title="删除渠道"
            description={`确认删除「${record.name}」？删除后写操作会复位其熔断状态。`}
            onConfirm={() => void remove(record.id)}
          >
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
          新建渠道
        </Button>
      </div>
      <Table<ChannelView>
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
        scroll={{ x: 1200 }}
      />

      <Drawer
        title={editing ? `编辑渠道 #${editing.id}` : '新建渠道'}
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
        <Form<ChannelFormValues> form={form} layout="vertical">
          <Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入渠道名称' }]}>
            <Input placeholder="如：官方直连" />
          </Form.Item>
          <Form.Item name="type" label="协议类型" rules={[{ required: true }]}>
            <Select options={TYPE_OPTIONS} />
          </Form.Item>
          <Form.Item
            name="base_url"
            label="Base URL"
            rules={[{ required: true, message: '请输入上游地址' }]}
          >
            <Input placeholder="如：https://api.example.com（含 /v1 由转发层自动拼接）" />
          </Form.Item>
          <Form.Item
            name="key"
            label="上游密钥"
            extra={editing ? '留空 = 保留旧密钥（当前回显为脱敏值）' : '用于转发鉴权与连通测试'}
          >
            <Input.Password
              placeholder={editing ? '已脱敏，留空保留旧值' : '上游 API Key'}
              autoComplete="new-password"
            />
          </Form.Item>
          <Form.Item
            name="models"
            label="模型清单"
            extra="输入模型名后回车；多个模型逗号分隔存储。通配渠道请输入 *"
          >
            <Select
              mode="tags"
              open={false}
              tokenSeparators={[',']}
              placeholder="如 gpt-4o-mini、claude-sonnet-4"
            />
          </Form.Item>
          <Space size="large">
            <Form.Item name="priority" label="优先级" tooltip="数值越大越优先被调度">
              <InputNumber min={0} style={{ width: 120 }} />
            </Form.Item>
            <Form.Item name="weight" label="权重" tooltip="同级渠道按权重随机分流">
              <InputNumber min={0} style={{ width: 120 }} />
            </Form.Item>
          </Space>
          <Form.Item name="status" label="状态" valuePropName="checked">
            <Switch checkedChildren="启用" unCheckedChildren="禁用" />
          </Form.Item>
          <Form.Item
            name="param_override"
            label="参数改写（param_override）"
            extra={'渠道级请求参数改写 JSON，如 {"set":{"temperature":0.5}}；留空不启用'}
            rules={[
              {
                validator: (_, value: string) => {
                  if (!value || !value.trim()) return Promise.resolve()
                  try {
                    const parsed = JSON.parse(value)
                    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
                      return Promise.reject(new Error('必须为 JSON 对象'))
                    }
                    return Promise.resolve()
                  } catch {
                    return Promise.reject(new Error('JSON 格式不合法'))
                  }
                },
              },
            ]}
          >
            <Input.TextArea rows={4} placeholder='{"set":{"temperature":0.5},"delete":{"top_p":null}}' />
          </Form.Item>
        </Form>
      </Drawer>
    </div>
  )
}
