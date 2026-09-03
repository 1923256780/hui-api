// Package worker 是极简后台任务池（M3-wave4）：统一 Start/Stop 生命周期的
// 定时任务执行器——每任务一个 goroutine 内 time.Ticker 循环，panic recover
// 隔离（单任务异常不带崩进程）。不引入 cron 库：任务集合小（验证码清扫、
// 订单超时关单）、周期固定、无分布式调度需求，手写 ticker 池约百行可测可审计。
//
// 装配约定（cmd/hui-api/main.go）：启动链路构造 Pool 并 Start 全部任务，随
// graceful shutdown 一起 Stop（等待在途执行退出后再做后续停机步骤）。
package worker

import (
	"log"
	"sync"
	"time"
)

// Task 单个后台周期任务。
type Task struct {
	Name     string        // 任务名（panic 日志归因）
	Interval time.Duration // 执行周期；<=0 或 Run 为 nil 的任务被忽略
	Run      func()        // 执行体；panic 由池统一 recover
}

// Pool 极简 ticker 任务池：Stop 后 Start 为空操作，Stop 幂等并等待全部
// goroutine 退出。
type Pool struct {
	wg   sync.WaitGroup
	stop chan struct{}
	once sync.Once
}

// NewPool 构造任务池。
func NewPool() *Pool {
	return &Pool{stop: make(chan struct{})}
}

// Start 为每个合法任务启动一个 goroutine（非阻塞；首个 tick 在 Interval 后）。
func (p *Pool) Start(tasks ...Task) {
	for _, t := range tasks {
		if t.Interval <= 0 || t.Run == nil {
			continue
		}
		p.wg.Add(1)
		go p.loop(t)
	}
}

// loop 单任务循环：ticker 触发 → runOnce；stop 关闭即退出。
func (p *Pool) loop(t Task) {
	defer p.wg.Done()
	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.runOnce(t)
		}
	}
}

// runOnce 执行一次任务，panic 恢复为日志（不向外扩散、不带崩进程）。
func (p *Pool) runOnce(t Task) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("worker: 任务 %s panic 已恢复: %v", t.Name, r)
		}
	}()
	t.Run()
}

// Stop 停止全部任务并等待 goroutine 退出（幂等，可安全多次调用）。
func (p *Pool) Stop() {
	p.once.Do(func() { close(p.stop) })
	p.wg.Wait()
}
