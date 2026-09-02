package hook

import (
	"context"
	"log"
)

// Noop 是空操作 Hook：占位与基准对照用，保证注册表里永远有一个安全实现。
type Noop struct{}

// Name 实现 Hook 接口。
func (Noop) Name() string { return "noop" }

// OnSuccess 实现 Hook 接口。
func (Noop) OnSuccess(context.Context, Event) error { return nil }

// OnFailure 实现 Hook 接口。
func (Noop) OnFailure(context.Context, Event) error { return nil }

// NewNoop 创建空操作 Hook。
func NewNoop() Hook { return Noop{} }

// Console 把事件打印到标准日志的 Hook：M1-wave1 的最小可用观测实现，
// 结构化日志交给 systemd 收集（docs/01 设计点 10）。M2 起逐步扩展 webhook 等实现。
type Console struct{}

// Name 实现 Hook 接口。
func (Console) Name() string { return "console" }

// OnSuccess 实现 Hook 接口。
func (Console) OnSuccess(_ context.Context, ev Event) error {
	log.Printf("[hook:console] success type=%s request=%s model=%s token=%d channel=%d data=%v",
		ev.Type, ev.RequestID, ev.Model, ev.TokenID, ev.ChannelID, ev.Data)
	return nil
}

// OnFailure 实现 Hook 接口。
func (Console) OnFailure(_ context.Context, ev Event) error {
	log.Printf("[hook:console] failure type=%s request=%s model=%s token=%d channel=%d err=%s",
		ev.Type, ev.RequestID, ev.Model, ev.TokenID, ev.ChannelID, ev.Err)
	return nil
}

// NewConsole 创建控制台日志 Hook。
func NewConsole() Hook { return Console{} }

// 编译期保证内置实现满足 Hook 接口。
var (
	_ Hook = Noop{}
	_ Hook = Console{}
)
