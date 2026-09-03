// Redemptions 兑换码管理：批量生成表单（count 1..100/面额/有效期）、分页列表
//（状态：未使用/已核销/已作废）、删除。生成响应的明文 keys 仅此一次，弹窗展示
// 并支持整批复制；核销状态机属 wave3。
import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  App,
  Button,
  Card,
  DatePicker,
  Form,
  Input,
  InputNumber,
  List,
  Popconfirm,
  Radio,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { ReloadOutlined } from '@ant-design/icons'
import type { Dayjs } from 'dayjs'
import { ApiError, api } from '../api/client'
import type { Paged, Redemption } from '../api/types'
import { QUOTA_PER_DOLLAR, REDEMPTION_STATUS, formatExpiry, formatTime, statusTag } from '../api/format'

const { Text, Paragraph } = Typography

interface GenFormValues {
  count: number
  name?: string
  quota: number
  expiredMode: 'forever' | 'custom'
  expired_at?: Dayjs | null
}

export default function RedemptionsPage() {
  const { message, modal } = App.useApp()
  const [data, setData] = useState<Paged<Redemption> | null>(null)
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const [generating, setGenerating] = useState(false)
  const [form] = Form.useForm<GenFormValues>()

  const load = useCallback(
    async (p = page, ps = pageSize) => {
      setLoading(true)
      try {
        const d = await api.get<Paged<Redemption>>('/api/redemption', { page: p, page_size: ps })
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

  const generate = async () => {
    const values = await form.validateFields()
    setGenerating(true)
    try {
      const resp = await api.post<{ items: Redemption[]; keys: string[] }>('/api/redemption', {
        count: values.count,
        name: values.name ?? '',
        quota: values.quota,
        expired_time:
          values.expiredMode === 'custom' && values.expired_at ? values.expired_at.unix() : 0,
      })
      void load()
      showKeys(resp.keys, values.quota)
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    } finally {
      setGenerating(false)
    }
  }

  // showKeys 生成成功的明文兑换码弹窗：仅此一次，支持整批复制。
  const showKeys = (keys: string[], quota: number) => {
    modal.success({
      title: `已生成 ${keys.length} 个兑换码`,
      width: 560,
      content: (
        <div>
          <Alert
            type="warning"
            showIcon
            message="明文兑换码仅显示这一次"
            description={`单枚面额 ${quotaToHint(quota)}。关闭后无法再次查看，请立即复制分发。`}
            style={{ margin: '12px 0 16px' }}
          />
          <List
            size="small"
            bordered
            dataSource={keys}
            style={{ maxHeight: 260, overflow: 'auto' }}
            renderItem={(k) => (
              <List.Item>
                <Paragraph code copyable style={{ marginBottom: 0, wordBreak: 'break-all' }}>
                  {k}
                </Paragraph>
              </List.Item>
            )}
          />
          <Button
            style={{ marginTop: 12 }}
            onClick={() => {
              void navigator.clipboard.writeText(keys.join('\n')).then(
                () => message.success('已复制全部兑换码'),
                () => message.error('复制失败，请手动选择复制'),
              )
            }}
          >
            复制全部
          </Button>
        </div>
      ),
      okText: '我已保存',
    })
  }

  const quotaToHint = (quota: number) => `${quota}（$${(quota / QUOTA_PER_DOLLAR).toFixed(4)}）`

  const remove = async (id: number) => {
    try {
      await api.del(`/api/redemption/${id}`)
      message.success('兑换码已删除')
      void load()
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    }
  }

  const columns: ColumnsType<Redemption> = [
    { title: 'ID', dataIndex: 'id', width: 72 },
    {
      title: '兑换码',
      dataIndex: 'key',
      width: 260,
      render: (v: string) => (
        <Paragraph code copyable style={{ marginBottom: 0 }}>
          {v}
        </Paragraph>
      ),
    },
    { title: '名称', dataIndex: 'name', width: 140, ellipsis: true },
    {
      title: '面额',
      dataIndex: 'quota',
      width: 130,
      render: (v: number) => `${v}（$${(v / QUOTA_PER_DOLLAR).toFixed(4)}）`,
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 96,
      render: (v: number) => {
        const tag = statusTag(v, REDEMPTION_STATUS)
        return <Tag color={tag.color}>{tag.text}</Tag>
      },
    },
    { title: '过期时间', dataIndex: 'expired_time', width: 160, render: (v: number) => formatExpiry(v) },
    { title: '核销人', dataIndex: 'used_by', width: 90, render: (v: number) => (v > 0 ? `#${v}` : '-') },
    { title: '核销时间', dataIndex: 'used_time', width: 160, render: (v: number) => formatTime(v) },
    { title: '创建时间', dataIndex: 'created_time', width: 160, render: (v: number) => formatTime(v) },
    {
      title: '操作',
      key: 'action',
      width: 90,
      render: (_, record) => (
        <Popconfirm title="删除兑换码" description="已核销记录同样可删，审计以日志为准。" onConfirm={() => void remove(record.id)}>
          <Button size="small" danger>
            删除
          </Button>
        </Popconfirm>
      ),
    },
  ]

  return (
    <div>
      <Card title="批量生成" style={{ marginBottom: 16 }}>
        <Form<GenFormValues>
          form={form}
          layout="inline"
          initialValues={{ count: 10, expiredMode: 'forever' }}
        >
          <Form.Item
            name="count"
            label="数量"
            rules={[{ required: true, message: '请输入数量' }]}
          >
            <InputNumber min={1} max={100} style={{ width: 100 }} />
          </Form.Item>
          <Form.Item name="name" label="名称备注">
            <Input placeholder="如：618 活动" style={{ width: 160 }} />
          </Form.Item>
          <Form.Item
            name="quota"
            label="单枚面额（quota）"
            rules={[{ required: true, message: '请输入面额' }]}
          >
            <InputNumber min={1} style={{ width: 140 }} />
          </Form.Item>
          <Form.Item name="expiredMode" label="有效期">
            <Radio.Group
              options={[
                { value: 'forever', label: '永久' },
                { value: 'custom', label: '截止' },
              ]}
              optionType="button"
            />
          </Form.Item>
          <Form.Item noStyle shouldUpdate={(prev, cur) => prev.expiredMode !== cur.expiredMode}>
            {({ getFieldValue }) =>
              getFieldValue('expiredMode') === 'custom' ? (
                <Form.Item name="expired_at" rules={[{ required: true, message: '请选择截止时间' }]}>
                  <DatePicker showTime placeholder="过期时刻" />
                </Form.Item>
              ) : null
            }
          </Form.Item>
          <Form.Item>
            <Button type="primary" loading={generating} onClick={() => void generate()}>
              生成
            </Button>
          </Form.Item>
        </Form>
        <Text type="secondary" style={{ fontSize: 12 }}>
          数量 1..100；核销功能（兑换 → 令牌加额）将在后续波次交付。
        </Text>
      </Card>

      <div className="page-toolbar">
        <span />
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => void load()}>
            刷新
          </Button>
        </Space>
      </div>
      <Table<Redemption>
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
        scroll={{ x: 1350 }}
      />
    </div>
  )
}
