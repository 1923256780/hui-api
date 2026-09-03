// Invite 邀请返利页（M3-wave3，docs/05 §5.10）：登录用户自服务只读视图，
// 数据源 GET /api/user/aff（本人作用域）：邀请码 / 邀请人数 / 累计返利 /
// 返利比例。aff_code 服务端惰性补发（wave1 前注册的老用户首次访问时生成）。
// 邀请链接 = {origin}/register?aff={code}，Register 页已支持 ?aff= 预填；
// 返利入账发生在充值结算事务内（order.go settleTopupOrder），此处不写数据。
import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Col, Input, Row, Space, Statistic, Typography } from 'antd'
import { CopyOutlined, ReloadOutlined } from '@ant-design/icons'
import { ApiError, api } from '../api/client'
import type { AffSummary } from '../api/types'
import { QUOTA_PER_DOLLAR } from '../api/format'

const { Text } = Typography

export default function InvitePage() {
  const { message } = App.useApp()
  const [aff, setAff] = useState<AffSummary | null>(null)

  const load = useCallback(async () => {
    try {
      const d = await api.get<AffSummary>('/api/user/aff')
      setAff(d)
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
    }
  }, [message])

  useEffect(() => {
    void load()
  }, [load])

  // 邀请链接指向注册页并预填邀请码（Register initialValues 读 ?aff=）。
  const link = aff ? `${window.location.origin}/register?aff=${aff.aff_code}` : ''
  const dollar = (quota: number) => `$${(quota / QUOTA_PER_DOLLAR).toFixed(4)}`

  // copy 剪贴板写入：非安全上下文（HTTP）或权限拒绝时提示手动复制。
  const copy = async (text: string, tip: string) => {
    try {
      await navigator.clipboard.writeText(text)
      message.success(tip)
    } catch {
      message.info('复制失败，请手动选择复制')
    }
  }

  return (
    <div>
      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col span={8}>
          <Card>
            <Statistic
              title="累计返利"
              value={aff?.aff_history_quota ?? 0}
              suffix={
                <Text type="secondary" style={{ fontSize: 14 }}>
                  {aff ? dollar(aff.aff_history_quota) : '-'}
                </Text>
              }
            />
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic title="邀请人数" value={aff?.invited_count ?? 0} />
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic
              title="返利比例"
              value={aff?.rebate_percent ?? 0}
              suffix={
                <Button size="small" icon={<ReloadOutlined />} onClick={() => void load()}>
                  刷新
                </Button>
              }
            />
          </Card>
        </Col>
      </Row>

      <Card title="我的邀请码">
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          <Space.Compact style={{ width: '100%', maxWidth: 640 }}>
            <Input value={aff?.aff_code ?? ''} readOnly placeholder="加载中…" />
            <Button
              icon={<CopyOutlined />}
              disabled={!aff?.aff_code}
              onClick={() => void copy(aff?.aff_code ?? '', '邀请码已复制')}
            >
              复制邀请码
            </Button>
            <Button
              type="primary"
              icon={<CopyOutlined />}
              disabled={!link}
              onClick={() => void copy(link, '邀请链接已复制')}
            >
              复制邀请链接
            </Button>
          </Space.Compact>
          <Input value={link} readOnly placeholder="邀请链接生成中…" />
          <Alert
            type="info"
            showIcon
            message="被邀请人通过链接注册后，其每笔在线充值你将按比例获得额度返利，返利随充值结算自动入账余额。"
          />
        </Space>
      </Card>
    </div>
  )
}
