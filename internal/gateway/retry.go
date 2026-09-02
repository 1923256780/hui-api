package gateway

import "time"

// BackoffKind 描述重试等待方式。
type BackoffKind int

const (
	BackoffNone BackoffKind = iota // 立即换点，不等待
	BackoffExp                     // 指数退避（RateLimit 专用）
)

// RetryPolicy 是一类错误的重试策略。
type RetryPolicy struct {
	Retryable bool        // 是否允许换点重试
	Backoff   BackoffKind // 等待方式
}

// MaxExcluded 是排除集跨跳累积上限：失败过的 deployment（按渠道 ID 去重）
// 达到该数量后不再重试，直接把最后一次错误透传给客户端。
const MaxExcluded = 5

// 默认策略表（docs/05 typed retry）：
//   - Auth：零重试——渠道密钥错误换点无意义，立即终止并按上游错误包装；
//   - RateLimit：可重试，指数退避（base*2^n，封顶 4s）；
//   - Timeout / BadRequest / ContextWindow / ContentPolicy / Server / Unknown：
//     可重试，立即换点。
//
// 注意：BadRequest 类换点重试在语义上通常无收益，但为满足「未配置模型路由
// 到错误渠道」的场景（换渠道可能命中支持该模型的渠道）仍允许换点；排除集
// 上限 5 兜底，避免放大故障。
func PolicyFor(class ErrClass) RetryPolicy {
	switch class {
	case ClassAuth:
		return RetryPolicy{Retryable: false}
	case ClassRateLimit:
		return RetryPolicy{Retryable: true, Backoff: BackoffExp}
	default:
		return RetryPolicy{Retryable: true, Backoff: BackoffNone}
	}
}

// RateLimit 退避参数：base=500ms，倍增，封顶 4s。
const (
	BackoffBase    = 500 * time.Millisecond
	BackoffMaxStep = 4 * time.Second
)

// RateLimitBackoff 计算第 attempt 次重试（从 0 计）前的等待时长。
func RateLimitBackoff(attempt int) time.Duration {
	d := BackoffBase
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= BackoffMaxStep {
			return BackoffMaxStep
		}
	}
	if d > BackoffMaxStep {
		return BackoffMaxStep
	}
	return d
}

// WaitBackoff 按策略等待（测试注入 sleepFunc 为零等待）。
func WaitBackoff(p RetryPolicy, attempt int) {
	if !p.Retryable || p.Backoff != BackoffExp {
		return
	}
	sleepFunc(RateLimitBackoff(attempt))
}
