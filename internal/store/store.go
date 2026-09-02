// Package store 是 hui-api 的统一存储层：GORM + 纯 Go SQLite 驱动（免 CGO）。
//
// 连接模型（ADR 0003）：SQLite 单写多读——写路径单连接串行化，读路径独立连接池，
// 配合 WAL 日志模式获得读并发。PRAGMA（journal_mode=WAL、synchronous=NORMAL、
// busy_timeout=5000）通过 DSN 在每个连接建立时执行。
package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// PRAGMA 配置常量：连接建立时按此初始化。
const (
	PragmaBusyTimeout = 5000 // 写锁等待毫秒数
	PragmaJournalMode = "WAL"
	PragmaSynchronous = "NORMAL"

	// DefaultReadPoolSize 读池默认最大连接数。
	DefaultReadPoolSize = 4
)

// Store 聚合读写两个 GORM 连接。所有写操作必须走 Write，读操作优先走 Read。
type Store struct {
	Write *gorm.DB
	Read  *gorm.DB
}

// Open 打开（或创建）SQLite 数据库并初始化读写两个连接池。
// path 支持任意 SQLite 文件路径；含 query 参数时自动用 & 追加 PRAGMA。
func Open(path string) (*Store, error) {
	write, err := openDB(path, 1, 1)
	if err != nil {
		return nil, err
	}
	read, err := openDB(path, DefaultReadPoolSize, DefaultReadPoolSize)
	if err != nil {
		// 读池失败时回收写池，避免句柄泄漏。
		if sqlDB, dbErr := write.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		return nil, err
	}
	return &Store{Write: write, Read: read}, nil
}

// openDB 建立单个带 PRAGMA 的连接池。maxOpen/maxIdle 控制池大小。
func openDB(path string, maxOpen, maxIdle int) (*gorm.DB, error) {
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(%s)&_pragma=synchronous(%s)",
		path, PragmaBusyTimeout, PragmaJournalMode, PragmaSynchronous)
	if strings.Contains(path, "?") {
		dsn = fmt.Sprintf("%s&_pragma=busy_timeout(%d)&_pragma=journal_mode(%s)&_pragma=synchronous(%s)",
			path, PragmaBusyTimeout, PragmaJournalMode, PragmaSynchronous)
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite(%s): %w", path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取底层 sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	return db, nil
}

// Close 释放读写两个连接池。SQLite 场景下 Close 失败通常无恢复手段，仅聚合返回。
func (s *Store) Close() error {
	var errs []error
	for name, gdb := range map[string]*gorm.DB{"read": s.Read, "write": s.Write} {
		if gdb == nil {
			continue
		}
		sqlDB, err := gdb.DB()
		if err != nil {
			errs = append(errs, fmt.Errorf("获取 %s 连接池: %w", name, err))
			continue
		}
		if err := sqlDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("关闭 %s 连接池: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
