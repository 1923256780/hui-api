// Dashboard 数据看板：按角色取数（M2 收官 Task #19）——root 聚合管理面
// /api/log 今日区间（分页拉取上限 10 页 × 100 条，超出时明确提示）；
// 普通用户调用登录态自服务端点 /api/user/stats（服务端 SQL 聚合今日汇总
// 与模型分布，一次请求返回）。任一数据源加载失败时优雅降级为空态
// （0 值卡片 + 空表），不再显示错误横幅。
import { useEffect, useState } from 'react'
import { Alert, Card, Col, Row, Spin, Statistic, Table, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import dayjs from 'dayjs'
import { api, getSession } from '../api/client'
import type { LogEntry, Paged, UserStats } from '../api/types'
import { quotaToDollar } from '../api/format'

const { Text } = Typography

const PAGE_SIZE = 100
const MAX_PAGES = 10

interface ModelStat {
  model: string
  requests: number
  promptTokens: number
  completionTokens: number
  quota: number
}

interface Summary {
  requests: number
  quota: number
  tokens: number
  rows: ModelStat[]
}

const EMPTY_SUMMARY: Summary = { requests: 0, quota: 0, tokens: 0, rows: [] }

const columns: ColumnsType<ModelStat> = [
  { title: '模型', dataIndex: 'model' },
  { title: '请求数', dataIndex: 'requests', width: 110 },
  {
    title: '输入 tokens',
    dataIndex: 'promptTokens',
    width: 140,
    render: (v: number) => v.toLocaleString(),
  },
  {
    title: '输出 tokens',
    dataIndex: 'completionTokens',
    width: 140,
    render: (v: number) => v.toLocaleString(),
  },
  {
    title: '消耗',
    dataIndex: 'quota',
    width: 130,
    render: (v: number) => quotaToDollar(v),
  },
]

// aggregateFromLogs root 数据源：管理面 /api/log 今日区间分页拉取后前端聚合。
// 返回 [汇总, 是否截断]。
async function aggregateFromLogs(): Promise<[Summary, boolean]> {
  const start = dayjs().startOf('day').unix()
  const end = dayjs().unix()
  const collected: LogEntry[] = []
  let total = 0
  for (let page = 1; page <= MAX_PAGES; page++) {
    const d = await api.get<Paged<LogEntry>>('/api/log', {
      page,
      page_size: PAGE_SIZE,
      start_timestamp: start,
      end_timestamp: end,
    })
    total = d.total
    collected.push(...d.items)
    if (collected.length >= total || d.items.length === 0) break
  }
  const byModel = new Map<string, ModelStat>()
  let quota = 0
  let tokens = 0
  for (const l of collected) {
    quota += l.quota
    tokens += l.prompt_tokens + l.completion_tokens
    const row =
      byModel.get(l.model_name) ??
      { model: l.model_name, requests: 0, promptTokens: 0, completionTokens: 0, quota: 0 }
    row.requests += 1
    row.promptTokens += l.prompt_tokens
    row.completionTokens += l.completion_tokens
    row.quota += l.quota
    byModel.set(l.model_name, row)
  }
  const rows = [...byModel.values()].sort((a, b) => b.quota - a.quota)
  return [{ requests: collected.length, quota, tokens, rows }, total > collected.length]
}

// fetchUserStats 普通用户数据源：登录态自服务端点，服务端已按会话用户聚合。
async function fetchUserStats(): Promise<Summary> {
  const s = await api.get<UserStats>('/api/user/stats')
  return {
    requests: s.requests,
    quota: s.quota,
    tokens: s.tokens,
    rows: s.models.map((m) => ({
      model: m.model_name,
      requests: m.requests,
      promptTokens: m.prompt_tokens,
      completionTokens: m.completion_tokens,
      quota: m.quota,
    })),
  }
}

export default function DashboardPage() {
  const [loading, setLoading] = useState(true)
  const [truncated, setTruncated] = useState(false)
  const [summary, setSummary] = useState<Summary>(EMPTY_SUMMARY)
  // 角色以会话元数据为准（ConsoleLayout 挂载时已经 /api/user/self 刷新）。
  const isAdmin = getSession()?.role === 100

  useEffect(() => {
    let alive = true
    ;(async () => {
      try {
        if (isAdmin) {
          const [s, tr] = await aggregateFromLogs()
          if (!alive) return
          setSummary(s)
          setTruncated(tr)
        } else {
          const s = await fetchUserStats()
          if (!alive) return
          setSummary(s)
          setTruncated(false)
        }
      } catch (err) {
        // 优雅降级：加载失败保留全零空态，不显示错误横幅（控制台留痕便于排查）。
        console.warn('[dashboard] 统计加载失败，已降级为空态', err)
      } finally {
        if (alive) setLoading(false)
      }
    })()
    return () => {
      alive = false
    }
  }, [isAdmin])

  return (
    <Spin spinning={loading}>
      <Row gutter={16}>
        <Col span={8}>
          <Card>
            <Statistic title="今日请求" value={summary.requests} suffix="次" />
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic title="今日消耗" value={quotaToDollar(summary.quota)} />
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic title="今日 tokens" value={summary.tokens} suffix="枚" />
          </Card>
        </Col>
      </Row>

      {isAdmin && truncated && (
        <Alert
          type="warning"
          showIcon
          style={{ marginTop: 16 }}
          message={`今日日志超过 ${summary.requests} 条，统计仅基于最近拉取的 ${summary.requests} 条（上限 ${MAX_PAGES * PAGE_SIZE} 条）`}
        />
      )}

      <Card title="模型分布（按消耗排序）" style={{ marginTop: 16 }}>
        <Table<ModelStat>
          rowKey="model"
          size="small"
          columns={columns}
          dataSource={summary.rows}
          pagination={false}
          locale={{ emptyText: '今日暂无请求日志' }}
        />
      </Card>

      <Text type="secondary" style={{ display: 'block', marginTop: 12 }}>
        {isAdmin
          ? '统计来源：管理面 /api/log 今日区间数据；渠道维度分布待日志 channel_id 回填后开放。'
          : '统计来源：自服务 /api/user/stats（当前账号今日数据）；渠道维度分布待日志 channel_id 回填后开放。'}
      </Text>
    </Spin>
  )
}
