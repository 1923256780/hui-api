package store

import (
	"os"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/model"
)

// TestOpenReadOnlyBlocksWrites 建库写入样本 → 关闭 → 只读重开：
// 读正常；INSERT/UPDATE/DELETE/CREATE TABLE 全部失败；PRAGMA query_only=1；
// 且原文件未被任何探测写污染。
func TestOpenReadOnlyBlocksWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	seed := &model.User{Username: "legacy-user", PasswordHash: "$2a$10$seedseedseedseedseedseedseedseedseedseedseedseedsee", Quota: 123}
	if err := st.Write.Create(seed).Error; err != nil {
		t.Fatalf("写入样本失败: %v", err)
	}
	if err := st.SetOption("keep-key", "keep-value"); err != nil {
		t.Fatalf("写入样本 option 失败: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关闭测试库失败: %v", err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("只读打开失败: %v", err)
	}
	defer func() { _ = CloseReadOnly(ro) }()

	// 读正常。
	var n int64
	if err := ro.Model(&model.User{}).Count(&n).Error; err != nil {
		t.Fatalf("只读池查询失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("只读池期望读到 1 行 users，实际 %d", n)
	}

	// 连接层守卫生效：query_only 恒为 1。
	var queryOnly int
	if err := ro.Raw("PRAGMA query_only").Scan(&queryOnly).Error; err != nil {
		t.Fatalf("读取 PRAGMA query_only 失败: %v", err)
	}
	if queryOnly != 1 {
		t.Fatalf("PRAGMA query_only 期望 1，实际 %d", queryOnly)
	}

	// 写路径全部失败（INSERT/UPDATE/DELETE/DDL/事务写）。
	if err := ro.Exec(`INSERT INTO options (key, value) VALUES ('__probe__', 'x')`).Error; err == nil {
		t.Fatal("只读池 INSERT 竟然成功，只读纪律失守")
	}
	if err := ro.Exec(`UPDATE options SET value = 'x' WHERE key = 'keep-key'`).Error; err == nil {
		t.Fatal("只读池 UPDATE 竟然成功，只读纪律失守")
	}
	if err := ro.Exec(`DELETE FROM options WHERE key = 'keep-key'`).Error; err == nil {
		t.Fatal("只读池 DELETE 竟然成功，只读纪律失守")
	}
	if err := ro.Exec(`CREATE TABLE __probe_tbl (id integer)`).Error; err == nil {
		t.Fatal("只读池 CREATE TABLE 竟然成功，只读纪律失守")
	}
	if err := ro.Transaction(func(tx *gorm.DB) error {
		return tx.Exec(`INSERT INTO options (key, value) VALUES ('__probe_tx__', 'x')`).Error
	}).Error; err == nil {
		t.Fatal("只读池事务内 INSERT 竟然成功，只读纪律失守")
	}

	// 文件未被污染：读写重开后无任何探测键/表。
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("复检重开失败: %v", err)
	}
	defer func() { _ = st2.Close() }()
	var probe int64
	if err := st2.Read.Model(&model.Option{}).Where("key LIKE ?", "__probe%").Count(&probe).Error; err != nil {
		t.Fatalf("复检查询失败: %v", err)
	}
	if probe != 0 {
		t.Fatalf("只读池的探测写污染了源文件（%d 行 __probe__ 键）", probe)
	}
	var opt model.Option
	if err := st2.Read.Where("key = ?", "keep-key").First(&opt).Error; err != nil {
		t.Fatalf("复检读取原键失败: %v", err)
	}
	if opt.Value != "keep-value" {
		t.Fatalf("原键值被改动: %q", opt.Value)
	}
}

// TestOpenReadOnlyMissingFile 目标不存在时报错而非创建（mode=ro 语义）。
func TestOpenReadOnlyMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-exist.db")
	ro, err := OpenReadOnly(path)
	if err == nil {
		_ = CloseReadOnly(ro)
		t.Fatal("只读打开不存在的库应失败（mode=ro 不得创建文件）")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("只读打开失败后不应留下新建的数据库文件")
	}
}

// TestOpenReadOnlyWALSource 以 WAL 模式库为源：正常读，写被拒。
// 旧网关运行库为 WAL 模式，备份副本（-wal 缺失）也必须可只读打开。
func TestOpenReadOnlyWALSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal-source.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开 WAL 源库失败: %v", err)
	}
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if err := st.SetOption("wal-key", "v"); err != nil {
		t.Fatalf("写入样本失败: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("只读打开 WAL 源库失败: %v", err)
	}
	defer func() { _ = CloseReadOnly(ro) }()
	var n int64
	if err := ro.Model(&model.Option{}).Where("key = ?", "wal-key").Count(&n).Error; err != nil {
		t.Fatalf("WAL 源只读查询失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("WAL 源期望读到 1 行，实际 %d", n)
	}
	if err := ro.Exec(`DELETE FROM options WHERE key = 'wal-key'`).Error; err == nil {
		t.Fatal("WAL 源只读池 DELETE 竟然成功，只读纪律失守")
	}
}
