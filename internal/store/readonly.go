// Package store 内的只读打开能力（M4 旧库迁移专用，docs/07、ADR-0008）：
// 双层只读防御——DSN 层 mode=ro + 连接层 PRAGMA query_only=1。
package store

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ReadOnlyMaxConns 只读池连接数上限。迁移工具为顺序读取，小池足够；
// 双连接保证探活与后续查询互不阻塞。
const ReadOnlyMaxConns = 2

// OpenReadOnly 以双层只读防御打开既有 SQLite 数据库：
//
//  1. DSN 层 mode=ro：以只读语义打开，目标不存在时报错而非创建；
//  2. 连接层 PRAGMA query_only=1：经 DSN _pragma 对池内每个连接生效，
//     挡住一切写语句（INSERT/UPDATE/DELETE/DDL 均报 readonly 错误）。
//
// 本池只允许查询，任何写操作必然失败，行为由 readonly_test.go 固化。
// 路径统一转为 URI 正斜杠形式以兼容 Windows 盘符；路径中不得包含
// '?'/'#' 等 URI 保留字符（部署面约束，违反时报打开错误）。
func OpenReadOnly(path string) (*gorm.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("只读打开路径为空")
	}
	dsn := "file:" + filepath.ToSlash(filepath.Clean(path)) +
		"?mode=ro&_pragma=busy_timeout(" + strconv.Itoa(PragmaBusyTimeout) + ")&_pragma=query_only(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("只读打开 SQLite(%s): %w", path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取只读池底层 sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(ReadOnlyMaxConns)
	sqlDB.SetMaxIdleConns(ReadOnlyMaxConns)
	// 探活：DSN 错误可能延迟到首次使用才暴露，提前显式失败并回收句柄。
	if err := db.Exec("SELECT 1").Error; err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("只读连接探活失败(%s): %w", path, err)
	}
	return db, nil
}

// CloseReadOnly 关闭 OpenReadOnly 返回的只读池。
func CloseReadOnly(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("获取只读池: %w", err)
	}
	if err := sqlDB.Close(); err != nil {
		return fmt.Errorf("关闭只读池: %w", err)
	}
	return nil
}
