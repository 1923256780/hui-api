// Logs 请求日志：分页表格（时间/用户/渠道/模型/tokens/quota/耗时）、筛选
//（用户/渠道下拉 + 模型名 + 时间区间）、明细展开（detail 计费依据 JSON 美化）。
// channel_id 过滤待 hook 回填后生效（wave1 仅链路就绪）。
import { useCallback, useEffect, useState } from 'react'
import {
  App,
  Button,
  Card,
  DatePicker,
  Form,
  Input,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { ReloadOutlined, SearchOutlined } from '@ant-design/icons'
import dayjs, { type Dayjs } from 'dayjs'
import { ApiError, api } from '../api/client'
import type { ChannelView, LogEntry, Paged, UserInfo } from '../api/types'
import { prettyJSON, quotaToDollar } from '../api/format'

const { Text } = Typography

interface FilterForm {
  user_id?: number
  channel_id?: number
  model_name?: string
  range?: [Dayjs, Dayjs] | null
}

export default function LogsPage() {
  const { message } = App.useApp()
  const [data, setData] = useState<Paged<LogEntry> | null>(null)
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [users, setUsers] = useState<UserInfo[]>([])
  const [channels, setChannels] = useState<ChannelView[]>([])
  const [form] = Form.useForm<FilterForm>()

  // filters 保存"已提交"的筛选条件（点查询才生效），与输入态分离。
  const [filters, setFilters] = useState<Partial<FilterForm>>({})

  useEffect(() => {
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
        const d = await api.get<Paged<LogEntry>>('/api/log', {
          page: p,
          page_size: ps,
          user_id: f.user_id,
          channel_id: f.channel_id,
          model_name: f.model_name?.trim() || undefined,
          start_timestamp: range?.[0] ? range[0].startOf('second').unix() : undefined,
          end_timestamp: range?.[1] ? range[1].startOf('second').unix() : undefined,
        })
        setData(d)
      } catch (err) {
        if (err instanceof ApiError && err.status !== 401) {
          message.error(err.message)
        }
      } finally {
        setLoading(false)
      }
    },
    [page, pageSize, filters, message],
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

  const columns: ColumnsType<LogEntry> = [
    { title: 'ID', dataIndex: 'id', width: 76 },
    {
      title: '时间',
      dataIndex: 'created_time',
      width: 160,
      render: (v: number) => dayjs(v * 1000).format('MM-DD HH:mm:ss'),
    },
    { title: '用户', dataIndex: 'user_id', width: 90, render: (v: number) => `#${v}` },
    { title: '令牌', dataIndex: 'token_id', width: 90, render: (v: number) => `#${v}` },
    { title: '渠道', dataIndex: 'channel_id', width: 90, render: (v: number) => (v > 0 ? `#${v}` : '-') },
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
          <Form.Item name="model_name" label="模型">
            <Input allowClear placeholder="精确模型名" style={{ width: 170 }} />
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

      <Table<LogEntry>
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
