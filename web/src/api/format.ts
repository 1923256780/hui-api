// format.ts 是展示层格式化工具：quota/美元换算、unix 秒时间显示与状态枚举文案。

import dayjs from 'dayjs'

// 与 internal/model.QuotaPerDollar 同源：500000 quota = $1。
export const QUOTA_PER_DOLLAR = 500000

export function quotaToDollar(quota: number): string {
  return `$${(quota / QUOTA_PER_DOLLAR).toFixed(4)}`
}

export function formatTime(unix: number): string {
  return unix > 0 ? dayjs(unix * 1000).format('YYYY-MM-DD HH:mm:ss') : '-'
}

// formatExpiry：expired_time 语义（-1 = 永不过期）。
export function formatExpiry(unix: number): string {
  return unix === -1 ? '永久' : formatTime(unix)
}

export interface StatusTag {
  text: string
  color: string
}

// 通用状态（channels/tokens/users.status）。
export const COMMON_STATUS: Record<number, StatusTag> = {
  1: { text: '启用', color: 'green' },
  2: { text: '禁用', color: 'red' },
  3: { text: '熔断中', color: 'orange' },
}

// 兑换码状态（redemptions.status）。
export const REDEMPTION_STATUS: Record<number, StatusTag> = {
  1: { text: '未使用', color: 'green' },
  2: { text: '已核销', color: 'blue' },
  3: { text: '已作废', color: 'red' },
}

export function statusTag(status: number, map: Record<number, StatusTag>): StatusTag {
  return map[status] ?? { text: `未知(${status})`, color: 'red' }
}

// prettyJSON 美化 JSON 文本；解析失败时原样返回（如 detail 为空或非 JSON）。
export function prettyJSON(text: string): string {
  if (!text) return ''
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

// safeParse 返回 JSON 解析结果或 null（用于 tpm_rpm / tags 等宽松展示）。
export function safeParse<T>(text: string): T | null {
  if (!text) return null
  try {
    return JSON.parse(text) as T
  } catch {
    return null
  }
}
