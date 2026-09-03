// Models 模型广场：三个视角展示可用模型。
// 1) 转发面 GET /v1/models（需 sk- 令牌鉴权，令牌仅存本地 localStorage）；
// 2) 虚拟模型组（options 的 relay.virtual_model_groups，/v1/models 只返回组名）；
// 3) 渠道模型聚合（管理视角，按渠道分组展示各渠道 models 清单）。
import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  App,
  Button,
  Card,
  Empty,
  Input,
  List,
  Space,
  Spin,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import { ApiError, api, fetchModels } from '../api/client'
import type { ModelEntry, ModelsResponse } from '../api/client'
import type { ChannelView, OptionListData, Paged } from '../api/types'
import { safeParse } from '../api/format'

const { Text, Paragraph } = Typography

const TOKEN_STORAGE_KEY = 'hui-models-token'

interface VirtualGroups {
  [group: string]: string[]
}

export default function ModelsPage() {
  const { message } = App.useApp()
  const [token, setToken] = useState(() => localStorage.getItem(TOKEN_STORAGE_KEY) ?? '')
  const [querying, setQuerying] = useState(false)
  const [models, setModels] = useState<ModelEntry[] | null>(null)

  const [virtualGroups, setVirtualGroups] = useState<VirtualGroups | null>(null)
  const [channels, setChannels] = useState<ChannelView[] | null>(null)
  const [loadingAgg, setLoadingAgg] = useState(true)

  // 渠道聚合与虚拟组：管理面数据，挂载即拉。
  const loadAgg = useCallback(async () => {
    setLoadingAgg(true)
    try {
      const ch = await api.get<Paged<ChannelView>>('/api/channel', { page: 1, page_size: 100 })
      setChannels(ch.items)
      const opts = await api.get<OptionListData>('/api/option')
      const raw = opts.items.find((o) => o.key === 'relay.virtual_model_groups')?.value ?? ''
      setVirtualGroups(safeParse<VirtualGroups>(raw))
    } catch (err) {
      if (err instanceof ApiError && err.status !== 401) {
        message.error(err.message)
      }
      setChannels([])
    } finally {
      setLoadingAgg(false)
    }
  }, [message])

  useEffect(() => {
    void loadAgg()
  }, [loadAgg])

  const query = async () => {
    const t = token.trim()
    if (!t) {
      message.warning('请输入转发面令牌（sk- 开头）')
      return
    }
    setQuerying(true)
    try {
      const resp: ModelsResponse = await fetchModels(t)
      localStorage.setItem(TOKEN_STORAGE_KEY, t)
      setModels(resp.data)
      message.success(`查询成功，共 ${resp.data.length} 个可用模型`)
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '查询失败，请稍后重试')
    } finally {
      setQuerying(false)
    }
  }

  return (
    <div>
      <Card title="可用模型（转发面 /v1/models）" style={{ marginBottom: 16 }}>
        <Paragraph type="secondary">
          该端点按转发面语义鉴权（Bearer 令牌），与控制台会话无关。输入任一启用令牌的明文密钥
          （sk- 开头）查询；令牌仅保存在浏览器本地。
        </Paragraph>
        <Space.Compact style={{ width: '100%', maxWidth: 640 }}>
          <Input.Password
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder="sk-..."
            onPressEnter={() => void query()}
            autoComplete="off"
          />
          <Button type="primary" loading={querying} onClick={() => void query()}>
            查询
          </Button>
        </Space.Compact>

        {models !== null &&
          (models.length === 0 ? (
            <Empty description="无可用模型（未配置启用渠道或虚拟组）" style={{ marginTop: 24 }} />
          ) : (
            <List
              style={{ marginTop: 16 }}
              grid={{ gutter: 12, xs: 1, sm: 2, md: 3, lg: 4, xl: 4 }}
              dataSource={models}
              renderItem={(m) => (
                <List.Item>
                  <Card size="small" hoverable>
                    <Paragraph code copyable style={{ marginBottom: 4 }}>
                      {m.id}
                    </Paragraph>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      owned_by {m.owned_by}
                    </Text>
                  </Card>
                </List.Item>
              )}
            />
          ))}
      </Card>

      <Card title="虚拟模型组" style={{ marginBottom: 16 }}>
        {virtualGroups && Object.keys(virtualGroups).length > 0 ? (
          <List
            dataSource={Object.entries(virtualGroups)}
            renderItem={([name, members]) => (
              <List.Item>
                <Space wrap>
                  <Tag color="purple">{name}</Tag>
                  <Text type="secondary">成员（对客户端不可见，仅暴露组名）：</Text>
                  {members.map((m) => (
                    <Tag key={m}>{m}</Tag>
                  ))}
                </Space>
              </List.Item>
            )}
          />
        ) : (
          <Text type="secondary">
            未配置。可在系统设置中编辑 relay.virtual_model_groups（JSON {'{"组名": ["成员", ...]}'}）。
          </Text>
        )}
      </Card>

      <Card title="渠道模型聚合（管理视角）">
        <Spin spinning={loadingAgg}>
          {channels && channels.length > 0 ? (
            <List
              dataSource={channels}
              renderItem={(ch) => {
                const list = ch.models ? ch.models.split(',').filter(Boolean) : []
                return (
                  <List.Item>
                    <Space wrap align="start">
                      <Tag color={ch.type === 2 ? 'purple' : 'geekblue'}>
                        {ch.name}（#{ch.id}）
                      </Tag>
                      {ch.status === 2 && <Tag color="red">禁用</Tag>}
                      {list.length === 0 ? (
                        <Text type="secondary">未配置模型</Text>
                      ) : (
                        <Tooltip title={ch.models}>
                          <span>
                            {list.map((m) => (
                              <Tag key={m}>{m}</Tag>
                            ))}
                          </span>
                        </Tooltip>
                      )}
                    </Space>
                  </List.Item>
                )
              }}
            />
          ) : (
            <Alert type="info" showIcon message="暂无渠道，先在「渠道」页创建并配置模型清单。" />
          )}
        </Spin>
      </Card>
    </div>
  )
}
