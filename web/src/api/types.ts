// types.ts 是管理面 API 的响应类型，与 internal/api 各视图/模型字段一一对应
// （契约见 docs/05 第五节）。

export interface Paged<T> {
  items: T[]
  total: number
  page: number
  page_size: number
}

// 渠道视图：key 为脱敏回显（首 3+***+末 4），编辑提交留空 = 保留旧密钥。
export interface ChannelView {
  id: number
  name: string
  type: number // 1=OpenAI 兼容 2=Anthropic
  base_url: string
  key: string
  models: string // 逗号分隔模型清单，"*" 表示通配
  priority: number
  weight: number
  status: number // 1 启用 2 禁用 3 熔断中
  param_override: string
  created_time: number
  updated_time: number
}

export interface ChannelTestResult {
  success: boolean
  status_code: number
  time_ms: number
  message: string
}

export interface Token {
  id: number
  user_id: number
  name: string
  status: number
  quota: number
  remain_quota: number
  unlimited_quota: boolean
  budget_duration: string // '' | '24h' | '7d' | '30d' | 'monthly'
  budget_reset_at: number
  tpm_rpm: string // JSON {"tpm":..,"rpm":..}
  tags: string // JSON 数组字符串
  group: string
  model_limits: string // 逗号分隔白名单，空=不限
  allow_ips: string
  expired_time: number // -1 永久
  created_time: number
  accessed_time: number
}

export interface UserInfo {
  id: number
  username: string
  display_name: string
  role: number // 1 普通用户 100 管理员（root）
  status: number
  quota: number
  used_quota: number
  email: string
  group: string
  created_time: number
  last_login_time: number
}

export interface Redemption {
  id: number
  key: string
  name: string
  status: number // 1 未使用 2 已核销 3 已作废 4 已过期
  quota: number
  created_by: number
  used_by: number
  used_time: number
  expired_time: number // -1 永久
  created_time: number
}

export interface LogEntry {
  id: number
  user_id: number
  token_id: number
  channel_id: number
  protocol: string
  model_name: string
  prompt_tokens: number
  completion_tokens: number
  quota: number
  use_time: number // 秒
  is_stream: boolean
  detail: string // 计费依据 JSON
  created_time: number
}

export interface OptionRow {
  key: string
  value: string
}

export interface OptionListData {
  items: OptionRow[]
  version: number
}
