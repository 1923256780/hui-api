// Package hook 实现 hui-api 的异步旁路（docs/01 设计点 6）：
// 审计、通知、回调类动作经有界队列异步投递，失败不阻塞转发主链路、不影响响应时延。
// 事件带幂等键（IdempotencyKey），投递方保证同键事件可安全重复消费。
package hook

import (
	"context"
	"time"
)

// Event 是旁路投递的事件载荷。Err 非空表示失败事件（路由到 OnFailure）。
type Event struct {
	Type           string         // 事件类型，如 request.completed / request.failed
	RequestID      string         // 请求标识（幂等键来源之一）
	TokenID        int64          // 关联令牌
	ChannelID      int64          // 关联渠道
	Model          string         // 模型名
	Err            string         // 失败原因；非空 = 失败事件
	Timestamp      time.Time      // 事件发生时间
	IdempotencyKey string         // 幂等键：重复投递不产生重复副作用
	Data           map[string]any // 附加数据（如 quota、tokens 用量）
}

// Hook 是旁路动作的最小接口。实现方必须保证调用快速返回——重活自行异步；
// 阻塞会拖慢消费循环，但永远不会阻塞 API 主链路（Dispatch 非阻塞）。
type Hook interface {
	// Name 返回唯一名称，用于注册表去重与日志定位。
	Name() string
	// OnSuccess 处理成功事件。
	OnSuccess(ctx context.Context, ev Event) error
	// OnFailure 处理失败事件。
	OnFailure(ctx context.Context, ev Event) error
}
