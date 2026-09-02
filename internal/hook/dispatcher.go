package hook

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// 默认投递参数。
const (
	DefaultQueueSize   = 256
	DefaultWorkers     = 1
	deliverTimeout     = 5 * time.Second
	defaultStopTimeout = 3 * time.Second
)

// Dispatcher 是有界队列异步投递器：
//   - Dispatch 非阻塞：队列满即丢弃并计数，主链路零等待；
//   - worker 消费事件并按 Err 是否为空路由到 OnSuccess / OnFailure；
//   - 单个 hook 的 panic 或错误不影响其他 hook 与后续事件。
type Dispatcher struct {
	queue     chan Event
	registry  *Registry
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	dropped   atomic.Int64
	processed atomic.Int64
	stopped   atomic.Bool
}

// NewDispatcher 创建分发器。queueSize<=0 时取默认值。
func NewDispatcher(reg *Registry, queueSize int) *Dispatcher {
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		queue:    make(chan Event, queueSize),
		registry: reg,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Start 启动 workers 个消费 goroutine。重复调用是安全的（幂等）。
func (d *Dispatcher) Start(workers int) {
	if workers <= 0 {
		workers = DefaultWorkers
	}
	d.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go d.worker()
	}
}

// Dispatch 异步投递事件。返回 false 表示未入队（已停止或队列满，均计入丢弃）。
func (d *Dispatcher) Dispatch(ev Event) bool {
	if d.stopped.Load() || d.ctx.Err() != nil {
		d.dropped.Add(1)
		return false
	}
	select {
	case d.queue <- ev:
		return true
	default:
		d.dropped.Add(1)
		log.Printf("[hook] 队列已满，丢弃事件 type=%s request=%s", ev.Type, ev.RequestID)
		return false
	}
}

// worker 消费循环：退出条件是 ctx 取消（Stop 触发）。
func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for {
		select {
		case <-d.ctx.Done():
			return
		case ev := <-d.queue:
			d.deliver(ev)
		}
	}
}

// deliver 把事件投递给全部注册 Hook。
func (d *Dispatcher) deliver(ev Event) {
	failed := ev.Err != ""
	ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	defer cancel()
	for _, h := range d.registry.List() {
		d.invoke(h, failed, ctx, ev)
	}
	d.processed.Add(1)
}

// invoke 调用单个 Hook，隔离 panic 与错误：任一 hook 异常不影响其他 hook 与后续事件。
func (d *Dispatcher) invoke(h Hook, failed bool, ctx context.Context, ev Event) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[hook] %s panic 已恢复: %v", h.Name(), r)
		}
	}()
	var err error
	if failed {
		err = h.OnFailure(ctx, ev)
	} else {
		err = h.OnSuccess(ctx, ev)
	}
	if err != nil {
		log.Printf("[hook] %s 投递失败: %v", h.Name(), err)
	}
}

// Stop 停止分发器：取消 ctx、等待 worker 退出，超时则放弃等待（不阻塞调用方）。
func (d *Dispatcher) Stop(timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	d.stopped.Store(true)
	d.cancel()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("[hook] 停止超时(%s)，可能仍有事件在处理", timeout)
	}
}

// Dropped 返回累计丢弃事件数（队列满或已停止后的投递）。
func (d *Dispatcher) Dropped() int64 {
	return d.dropped.Load()
}

// Processed 返回累计完成投递的事件数（按事件计，一个事件广播给多个 hook 算一次）。
func (d *Dispatcher) Processed() int64 {
	return d.processed.Load()
}
