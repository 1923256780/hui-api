// TopUp 充值页（M2-wave3，契约见 docs/05）：登录用户自服务——
//   1. 余额展示（GET /api/user/self，quota 与美元换算同 QUOTA_PER_DOLLAR）；
//   2. 兑换码核销（POST /api/user/topup：事务原子核销 → 面值入账用户余额）；
//   3. 额度划转（POST /api/token/:id/assign：user.quota → token.remain_quota）。
// 控制台为管理面会话：划转下拉列出当前登录用户名下令牌（GET /api/token?user_id=）；
// 面向普通用户的独立门户属 M3 范畴。
import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  App,
  Button,
  Card,
  Col,
  Input,
  InputNumber,
  Row,
  Select,
  Space,
  Statistic,
  Typography,
} from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import { ApiError, api } from '../api/client'
import type { Paged, Token, UserInfo } from '../api/types'
import { QUOTA_PER_DOLLAR } from '../api/format'

const { Text } = Typography

export default function TopUpPage() {
  const { message } = App.useApp()
  const [self, setSelf] = useState<UserInfo | null>(null)
  const [tokens, setTokens] = useState<Token[]>([])
  const [redeemKey, setRedeemKey] = useState('')
  const [redeeming, setRedeeming] = useState(false)
  const [tokenID, setTokenID] = useState<number | undefined>(undefined)
  const [assignQuota, setAssignQuota] = useState<number | null>(null)
  const [assigning, setAssigning] = useState(false)

  const load = useCallback(async () => {
    try {
      const u = await api.get<UserInfo>('/api/user/self')
      setSelf(u)
      const d = await api.get<Paged<Token>>('/api/token', {
        user_id: u.id,
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

  const dollar = (quota: number) => `$${(quota / QUOTA_PER_DOLLAR).toFixed(4)}`

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
    </div>
  )
}
