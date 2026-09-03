// Logs 请求日志（M3-wave4 按角色取数）：root 走管理面 /api/log（全量 +
// 用户/渠道/模型/时间筛选）；普通用户走登录态 /api/log/mine（服务端会话
// 作用域 + logMineView 白名单——响应无 user_id/channel_id 字段，前端相应
// 隐藏用户/渠道筛选与列，普通用户不请求管理面下拉数据源）。分页表格 +
// 明细展开（detail 计费依据 JSON 美化），两种视角的公共列与交互不变。
import { useCallback, useEffect, useState } from 'react'
import {
  App,
  AutoComplete,
  Button,
  Card,
  DatePicker,
  Form,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { ReloadOutlined, SearchOutlined } from '@ant-design/icons'
import dayjs, { type Dayjs } from 'dayjs'
import { ApiError, api, getSession, type Query } from '../api/client'
import type { ChannelView, LogEntry, LogMineEntry, Paged, UserInfo } from '../api/types'
import { prettyJSON, quotaToDollar } from '../api/format'

const { Text } = Typography

// LogRow 两种视角的行联合：/api/log 条目（LogEntry，root）与 /api/log/mine
// 条目（LogMineEntry，白名单无 user_id/channel_id）；公共列 dataIndex 一致。
type LogRow = LogEntry | LogMineEntry

interface FilterForm {
  user_id?: number
  channel_id?: number
  model_name?: string
  range?: [Dayjs, Dayjs] | null
}

export default function LogsPage() {
  const { message } = App.useApp()
  // 角色决定数据端点与展示面：会话元数据由 ConsoleLayout 探针（/api/user/self）
  // 刷新过，role 以服务端为准；mount 时再读一次兜底直访场景。
  const [isAdmin, setIsAdmin] = useState(() => getSession()?.role === 100)
  const [data, setData] = useState<Paged<LogRow> | null>(null)
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [users, setUsers] = useState<UserInfo[]>([])
  const [channels, setChannels] = useState<ChannelView[]>([])
  // 模型筛选选项：从已加载的日志数据去重生成（/v1/models 为转发面 Bearer
  // 鉴权端点，管理台会话不可用，故以日志数据为选项源）。
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [form] = Form.useForm<FilterForm>()

  // filters 保存"已提交"的筛选条件（点查询才生效），与输入态分离。
  const [filters, setFilters] = useState<Partial<FilterForm>>({})

  useEffect(() => {
    const admin = getSession()?.role === 100
    setIsAdmin(admin)
    if (!admin) return // 普通用户不请求管理面数据源（/api/user、/api/channel 403）
    api
      .get<Paged<UserInfo>>('/api/user', { page: 1, page_size: 100 })
      .then((d) => setUsers(d.items))
      .catch(() => undefined)
    api
      .get<Paged<ChannelView>>('/api/channel', { page: 1, page_size: 100 })
      .then((d) => setChannels(d.items))
      .catch(() => undefined)
  }, [])

  const load = useCallback(
    async (p = page, ps = pageSize, f: Partial<FilterForm> = filters) => {
      setLoading(true)
      try {
        const range = f.range
        // 按角色取端点：root 管理面全量（user/channel 过滤可用）；普通用户
        // 登录态个人视角（服务端强制会话作用域，user/channel 过滤不传）。
        const query: Query = {
          page: p,
          page_size: ps,
          user_id: isAdmin ? f.user_id : undefined,
          channel_id: isAdmin ? f.channel_id : undefined,
          model_name: f.model_name?.trim() || undefined,
          start_timestamp: range?.[0] ? range[0].startOf('second').unix() : undefined,
          end_timestamp: range?.[1] ? range[1].startOf('second').unix() : undefined,
        }
        const d = await api.get<Paged<LogRow>>(isAdmin ? '/api/log' : '/api/log/mine', query)
        setData(d)
        // 当前页模型名去重并入筛选选项（排序保持下拉可检索）。
        setModelOptions((prev) => {
          const merged = new Set(prev)
          for (const it of d.items) {
            if (it.model_name) merged.add(it.model_name)
          }
          return Array.from(merged).sort()
        })
      } catch (err) {
        if (err instanceof ApiError && err.status !== 401) {
          message.error(err.message)
        }
      } finally {
        setLoading(false)
      }
    },
    [page, pageSize, filters, isAdmin, message],
  )

  useEffect(() => {
    void load()
  }, [load])

  const applyFilters = async () => {
    const values = await form.validateFields()
    setPage(1)
    setFilters(values)
    void load(1, pageSize, values)
  }

  const resetFilters = () => {
    form.resetFields()
    setPage(1)
    setFilters({})
    void load(1, pageSize, {})
  }

  // 列定义：root 保留管理视角的用户/渠道列（行为不变）；普通用户按
  // logMineView 白名单收窄（无用户/渠道列）。
  const columns: ColumnsType<LogRow> = [
    { title: 'ID', dataIndex: 'id', width: 76 },
    {
      title: '时间',
      dataIndex: 'created_time',
      width: 160,
      render: (v: number) => dayjs(v * 1000).format('MM-DD HH:mm:ss'),
    },
    ...(isAdmin
      ? ([
          { title: '用户', dataIndex: 'user_id', width: 90, render: (v: number) => `#${v}` },
          {
            title: '渠道',
            dataIndex: 'channel_id',
            width: 90,
            render: (v: number) => (v > 0 ? `#${v}` : '-'),
          },
        ] as ColumnsType<LogRow>)
      : []),
    { title: '令牌', dataIndex: 'token_id', width: 90, render: (v: number) => `#${v}` },
    {
      title: '协议',
      dataIndex: 'protocol',
      width: 96,
      render: (v: string) => (v ? <Tag>{v}</Tag> : '-'),
    },
    { title: '模型', dataIndex: 'model_name', width: 170, ellipsis: true },
    { title: '输入', dataIndex: 'prompt_tokens', width: 90, render: (v: number) => v.toLocaleString() },
    { title: '输出', dataIndex: 'completion_tokens', width: 90, render: (v: number) => v.toLocaleString() },
    {
      title: '消耗',
      dataIndex: 'quota',
      width: 110,
      render: (v: number) => quotaToDollar(v),
    },
    {
      title: '耗时',
      dataIndex: 'use_time',
      width: 84,
      render: (v: number) => `${v.toFixed(2)}s`,
    },
    {
      title: '流式',
      dataIndex: 'is_stream',
      width: 72,
      render: (v: boolean) => (v ? <Tag color="cyan">SSE</Tag> : <Text type="secondary">否</Text>),
    },
  ]

  return (
    <div>
      <Card style={{ marginBottom: 16 }}>
        <Form<FilterForm> form={form} layout="inline" onFinish={() => void applyFilters()}>
          {isAdmin && (
            <Form.Item name="user_id" label="用户">
              <Select
                allowClear
                showSearch
                optionFilterProp="label"
                placeholder="全部"
                style={{ width: 140 }}
                options={users.map((u) => ({ value: u.id, label: u.username }))}
              />
            </Form.Item>
          )}
          {isAdmin && (
            <Form.Item name="channel_id" label="渠道">
              <Select
                allowClear
                showSearch
                optionFilterProp="label"
                placeholder="全部"
                style={{ width: 140 }}
                options={channels.map((c) => ({ value: c.id, label: c.name }))}
              />
            </Form.Item>
          )}
          <Form.Item name="model_name" label="模型">
            <AutoComplete
              allowClear
              options={modelOptions.map((m) => ({ value: m }))}
              filterOption
              placeholder="模型名（下拉选择或输入）"
              style={{ width: 180 }}
            />
          </Form.Item>
          <Form.Item name="range" label="时间">
            <DatePicker.RangePicker showTime />
          </Form.Item>
          <Form.Item>
            <Space>
              <Button type="primary" htmlType="submit" icon={<SearchOutlined />}>
                查询
              </Button>
              <Button icon={<ReloadOutlined />} onClick={resetFilters}>
                重置
              </Button>
            </Space>
          </Form.Item>
        </Form>
      </Card>

      <Table<LogRow>
        rowKey="id"
        loading={loading}
        columns={columns}
        dataSource={data?.items ?? []}
        expandable={{
          expandedRowRender: (record) => {
            if (!record.detail) {
              return <Text type="secondary">无明细（detail 为空）</Text>
            }
            return <pre className="json-pre">{prettyJSON(record.detail)}</pre>
          },
          rowExpandable: () => true,
        }}
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
        scroll={{ x: 1250 }}
      />
    </div>
  )
}
