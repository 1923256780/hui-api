package store

import (
	"errors"
	"fmt"
	"strconv"

	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/model"
)

// SchemaVersion 是当前代码对应的库 schema 版本。0001 基线由 AutoMigrate 以
// internal/model 为唯一源建表；后续 schema 变更按 docs/03 第四节规则以
// 只前进（up-only）的 SQL 迁移脚本叠加，本常量随之递增且禁止回退。
//
// 版本历史：
//
//	1（M1-wave1）六表基线；
//	2（M1-wave2）channels 新增 param_override 列（0002_channel_param_override.sql）。
const SchemaVersion int64 = 2

// OptionKeySchemaVersion 是 options 表中记录 schema 版本的键。
const OptionKeySchemaVersion = "schema_version"

// Migrate 执行版本化迁移并返回当前 schema 版本。
//
// 规则（docs/03 第四节）：
//  1. 基线（版本 1）：AutoMigrate 以 internal/model 为源创建六表；
//  2. 迁移幂等：重复启动重复执行不产生副作用；AutoMigrate 对后续版本的新列
//     同样自动补齐（与 migrations/*.sql 文档源保持等价，由 TestDDLEquivalence 对账）；
//  3. 成功后把 schema_version 写入 options 表；
//  4. 禁止修改历史迁移：本波之后的新变更走新版本号迁移，不回改基线。
func (s *Store) Migrate() (int64, error) {
	if err := s.Write.AutoMigrate(model.AllModels()...); err != nil {
		return 0, fmt.Errorf("AutoMigrate 基线表: %w", err)
	}
	if err := s.setSchemaVersion(SchemaVersion); err != nil {
		return 0, err
	}
	return SchemaVersion, nil
}

// setSchemaVersion 把 schema 版本写入 options（存在则更新，不存在则创建）。
func (s *Store) setSchemaVersion(v int64) error {
	value := strconv.FormatInt(v, 10)
	var opt model.Option
	err := s.Write.Where("key = ?", OptionKeySchemaVersion).First(&opt).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := s.Write.Create(&model.Option{Key: OptionKeySchemaVersion, Value: value}).Error; err != nil {
			return fmt.Errorf("写入 %s: %w", OptionKeySchemaVersion, err)
		}
	case err != nil:
		return fmt.Errorf("查询 %s: %w", OptionKeySchemaVersion, err)
	case opt.Value != value:
		if err := s.Write.Model(&model.Option{}).Where("key = ?", OptionKeySchemaVersion).
			Update("value", value).Error; err != nil {
			return fmt.Errorf("更新 %s: %w", OptionKeySchemaVersion, err)
		}
	}
	return nil
}

// SchemaVersionRead 从已迁移的库中读取记录的 schema 版本（未记录时返回 0）。
func (s *Store) SchemaVersionRead() (int64, error) {
	var opt model.Option
	err := s.Read.Where("key = ?", OptionKeySchemaVersion).First(&opt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取 %s: %w", OptionKeySchemaVersion, err)
	}
	v, err := strconv.ParseInt(opt.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("解析 %s=%q: %w", OptionKeySchemaVersion, opt.Value, err)
	}
	return v, nil
}
