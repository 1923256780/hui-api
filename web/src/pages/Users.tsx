// Users 用户管理：分页列表（角色/分组/余额）、新建/编辑抽屉（改密非空即重置
// 并失效旧会话）、删除确认（管理员禁删）。编辑当前登录账号时禁改角色与状态
//（与后端 self_lockout 防自锁语义对齐）。
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
import { FileAddOutlined, ReloadOutlined } from '@ant-design/icons'
import { ApiError, api, getSession } from '../api/client'
import type { Paged, UserInfo } from '../api/types'
import { COMMON_STATUS, formatTime, quotaToDollar, statusTag } from '../api/format'

const { Text } = Typography

interface UserFormValues {
  username: string
  password?: string
  display_name?: string
  role: number
  status: boolean
  quota?: number
  group?: string
  email?: string
}

export default function UsersPage() {
  const { message } = App.useApp()
  const [data, setData] = useState<Paged<UserInfo> | null>(null)
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const [drawerOpen, setDrawerOpen] = useState(false)
  const [editing, setEditing] = useState<UserInfo | null>(null)
  const [saving, setSaving] = useState(false)
  const [form] = Form.useForm<UserFormValues>()

  const me = getSession()
  const selfId = me?.id ?? 0

  const load = useCallback(
    async (p = page, ps = pageSize) => {
      setLoading(true)
      try {
        const d = await api.get<Paged<UserInfo>>('/api/user', { page: p, page_size: ps })
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
    form.setFieldsValue({ role: 1, status: true, quota: 0, group: 'default' })
    setDrawerOpen(true)
  }

  const openEdit = (u: UserInfo) => {
    setEditing(u)
    form.resetFields()
    form.setFieldsValue({
      username: u.username,
      password: '',
      display_name: u.display_name,
      role: u.role,
      status: u.status !== 2,
      quota: u.quota,
      group: u.group,
      email: u.email,
    })
    setDrawerOpen(true)
  }

  const submit = async () => {
    const values = await form.validateFields()
    setSaving(true)
    try {
      const payload = {
        username: values.username.trim(),
        password: values.password ?? '',
        display_name: values.display_name ?? '',
        // 编辑自己时角色/状态不渲染表单项，显式回传当前值避免后端 self_lockout 误拦。
        role: editing && editing.id === selfId ? editing.role : values.role,
        status:
          editing && editing.id === selfId
            ? editing.status
            : values.status
              ? 1
              : 2,
        quota: values.quota ?? 0,
        group: (values.group ?? '').trim() || 'default',
        email: values.email ?? '',
      }
      if (editing) {
        await api.put(`/api/user/${editing.id}`, payload)
        message.success(
          payload.password ? '用户已更新（改密后其旧会话已全部失效）' : '用户已更新',
        )
      } else {
        await api.post('/api/user', payload)
        message.success('用户已创建')
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

  const remove = async (u: UserInfo) => {
    try {
      await api.del(`/api/user/${u.id}`)
      message.success(`用户「${u.username}」已删除（其令牌已级联删除）`)
      void load()
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    }
  }

  const columns: ColumnsType<UserInfo> = [
    { title: 'ID', dataIndex: 'id', width: 64 },
    { title: '用户名', dataIndex: 'username', width: 140 },
    { title: '显示名', dataIndex: 'display_name', width: 130, ellipsis: true },
    {
      title: '角色',
      dataIndex: 'role',
      width: 96,
      render: (v: number) =>
        v === 100 ? <Tag color="red">管理员</Tag> : <Tag>普通用户</Tag>,
    },
    {
      title: '分组',
      dataIndex: 'group',
      width: 96,
      render: (v: string) => <Tag color="blue">{v || 'default'}</Tag>,
    },
    {
      title: '余额 / 已用',
      dataIndex: 'quota',
      width: 170,
      render: (_, record) => (
        <Text>
          {quotaToDollar(record.quota)}
          <Text type="secondary"> / {quotaToDollar(record.used_quota)}</Text>
        </Text>
      ),
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
    { title: '创建时间', dataIndex: 'created_time', width: 160, render: (v: number) => formatTime(v) },
    { title: '最后登录', dataIndex: 'last_login_time', width: 160, render: (v: number) => formatTime(v) },
    {
      title: '操作',
      key: 'action',
      width: 140,
      render: (_, record) => {
        const isSelf = record.id === selfId
        return (
          <Space size="small">
            <Button size="small" onClick={() => openEdit(record)}>
              编辑
            </Button>
            {record.role === 100 ? (
              <Tooltip title="不允许删除管理员账号">
                <Button size="small" danger disabled>
                  删除
                </Button>
              </Tooltip>
            ) : (
              <Popconfirm
                title="删除用户"
                description={`确认删除「${record.username}」？其全部令牌将级联删除。`}
                onConfirm={() => void remove(record)}
              >
                <Button size="small" danger disabled={isSelf}>
                  删除
                </Button>
              </Popconfirm>
            )}
          </Space>
        )
      },
    },
  ]

  return (
    <div>
      <div className="page-toolbar">
        <Button icon={<ReloadOutlined />} onClick={() => void load()}>
          刷新
        </Button>
        <Button type="primary" icon={<FileAddOutlined />} onClick={openCreate}>
          新建用户
        </Button>
      </div>
      <Table<UserInfo>
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
        scroll={{ x: 1250 }}
      />

      <Drawer
        title={editing ? `编辑用户 #${editing.id}` : '新建用户'}
        width={520}
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
        <Form<UserFormValues> form={form} layout="vertical">
          <Form.Item
            name="username"
            label="用户名"
            rules={[{ required: true, message: '请输入用户名' }]}
          >
            <Input autoComplete="off" />
          </Form.Item>
          <Form.Item
            name="password"
            label="密码"
            required={!editing}
            extra={
              editing
                ? '留空 = 不修改密码；填写新密码后该用户全部旧会话立即失效'
                : '登录口令（bcrypt 存储）'
            }
            rules={editing ? [] : [{ required: true, message: '请输入密码' }]}
          >
            <Input.Password
              placeholder={editing ? '留空保持不变' : '设置初始密码'}
              autoComplete="new-password"
            />
          </Form.Item>
          <Form.Item name="display_name" label="显示名">
            <Input />
          </Form.Item>
          {editing && editing.id === selfId ? (
            <Form.Item label="角色 / 状态" extra="不允许修改自己的角色或状态（防自锁）">
              <Space>
                <Tag color="red">管理员</Tag>
                <Tag color="green">{editing.status !== 2 ? '启用' : '禁用'}</Tag>
              </Space>
            </Form.Item>
          ) : (
            <>
              <Form.Item name="role" label="角色">
                <Select
                  options={[
                    { value: 1, label: '普通用户' },
                    { value: 100, label: '管理员（root，可访问管理面）' },
                  ]}
                />
              </Form.Item>
              <Form.Item name="status" label="状态" valuePropName="checked">
                <Switch checkedChildren="启用" unCheckedChildren="禁用" />
              </Form.Item>
            </>
          )}
          <Form.Item name="quota" label="余额（quota）" tooltip="500000 quota = $1">
            <InputNumber min={0} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="group" label="默认分组" extra="该用户新建令牌的缺省归属组">
            <Input placeholder="default" />
          </Form.Item>
          <Form.Item name="email" label="邮箱">
            <Input autoComplete="off" />
          </Form.Item>
        </Form>
      </Drawer>
    </div>
  )
}
