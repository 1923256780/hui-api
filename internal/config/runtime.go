package config

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/1923256780/hui-api/internal/store"
)

// snapshot 是运行轨配置的一次不可变快照。整体经 atomic.Value 原子替换，
// 读侧永远看到完整一致的键值集合与单调递增的版本号。
type snapshot struct {
	Version int64
	Values  map[string]string
}

// Runtime 是运行轨配置：以 options 表为数据源的只读覆盖层。
// 管理面（M2）写 options 后调用 Reload 即热生效，无需重启进程。
type Runtime struct {
	v     atomic.Value // 存 snapshot
	store *store.Store
}

// NewRuntime 构造 Runtime 并完成首次加载。构造失败视为启动错误（库不可读）。
func NewRuntime(s *store.Store) (*Runtime, error) {
	r := &Runtime{store: s}
	r.v.Store(snapshot{Version: 0, Values: map[string]string{}})
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload 从 options 表重建快照并原子替换。每次成功 Reload 版本号 +1。
func (r *Runtime) Reload() error {
	values, err := r.store.GetAllOptions()
	if err != nil {
		return fmt.Errorf("热加载 options: %w", err)
	}
	current := r.Version()
	r.v.Store(snapshot{Version: current + 1, Values: values})
	return nil
}

// Version 返回当前快照版本号（0 表示尚未加载）。
func (r *Runtime) Version() int64 {
	return r.load().Version
}

// Get 返回键值与是否存在。
func (r *Runtime) Get(key string) (string, bool) {
	v, ok := r.load().Values[key]
	return v, ok
}

// GetInt64 返回整型配置值；键不存在或解析失败时返回 def。
func (r *Runtime) GetInt64(key string, def int64) int64 {
	v, ok := r.Get(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return def
	}
	return n
}

// GetBool 返回布尔配置值；键不存在或解析失败时返回 def。
func (r *Runtime) GetBool(key string, def bool) bool {
	v, ok := r.Get(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// load 原子读取当前快照。
func (r *Runtime) load() snapshot {
	return r.v.Load().(snapshot)
}
