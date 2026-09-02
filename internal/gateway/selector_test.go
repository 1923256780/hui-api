package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// newTestSelector 构造临时库上的选择器。
func newTestSelector(t *testing.T) (*Selector, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return NewSelector(st), st
}

func seedChannel(t *testing.T, st *store.Store, ch model.Channel) model.Channel {
	t.Helper()
	ch.Status = model.StatusEnabled
	ch.CreatedTime = time.Now().Unix()
	if err := st.Write.Create(&ch).Error; err != nil {
		t.Fatalf("写入渠道失败: %v", err)
	}
	return ch
}

// TestPickPriorityAheadOfWeight priority desc：低优先级不参与同轮调度。
func TestPickPriorityAheadOfWeight(t *testing.T) {
	sel, st := newTestSelector(t)
	low := seedChannel(t, st, model.Channel{Name: "low", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: "https://low", Models: "m1", Priority: 1, Weight: 100})
	high := seedChannel(t, st, model.Channel{Name: "high", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: "https://high", Models: "m1", Priority: 10, Weight: 1})

	for i := 0; i < 50; i++ {
		ch, err := sel.Pick(model.ChannelTypeOpenAICompatible, "m1", nil)
		if err != nil || ch == nil {
			t.Fatalf("Pick 失败: %v", err)
		}
		if ch.ID != high.ID {
			t.Fatalf("高优先级应独占调度，实际选中 %s（低=%d）", ch.Name, low.ID)
		}
	}
}

// TestPickWeightDistribution 同优先级内 weight 加权随机。
func TestPickWeightDistribution(t *testing.T) {
	sel, st := newTestSelector(t)
	a := seedChannel(t, st, model.Channel{Name: "a", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: "https://a", Models: "m1", Priority: 5, Weight: 9})
	b := seedChannel(t, st, model.Channel{Name: "b", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: "https://b", Models: "m1", Priority: 5, Weight: 1})
	_ = b

	countA := 0
	n := 2000
	for i := 0; i < n; i++ {
		ch, err := sel.Pick(model.ChannelTypeOpenAICompatible, "m1", nil)
		if err != nil || ch == nil {
			t.Fatalf("Pick 失败: %v", err)
		}
		if ch.ID == a.ID {
			countA++
		}
	}
	// 期望 ~90%（1800）；统计断言放宽到 80%-98%，两端均不应为 0。
	if countA < n*8/10 || countA > n*98/100 {
		t.Fatalf("weight 9:1 分布异常: a 命中 %d/%d", countA, n)
	}
}

// TestPickZeroWeightUniform 全部 weight=0 时均匀随机。
func TestPickZeroWeightUniform(t *testing.T) {
	sel, st := newTestSelector(t)
	a := seedChannel(t, st, model.Channel{Name: "a", Type: model.ChannelTypeOpenAICompatible,
		Models: "m1", Weight: 0})
	b := seedChannel(t, st, model.Channel{Name: "b", Type: model.ChannelTypeOpenAICompatible,
		Models: "m1", Weight: 0})

	seen := map[int64]bool{}
	for i := 0; i < 200; i++ {
		ch, err := sel.Pick(model.ChannelTypeOpenAICompatible, "m1", nil)
		if err != nil || ch == nil {
			t.Fatalf("Pick 失败: %v", err)
		}
		seen[ch.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("weight=0 渠道应均匀参与，实际仅命中 %v", seen)
	}
	_ = a
	_ = b
}

// TestPickExcludedAndFilters 排除集、协议类型、模型匹配、禁用渠道过滤。
func TestPickExcludedAndFilters(t *testing.T) {
	sel, st := newTestSelector(t)
	only := seedChannel(t, st, model.Channel{Name: "only", Type: model.ChannelTypeAnthropic,
		Models: "m1,m2"})

	// 排除唯一渠道 → nil。
	ch, err := sel.Pick(model.ChannelTypeAnthropic, "m1", map[int64]bool{only.ID: true})
	if err != nil || ch != nil {
		t.Fatalf("排除唯一渠道应返回 nil: %v %v", ch, err)
	}

	// 协议不匹配（OpenAI 请求 Anthropic 渠道）→ nil（渠道组不匹配快速失败）。
	if ch, err = sel.Pick(model.ChannelTypeOpenAICompatible, "m1", nil); err != nil || ch != nil {
		t.Fatalf("协议不匹配应返回 nil: %v %v", ch, err)
	}

	// 模型不匹配 → nil。
	if ch, err = sel.Pick(model.ChannelTypeAnthropic, "m9", nil); err != nil || ch != nil {
		t.Fatalf("模型不匹配应返回 nil: %v %v", ch, err)
	}

	// 正常命中。
	if ch, err = sel.Pick(model.ChannelTypeAnthropic, "m2", nil); err != nil || ch == nil || ch.ID != only.ID {
		t.Fatalf("m2 应命中: %v %v", ch, err)
	}

	// 禁用渠道不参与。
	_ = st.Write.Model(&model.Channel{}).Where("id = ?", only.ID).
		Update("status", model.StatusDisabled).Error
	if ch, err = sel.Pick(model.ChannelTypeAnthropic, "m2", nil); err != nil || ch != nil {
		t.Fatalf("禁用渠道不应参与: %v %v", ch, err)
	}
}

// TestPickWildcard 通配渠道承接全部模型。
func TestPickWildcard(t *testing.T) {
	sel, st := newTestSelector(t)
	seedChannel(t, st, model.Channel{Name: "wild", Type: model.ChannelTypeOpenAICompatible,
		Models: "*"})
	ch, err := sel.Pick(model.ChannelTypeOpenAICompatible, "any-model", nil)
	if err != nil || ch == nil {
		t.Fatalf("通配渠道应承接任意模型: %v %v", ch, err)
	}
}
