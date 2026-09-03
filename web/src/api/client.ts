// client.ts 是管理台 API 客户端：统一携带会话 cookie、解析管理面包裹结构
//（{success,message,data}，契约 docs/05 第二节）、归一化错误，并在会话失效
//（401）时跳转登录页。转发面（/v1/*）不带该包裹，单独提供 fetchModels。

export class ApiError extends Error {
  readonly code: string
  readonly status: number

  constructor(message: string, code: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
  }
}

export type Query = Record<string, string | number | undefined | null>

// 会话信息缓存：登录成功后写入 localStorage，供布局展示与防自锁判断。
// 注意：真正的鉴权凭据是 HttpOnly 签名 cookie，localStorage 只存展示用元数据。
export interface SessionUser {
  id: number
  username: string
  display_name?: string
  role: number
  email?: string
  totp_enabled?: boolean // M3-wave2：GET /api/user/self 完整返回，登录响应不含此字段
}

const SESSION_KEY = 'hui-session'

export function saveSession(user: SessionUser): void {
  localStorage.setItem(SESSION_KEY, JSON.stringify(user))
}

export function getSession(): SessionUser | null {
  try {
    const raw = localStorage.getItem(SESSION_KEY)
    return raw ? (JSON.parse(raw) as SessionUser) : null
  } catch {
    return null
  }
}

export function clearSession(): void {
  localStorage.removeItem(SESSION_KEY)
}

function buildURL(path: string, query?: Query): string {
  if (!query) return path
  const usp = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined && value !== null && value !== '') {
      usp.set(key, String(value))
    }
  }
  const qs = usp.toString()
  return qs ? `${path}?${qs}` : path
}

interface Envelope<T> {
  success?: boolean
  message?: string
  code?: string
  data?: T
}

async function request<T>(path: string, init?: RequestInit & { query?: Query }): Promise<T> {
  const { query, ...rest } = init ?? {}
  let res: Response
  try {
    res = await fetch(buildURL(path, query), {
      credentials: 'same-origin',
      ...rest,
      headers: { 'Content-Type': 'application/json', ...(rest.headers ?? {}) },
    })
  } catch {
    throw new ApiError('网络请求失败，请检查服务是否可用', 'network_error', 0)
  }

  // 会话失效（未登录/过期/改密失效）→ 跳转登录页；登录接口自身的 401 除外。
  if (
    res.status === 401 &&
    !path.startsWith('/api/user/login') &&
    !window.location.pathname.startsWith('/login')
  ) {
    window.location.assign('/login')
  }

  let body: Envelope<T> | null = null
  try {
    body = (await res.json()) as Envelope<T>
  } catch {
    // 非 JSON 响应（如反代错误页），落入下方统一错误分支。
  }
  if (!res.ok || !body || body.success !== true) {
    throw new ApiError(
      body?.message || `请求失败（HTTP ${res.status}）`,
      body?.code ?? 'unknown',
      res.status,
    )
  }
  return body.data as T
}

export const api = {
  get: <T>(path: string, query?: Query) => request<T>(path, { query }),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'POST', body: JSON.stringify(body ?? {}) }),
  put: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'PUT', body: JSON.stringify(body ?? {}) }),
  del: <T>(path: string) => request<T>(path, { method: 'DELETE' }),
}

export interface ModelEntry {
  id: string
  object: string
  owned_by: string
}

export interface ModelsResponse {
  object: string
  data: ModelEntry[]
}

// fetchModels 调转发面 GET /v1/models（Bearer 令牌鉴权，响应为 OpenAI 形状，
// 不带管理面 success 包裹，错误为 OpenAI error object）。
export async function fetchModels(token: string): Promise<ModelsResponse> {
  let res: Response
  try {
    res = await fetch('/v1/models', { headers: { Authorization: `Bearer ${token}` } })
  } catch {
    throw new ApiError('网络请求失败，请检查服务是否可用', 'network_error', 0)
  }
  if (!res.ok) {
    let msg = `查询模型失败（HTTP ${res.status}）`
    try {
      const j = (await res.json()) as { error?: { message?: string } }
      if (j?.error?.message) msg = j.error.message
    } catch {
      // 保留默认文案
    }
    throw new ApiError(msg, 'models_error', res.status)
  }
  return (await res.json()) as ModelsResponse
}
