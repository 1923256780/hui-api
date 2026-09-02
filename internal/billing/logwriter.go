// 请求日志异步批量落库（docs/01 设计点 2/10：主链路最短，重活走异步旁路）：
//
//   - Submit 强类型入队（LogRecord，非 map/string 拼装），队列满非阻塞丢弃并计数，
//     永不阻塞转发主链路；
//   - 后台 flusher 按批（batch 上限）或定时窗口（interval）攒批后单条批量 INSERT；
//   - Close(超时) 停止接收 → 排空（drain）channel 内剩余 → 等待 flusher 退出；
//   - 批量写失败降级为逐条重试（批量插入是单事务，整体失败无部分写入，重试安全）。
package billing

import (
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// 日志写盘默认参数。
const (
	DefaultLogBuffer   = 1024                     // channel 缓冲
	DefaultLogBatch    = 128                      // 单批最多条数
	DefaultLogInterval = 200 * time.Millisecond   // 攒批定时窗口
)

// Detail 是计费依据 JSON（logs.detail 列，docs/03：供对账反向重算）。
// 字段全部可选：零值整体编码为空串。
type Detail struct {
	Mode       string  `json:"mode,omitempty"`             // 计费模式
	Expr       string  `json:"expr,omitempty"`             // tiered_expr 表达式（计费依据）
	GroupRatio float64 `json:"group_ratio,omitempty"`      // 组倍率
	ModelRatio float64 `json:"model_ratio,omitempty"`      // classic 输入价（对账依据）
	CompRatio  float64 `json:"completion_ratio,omitempty"` // classic 输出倍率
	Frozen     int64   `json:"frozen,omitempty"`           // 预扣冻结额
	CacheRead  int     `json:"cache_read_tokens,omitempty"` // 缓存读 tokens（prompt 的子集）
	BilledIn   int     `json:"billed_input_tokens,omitempty"` // 表达式变量 p = prompt - cache
	Estimated  bool    `json:"estimated,omitempty"`        // usage 缺失，本地粗估
	Aborted    bool    `json:"aborted,omitempty"`          // 失败/流中断（退款终态）
	RefundFull bool    `json:"refund_full,omitempty"`      // 全额退款标记
	Unlimited  bool    `json:"unlimited,omitempty"`        // 令牌不限额（跳过账本）
	Err        string  `json:"error,omitempty"`            // 计费异常摘要（宁可少收路径）
}

// empty 判断 Detail 是否全零（全零不落 detail，省存储）。
func (d Detail) empty() bool {
	return d == Detail{}
}

// encode 序列化为 JSON 字符串（全零返回空串）。
func (d Detail) encode() string {
	if d.empty() {
		return ""
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// LogRecord 是一条请求日志的强类型记录（异步入参，不与 string/map 混用）。
type LogRecord struct {
	UserID           int64
	TokenID          int64
	ChannelID        int64
	Protocol         string
	ModelName        string
	PromptTokens     int    // 上游 usage 原始值（含缓存读部分）
	CompletionTokens int
	Quota            int64  // 实结（或粗估）quota
	UseTime          int64  // 请求耗时（秒）
	IsStream         bool
	CreatedTime      int64  // unix 秒
	Detail           Detail // 计费依据
}

// toModel 映射为 logs 表模型。
func (r LogRecord) toModel() model.Log {
	return model.Log{
		UserID:           r.UserID,
		TokenID:          r.TokenID,
		ChannelID:        r.ChannelID,
		Protocol:         r.Protocol,
		ModelName:        r.ModelName,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		Quota:            r.Quota,
		UseTime:          r.UseTime,
		IsStream:         r.IsStream,
		Detail:           r.Detail.encode(),
		CreatedTime:      r.CreatedTime,
	}
}

// LogWriterConfig 是异步日志写盘参数（零值字段取默认值）。
type LogWriterConfig struct {
	Buffer   int           // channel 缓冲
	Batch    int           // 单批上限
	Interval time.Duration // 攒批窗口
}

// AsyncLogWriter 是 logs 表异步批量写入器。并发安全。
type AsyncLogWriter struct {
	st      *store.Store
	ch      chan LogRecord
	stop    chan struct{}
	cfg     LogWriterConfig
	dropped atomic.Int64
	wg      sync.WaitGroup
	closed  atomic.Bool
	once    sync.Once
}

// NewAsyncLogWriter 以默认参数构造并启动后台 flusher。
func NewAsyncLogWriter(st *store.Store) *AsyncLogWriter {
	return NewAsyncLogWriterWith(st, LogWriterConfig{})
}

// NewAsyncLogWriterWith 以自定义参数构造并启动后台 flusher（测试/调优用）。
func NewAsyncLogWriterWith(st *store.Store, cfg LogWriterConfig) *AsyncLogWriter {
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultLogBuffer
	}
	if cfg.Batch <= 0 {
		cfg.Batch = DefaultLogBatch
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultLogInterval
	}
	w := &AsyncLogWriter{
		st:   st,
		ch:   make(chan LogRecord, cfg.Buffer),
		stop: make(chan struct{}),
		cfg:  cfg,
	}
	w.wg.Add(1)
	go w.loop()
	return w
}

// Dropped 返回累计丢弃条数（观测用）。
func (w *AsyncLogWriter) Dropped() int64 { return w.dropped.Load() }

// Submit 强类型入队：非阻塞，队列满丢弃并计数；停机后调用直接丢弃。
func (w *AsyncLogWriter) Submit(rec LogRecord) {
	if w.closed.Load() {
		w.dropped.Add(1)
		return
	}
	select {
	case w.ch <- rec:
	default:
		w.dropped.Add(1)
	}
}

// Close 停止接收 → 排空剩余日志 → 等待 flusher 退出（超过 timeout 记日志放弃等待）。
// 幂等。
func (w *AsyncLogWriter) Close(timeout time.Duration) {
	w.once.Do(func() {
		w.closed.Store(true)
		close(w.stop)
		done := make(chan struct{})
		go func() {
			w.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
			log.Printf("[billing] 日志 flusher 停机超时(%s)，剩余日志随进程终止", timeout)
		}
	})
}

// loop 是后台 flusher 主循环：攒批（批满或窗口到期）→ flush。
func (w *AsyncLogWriter) loop() {
	defer w.wg.Done()
	for {
		select {
		case rec := <-w.ch:
			batch := []LogRecord{rec}
			timer := time.NewTimer(w.cfg.Interval)
		collect:
			for len(batch) < w.cfg.Batch {
				select {
				case rec := <-w.ch:
					batch = append(batch, rec)
				case <-timer.C:
					break collect
				case <-w.stop:
					if !timer.Stop() {
						<-timer.C
					}
					batch = append(batch, w.drainBatch(w.cfg.Batch-len(batch))...)
					w.flush(batch)
					w.drainRest()
					return
				}
			}
			timer.Stop()
			w.flush(batch)
		case <-w.stop:
			w.drainRest()
			return
		}
	}
}

// drainBatch 非阻塞收集最多 max 条（排空阶段用）。
func (w *AsyncLogWriter) drainBatch(max int) []LogRecord {
	var batch []LogRecord
	for len(batch) < max {
		select {
		case rec := <-w.ch:
			batch = append(batch, rec)
		default:
			return batch
		}
	}
	return batch
}

// drainRest 排空 channel 全部剩余（一批批 flush）。
func (w *AsyncLogWriter) drainRest() {
	for {
		batch := w.drainBatch(w.cfg.Batch)
		if len(batch) == 0 {
			return
		}
		w.flush(batch)
	}
}

// flush 批量落库；失败降级逐条重试（防批量整体失败丢整批）。
func (w *AsyncLogWriter) flush(batch []LogRecord) {
	if len(batch) == 0 {
		return
	}
	rows := make([]model.Log, len(batch))
	for i, r := range batch {
		rows[i] = r.toModel()
	}
	if err := w.st.Write.Create(&rows).Error; err != nil {
		log.Printf("[billing] 批量写日志失败(%d 条)，降级逐条重试: %v", len(batch), err)
		for i := range rows {
			if err := w.st.Write.Create(&rows[i]).Error; err != nil {
				log.Printf("[billing] 写日志失败(模型 %s, quota %d): %v",
					rows[i].ModelName, rows[i].Quota, err)
			}
		}
	}
}
