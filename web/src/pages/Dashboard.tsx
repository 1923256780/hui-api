// Dashboard 数据看板：基于 /api/log 的今日数据聚合统计卡片与模型分布表。
// 统计口径：今日 0 点至今的日志（分页拉取上限 10 页 × 100 条，超出时明确提示）；
// 不引入重型图表库，表格 + 卡片即满足管理概览。
import { useEffect, useMemo, useState } from 'react'
import { Alert, Card, Col, Row, Spin, Statistic, Table, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import dayjs from 'dayjs'
import { api } from '../api/client'
import type { LogEntry, Paged } from '../api/types'
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

export default function DashboardPage() {
  const [loading, setLoading] = useState(true)
  const [failed, setFailed] = useState(false)
  const [truncated, setTruncated] = useState(false)
  const [logs, setLogs] = useState<LogEntry[]>([])

  useEffect(() => {
    let alive = true
    const start = dayjs().startOf('day').unix()
    const end = dayjs().unix()
    ;(async () => {
      const collected: LogEntry[] = []
      let total = 0
      try {
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
      } catch {
        setFailed(true)
      }
      if (!alive) return
      setLogs(collected)
      setTruncated(total > collected.length)
      setLoading(false)
    })()
    return () => {
      alive = false
    }
  }, [])

  const { todayRequests, todayQuota, todayTokens, modelRows } = useMemo(() => {
    const byModel = new Map<string, ModelStat>()
    let quota = 0
    let tokens = 0
    for (const l of logs) {
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
    return { todayRequests: logs.length, todayQuota: quota, todayTokens: tokens, modelRows: rows }
  }, [logs])

  return (
    <Spin spinning={loading}>
      {failed && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="统计加载失败"
          description="无法读取日志数据，请检查服务状态后刷新。"
        />
      )}
      <Row gutter={16}>
        <Col span={8}>
          <Card>
            <Statistic title="今日请求" value={todayRequests} suffix="次" />
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic title="今日消耗" value={quotaToDollar(todayQuota)} />
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic title="今日 tokens" value={todayTokens} suffix="枚" />
          </Card>
        </Col>
      </Row>

      {truncated && (
        <Alert
          type="warning"
          showIcon
          style={{ marginTop: 16 }}
          message={`今日日志超过 ${logs.length} 条，统计仅基于最近拉取的 ${logs.length} 条（上限 ${MAX_PAGES * PAGE_SIZE} 条）`}
        />
      )}

      <Card title="模型分布（按消耗排序）" style={{ marginTop: 16 }}>
        <Table<ModelStat>
          rowKey="model"
          size="small"
          columns={columns}
          dataSource={modelRows}
          pagination={false}
          locale={{ emptyText: '今日暂无请求日志' }}
        />
      </Card>

      <Text type="secondary" style={{ display: 'block', marginTop: 12 }}>
        统计来源：管理面 /api/log 今日区间数据；渠道维度分布待日志 channel_id 回填后开放。
      </Text>
    </Spin>
  )
}
