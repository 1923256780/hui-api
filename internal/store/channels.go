package store

import (
	"fmt"
	"strings"

	"github.com/1923256780/hui-api/internal/model"
)

// GetEnabledChannels 读取全部启用（status=1）渠道，按 priority 降序、id 升序稳定排序。
// 渠道量级为个位/十位数，全量读取后在内存中按协议与模型过滤（见 internal/gateway）。
func (s *Store) GetEnabledChannels() ([]model.Channel, error) {
	var rows []model.Channel
	if err := s.Read.Where("status = ?", model.StatusEnabled).
		Order("priority DESC, id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("读取启用渠道: %w", err)
	}
	return rows, nil
}

// ChannelServesModel 判断渠道模型清单（逗号分隔）是否包含指定模型。
// 清单项与目标模型均做 trim；支持 "*" 通配（渠道承接全部模型）。
func ChannelServesModel(ch model.Channel, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, m := range strings.Split(ch.Models, ",") {
		m = strings.TrimSpace(m)
		if m == "*" || m == name {
			return true
		}
	}
	return false
}

// ChannelModelList 返回渠道模型清单（去空格、去空项）。
func ChannelModelList(ch model.Channel) []string {
	var out []string
	for _, m := range strings.Split(ch.Models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}
