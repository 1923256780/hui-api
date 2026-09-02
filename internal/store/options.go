package store

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/model"
)

// GetAllOptions 读取 options 全量键值。运行轨热更（config.Runtime.Reload）的数据源。
func (s *Store) GetAllOptions() (map[string]string, error) {
	var rows []model.Option
	if err := s.Read.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("读取 options: %w", err)
	}
	values := make(map[string]string, len(rows))
	for _, r := range rows {
		values[r.Key] = r.Value
	}
	return values, nil
}

// SetOption 以 upsert 语义写入单个键值。管理面写 options 后由调用方触发热加载。
func (s *Store) SetOption(key, value string) error {
	var opt model.Option
	err := s.Write.Where("key = ?", key).First(&opt).Error
	if err == nil {
		if err := s.Write.Model(&model.Option{}).Where("key = ?", key).Update("value", value).Error; err != nil {
			return fmt.Errorf("更新 option %s: %w", key, err)
		}
		return nil
	}
	if createErr := s.Write.Create(&model.Option{Key: key, Value: value}).Error; createErr != nil {
		return fmt.Errorf("创建 option %s: %w", key, createErr)
	}
	return nil
}

// SetOptions 批量 upsert（单事务）。供 M2 管理面批量保存配置复用。
func (s *Store) SetOptions(kv map[string]string) error {
	return s.Write.Transaction(func(tx *gorm.DB) error {
		for k, v := range kv {
			var opt model.Option
			if err := tx.Where("key = ?", k).First(&opt).Error; err == nil {
				if err := tx.Model(&model.Option{}).Where("key = ?", k).Update("value", v).Error; err != nil {
					return fmt.Errorf("更新 option %s: %w", k, err)
				}
				continue
			}
			if err := tx.Create(&model.Option{Key: k, Value: v}).Error; err != nil {
				return fmt.Errorf("创建 option %s: %w", k, err)
			}
		}
		return nil
	})
}

// DeleteOption 删除单个键。删除不存在的键视为成功（幂等）。
func (s *Store) DeleteOption(key string) error {
	if err := s.Write.Where("key = ?", key).Delete(&model.Option{}).Error; err != nil {
		return fmt.Errorf("删除 option %s: %w", key, err)
	}
	return nil
}
