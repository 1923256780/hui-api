package hook

import (
	"fmt"
	"sort"
	"sync"
)

// Registry 是 Hook 注册表：按名字去重，供 Dispatcher 遍历投递。
type Registry struct {
	mu    sync.RWMutex
	hooks map[string]Hook
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{hooks: make(map[string]Hook)}
}

// Register 注册 Hook。同名重复注册返回错误，避免静默覆盖。
func (r *Registry) Register(h Hook) error {
	if h == nil {
		return fmt.Errorf("hook 不能为空")
	}
	name := h.Name()
	if name == "" {
		return fmt.Errorf("hook 名不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.hooks[name]; exists {
		return fmt.Errorf("hook %q 已注册", name)
	}
	r.hooks[name] = h
	return nil
}

// Unregister 按名注销。不存在的名视为成功（幂等）。
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.hooks, name)
}

// List 返回当前全部 Hook（按名字排序，保证投递顺序稳定）。
func (r *Registry) List() []Hook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.hooks))
	for name := range r.hooks {
		names = append(names, name)
	}
	sort.Strings(names)
	hooks := make([]Hook, 0, len(names))
	for _, name := range names {
		hooks = append(hooks, r.hooks[name])
	}
	return hooks
}

// Count 返回已注册 Hook 数量。
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.hooks)
}
