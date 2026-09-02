package gateway

import (
	"math/rand"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// Selector 从候选渠道中选择 deployment（docs/01 设计点 3）：
//
//	启用渠道（status=1）→ 协议渠道类型过滤 → 模型清单匹配 → 熔断过滤
//	→ 排除集过滤 → priority 降序取最高优先级组 → 组内 weight 加权随机。
//
// 随机性仅用于负载均衡调度（非安全敏感场景），使用 math/rand 顶层函数
// （Go 1.22+ 自动种子化且并发安全）；令牌与密钥场景均使用 crypto/sha256。
type Selector struct {
	st *store.Store
}

// NewSelector 构造选择器。
func NewSelector(st *store.Store) *Selector {
	return &Selector{st: st}
}

// Pick 执行一轮选择。
// excluded 为本次请求已失败的渠道 ID 集合（跨跳累积，上限见 MaxExcluded）。
// 无可用渠道时返回 (nil, nil) —— 调用方映射 503 语义错误；库错误返回 error。
func (s *Selector) Pick(channelType int, name string, excluded map[int64]bool) (*model.Channel, error) {
	channels, err := s.st.GetEnabledChannels()
	if err != nil {
		return nil, err
	}

	// 同一优先级分组的最高组（渠道已按 priority desc 排序）。
	var best []*model.Channel
	bestPriority := int64(0)
	for i := range channels {
		ch := &channels[i]
		if ch.Type != channelType {
			continue // 协议与渠道类型不匹配：本地快速跳过（渠道组不匹配）
		}
		if !store.ChannelServesModel(*ch, name) {
			continue
		}
		if excluded[ch.ID] {
			continue
		}
		if len(best) > 0 && ch.Priority < bestPriority {
			break // 已排序：后续优先级只会更低
		}
		if len(best) == 0 {
			bestPriority = ch.Priority
		}
		best = append(best, ch)
	}
	if len(best) == 0 {
		return nil, nil
	}
	return s.weightedPick(best), nil
}

// weightedPick 组内加权随机：weight<=0 不参与；全部 weight<=0 时均匀随机。
func (s *Selector) weightedPick(cands []*model.Channel) *model.Channel {
	total := int64(0)
	for _, ch := range cands {
		if ch.Weight > 0 {
			total += ch.Weight
		}
	}
	if total <= 0 {
		return cands[rand.Intn(len(cands))]
	}
	r := rand.Int63n(total)
	var acc int64
	for _, ch := range cands {
		if ch.Weight <= 0 {
			continue
		}
		acc += ch.Weight
		if r < acc {
			return ch
		}
	}
	// 数学上不可达；兜底返回首个正权重渠道。
	for _, ch := range cands {
		if ch.Weight > 0 {
			return ch
		}
	}
	return cands[0]
}
