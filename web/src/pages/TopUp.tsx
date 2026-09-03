// TopUp 充值页（M2-wave3 基础 + M3-wave3 在线充值，契约见 docs/05 §5.10）：
// 登录用户自服务——
//   1. 余额展示（GET /api/user/self，quota 与美元换算同 QUOTA_PER_DOLLAR）；
//   2. 兑换码核销（POST /api/user/topup：事务原子核销 → 面值入账用户余额）；
//   3. 额度划转（POST /api/token/:id/assign：user.quota → token.remain_quota）；
//   4. 在线充值（M3-wave3）：/api/setup 的 topup 网关开关才渲染区块，
//      POST /api/user/topup/order 下单后整页跳转 pay_url；支付网关回跳
//      /console/topup?order=<order_no> 时从订单列表回查该单展示支付状态；
//      订单列表走本人作用域端点 GET /api/user/topup/orders（分页）。
// 名下令牌列表走所有权作用域端点 GET /api/token/mine（登录态即可；管理列表
// /api/token 为 root 专属，普通用户会 403——M2 浏览器验收缺陷修复）。
import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  App,
  Button,
  Card,
  Col,
  Input,
  InputNumber,
  Radio,
  Row,
  Select,
  Space,
  Statistic,
  Table,
  Tag,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { useSearchParams } from 'react-router-dom'
import { ReloadOutlined } from '@ant-design/icons'
import { ApiError, api } from '../api/client'
import type {
  OrderCreateResp,
  Paged,
  SetupData,
  Token,
  TopupOrderView,
  UserInfo,
} from '../api/types'
import { formatTime, QUOTA_PER_DOLLAR, statusTag, TOPUP_ORDER_STATUS } from '../api/format'

const { Text } = Typography

// currencySymbol 金额货币符号映射（CNY/USD 常用，其余显示 ISO 码）。
function currencySymbol(c: string): string {
  if (c === 'CNY') return '¥'
  if (c === 'USD') return '$'
  return ''
}

const dollarText = (quota: number) => `$${(quota / QUOTA_PER_DOLLAR).toFixed(4)}`

// 订单列表列定义（M3-wave3）：金额分→元展示，状态映射 TOPUP_ORDER_STATUS，
// 未支付订单 paid_time=0 显示 '-'（formatTime 兜底）。
const orderColumns: ColumnsType<TopupOrderView> = [
  {
    title: '订单号',
    dataIndex: 'order_no',
    width: 230,
    render: (v: string) => <Text code>{v}</Text>,
  },
  { title: '网关', dataIndex: 'gateway', width: 80 },
  {
    title: '金额',
    dataIndex: 'amount_cents',
    width: 110,
    render: (v: number, row) => {
      const s = currencySymbol(row.currency)
      return s ? `${s}${(v / 100).toFixed(2)}` : `${(v / 100).toFixed(2)} ${row.currency}`
    },
  },
  {
    title: '到账额度',
    dataIndex: 'quota',
    width: 150,
    render: (v: number) => `${v}（${dollarText(v)}）`,
  },
  {
    title: '状态',
    dataIndex: 'status',
    width: 90,
    render: (v: number) => {
      const t = statusTag(v, TOPUP_ORDER_STATUS)
      return <Tag color={t.color}>{t.text}</Tag>
    },
  },
  { title: '创建时间', dataIndex: 'created_time', width: 165, render: (v: number) => formatTime(v) },
  { title: '支付时间', dataIndex: 'paid_time', width: 165, render: (v: number) => formatTime(v) },
]

export default function TopUpPage() {
  const { message } = App.useApp()
  const [searchParams] = useSearchParams()
  const [self, setSelf] = useState<UserInfo | null>(null)
  const [setup, setSetup] = useState<SetupData | null>(null)
  const [tokens, setTokens] = useState<Token[]>([])
  // 在线充值状态（M3-wave3）：网关选择、金额（元）、下单中、订单列表与回跳查单。
  // returnedOrder：undefined=无回跳参数；null=有参数但未在最近订单中找到；
  // 其余为回跳单详情（按 status 展示支付结果）。
  const [gateway, setGateway] = useState<'epay' | 'stripe'>('epay')
  const [amountYuan, setAmountYuan] = useState<number | null>(null)
  const [paying, setPaying] = useState(false)
  const [orders, setOrders] = useState<TopupOrderView[]>([])
  const [ordersTotal, setOrdersTotal] = useState(0)
  const [ordersPage, setOrdersPage] = useState(1)
  const [ordersLoading, setOrdersLoading] = useState(false)
  const [returnedOrder, setReturnedOrder] = useState<TopupOrderView | null | undefined>(
    undefined,
  )
  const [redeemKey, setRedeemKey] = useState('')
  const [redeeming, setRedeeming] = useState(false)
  const [tokenID, setTokenID] = useState<number | undefined>(undefined)
  const [assignQuota, setAssignQuota] = useState<number | null>(null)
  const [assigning, setAssigning] = useState(false)

  const load = useCallback(async () => {
    try {
      const u = await api.get<UserInfo>('/api/user/self')
      setSelf(u)
      // 能力发现：topup 网关开关决定在线充值区块是否渲染（/api/setup 公开端点）。
      const s = await api.get<SetupData>('/api/setup')
      setSetup(s)
      // 名下令牌：所有权作用域端点（登录态；user_id 由服务端强制取会话用户）。
      // 响应为白名单字段视图（无 tpm_rpm/tags/allow_ips 等管理字段），
      // Token 类型中缺失字段不影响本页使用的 id/name/remain_quota。
      const d = await api.get<Paged<Token>>('/api/token/mine', {
        page: 1,
        page_size: 100,
      })
      setTokens(d.items)
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    }
  }, [message])

  useEffect(() => {
    void load()
  }, [load])

  // loadTopup 订单列表（本人作用域分页）；回跳场景（URL ?order=）在首页
  // 结果中回查该单展示支付状态——order_no 全局唯一且回跳单必为本人最新
  // 订单之一，首页即可命中；未找到时提示稍后刷新。
  const loadTopup = useCallback(
    async (page: number) => {
      setOrdersLoading(true)
      try {
        const d = await api.get<Paged<TopupOrderView>>('/api/user/topup/orders', {
          page,
          page_size: 10,
        })
        setOrders(d.items)
        setOrdersTotal(d.total)
        setOrdersPage(page)
        const no = searchParams.get('order')
        if (no) setReturnedOrder(d.items.find((o) => o.order_no === no) ?? null)
      } catch (err) {
        if (err instanceof ApiError && err.status !== 401) {
          message.error(err.message)
        }
      } finally {
        setOrdersLoading(false)
      }
    },
    [message, searchParams],
  )

  useEffect(() => {
    void loadTopup(1)
  }, [loadTopup])

  // setup 到位后校正网关默认值：默认 'epay' 未启用而 stripe 启用时切换。
  useEffect(() => {
    if (!setup?.topup) return
    if (gateway === 'epay' && !setup.topup.epay && setup.topup.stripe) setGateway('stripe')
  }, [setup, gateway])

  const dollar = (quota: number) => `$${(quota / QUOTA_PER_DOLLAR).toFixed(4)}`

  // payTopup 在线下单：金额元 → 分（整数化），后端校验网关开关与 min/max
  // 金额并按汇率快照换算额度、落 pending 订单后返回跳转 URL（epay 网关
  // 提交地址 / Stripe Checkout Session URL）；整页跳转交由支付网关，支付
  // 结果经 notify/webhook 回写订单状态后自动入账，跳转失败时恢复按钮。
  const payTopup = async () => {
    if (!amountYuan || amountYuan <= 0) {
      message.warning('请输入充值金额')
      return
    }
    setPaying(true)
    try {
      const r = await api.post<OrderCreateResp>('/api/user/topup/order', {
        gateway,
        amount_cents: Math.round(amountYuan * 100),
      })
      message.info(`订单 ${r.order_no} 已创建，正在跳转支付页面…`)
      window.location.assign(r.pay_url)
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
      setPaying(false)
    }
  }

  // redeem 兑换码核销：成功后刷新余额与令牌余额（面额已入账，尚未分配）。
  const redeem = async () => {
    const key = redeemKey.trim()
    if (!key) {
      message.warning('请输入兑换码')
      return
    }
    setRedeeming(true)
    try {
      const r = await api.post<{ quota_added: number; user_quota: number }>('/api/user/topup', {
        key,
      })
      message.success(
        `兑换成功：+${r.quota_added}（${dollar(r.quota_added)}），当前余额 ${r.user_quota}（${dollar(r.user_quota)}）`,
      )
      setRedeemKey('')
      void load()
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    } finally {
      setRedeeming(false)
    }
  }

  // assign 额度划转：用户余额 → 令牌余额（不足/不限额由后端拒绝并提示）。
  const assign = async () => {
    if (!tokenID) {
      message.warning('请选择目标令牌')
      return
    }
    if (!assignQuota || assignQuota <= 0) {
      message.warning('请输入划转额度')
      return
    }
    setAssigning(true)
    try {
      const r = await api.post<{ quota_assigned: number; remain_quota: number }>(
        `/api/token/${tokenID}/assign`,
        { quota: assignQuota },
      )
      message.success(
        `已划转 ${r.quota_assigned}（${dollar(r.quota_assigned)}），令牌余额 ${r.remain_quota}（${dollar(r.remain_quota)}）`,
      )
      setAssignQuota(null)
      void load()
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    } finally {
      setAssigning(false)
    }
  }

  // returnAlert 回跳支付结果提示：undefined=无回跳参数不渲染；
  // null=订单号未命中最近订单（可能尚未同步）；其余按订单状态分四级展示。
  const returnAlert = (() => {
    if (returnedOrder === undefined) return null
    if (returnedOrder === null) {
      const no = searchParams.get('order')
      return {
        type: 'info' as const,
        message: `未在最近订单中找到订单 ${no ?? ''}，可能尚未同步，请稍后刷新订单列表查看。`,
      }
    }
    const { status, order_no } = returnedOrder
    if (status === 2)
      return { type: 'success' as const, message: `订单 ${order_no} 已支付，额度已入账。` }
    if (status === 1)
      return {
        type: 'warning' as const,
        message: `订单 ${order_no} 待支付；若已完成付款，网关回调确认后额度将自动入账。`,
      }
    return {
      type: 'error' as const,
      message: `订单 ${order_no} 当前状态「${statusTag(status, TOPUP_ORDER_STATUS).text}」，额度未入账。`,
    }
  })()

  return (
    <div>
      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col span={8}>
          <Card>
            <Statistic
              title="当前余额"
              value={self?.quota ?? 0}
              suffix={<Text type="secondary" style={{ fontSize: 14 }}>{self ? dollar(self.quota) : '-'}</Text>}
            />
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic
              title="累计已用"
              value={self?.used_quota ?? 0}
              suffix={<Text type="secondary" style={{ fontSize: 14 }}>{self ? dollar(self.used_quota) : '-'}</Text>}
            />
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic
              title="名下令牌"
              value={tokens.length}
              suffix={
                <Button size="small" icon={<ReloadOutlined />} onClick={() => void load()}>
                  刷新
                </Button>
              }
            />
          </Card>
        </Col>
      </Row>

      {returnAlert && (
        <Alert
          style={{ marginBottom: 16 }}
          showIcon
          type={returnAlert.type}
          message={returnAlert.message}
        />
      )}

      {setup?.topup && (setup.topup.epay || setup.topup.stripe) && (
        <Card title="在线充值" style={{ marginBottom: 16 }}>
          <Space direction="vertical" style={{ width: '100%' }} size={12}>
            <Radio.Group
              value={gateway}
              onChange={(e) => setGateway(e.target.value as 'epay' | 'stripe')}
              disabled={paying}
            >
              {setup.topup.epay && <Radio.Button value="epay">易支付（EPay）</Radio.Button>}
              {setup.topup.stripe && <Radio.Button value="stripe">Stripe</Radio.Button>}
            </Radio.Group>
            <Space.Compact style={{ width: '100%', maxWidth: 480 }}>
              <InputNumber
                style={{ width: '100%' }}
                min={0.01}
                precision={2}
                placeholder="充值金额（元）"
                value={amountYuan}
                onChange={(v) => setAmountYuan(v)}
                onPressEnter={() => void payTopup()}
              />
              <Button type="primary" loading={paying} onClick={() => void payTopup()}>
                去支付
              </Button>
            </Space.Compact>
            <Alert
              type="info"
              showIcon
              message="下单后跳转支付页面完成付款，额度在网关回调确认后自动入账；未支付订单不会扣款。"
            />
          </Space>
        </Card>
      )}

      <Row gutter={16}>
        <Col span={12}>
          <Card title="兑换码充值" style={{ marginBottom: 16 }}>
            <Space.Compact style={{ width: '100%' }}>
              <Input
                placeholder="粘贴兑换码明文"
                value={redeemKey}
                onChange={(e) => setRedeemKey(e.target.value)}
                onPressEnter={() => void redeem()}
              />
              <Button type="primary" loading={redeeming} onClick={() => void redeem()}>
                兑换
              </Button>
            </Space.Compact>
            <Alert
              type="info"
              showIcon
              style={{ marginTop: 12 }}
              message="核销成功后面额立即入账当前用户余额（同一兑换码仅可核销一次；过期码将被拒绝并标记）。"
            />
          </Card>
        </Col>
        <Col span={12}>
          <Card title="额度划转到令牌" style={{ marginBottom: 16 }}>
            <Space direction="vertical" style={{ width: '100%' }} size={12}>
              <Select<number>
                placeholder="选择名下令牌"
                style={{ width: '100%' }}
                value={tokenID}
                onChange={setTokenID}
                options={tokens.map((t) => ({
                  value: t.id,
                  label: t.unlimited_quota
                    ? `#${t.id} ${t.name}（不限额，无需划转）`
                    : `#${t.id} ${t.name}（余额 ${t.remain_quota} / ${dollar(t.remain_quota)}）`,
                }))}
              />
              <Space.Compact style={{ width: '100%' }}>
                <InputNumber
                  min={1}
                  style={{ width: '100%' }}
                  placeholder="划转额度（quota）"
                  value={assignQuota}
                  onChange={(v) => setAssignQuota(v)}
                />
                <Button type="primary" loading={assigning} onClick={() => void assign()}>
                  划转
                </Button>
              </Space.Compact>
              <Alert
                type="info"
                showIcon
                message="划转为「用户余额 → 令牌余额」的转移事务，余额不足或目标为不限额令牌时将被拒绝。"
              />
            </Space>
          </Card>
        </Col>
      </Row>

      <Card
        title="充值订单"
        extra={
          <Button size="small" icon={<ReloadOutlined />} onClick={() => void loadTopup(ordersPage)}>
            刷新
          </Button>
        }
      >
        <Table<TopupOrderView>
          rowKey="order_no"
          size="small"
          loading={ordersLoading}
          dataSource={orders}
          columns={orderColumns}
          pagination={{
            current: ordersPage,
            pageSize: 10,
            total: ordersTotal,
            onChange: (p) => void loadTopup(p),
            showSizeChanger: false,
          }}
        />
      </Card>
    </div>
  )
}
