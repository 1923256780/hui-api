// cmd/migrate 测试：run 函数的报告落盘语义（失败路径也落盘）与摘要稳健性。
// 迁移全链路正确性由 internal/migrate 集成测试覆盖，此处聚焦 CLI 层 IO 行为。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/1923256780/hui-api/internal/migrate"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// TestRunLegacyMissing 旧库不存在：run 返回错误（只读打开不创建文件），报告不落盘。
func TestRunLegacyMissing(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "no-such.db")
	reportPath := filepath.Join(dir, "report.json")
	if err := run(legacy, filepath.Join(dir, "hui.db"), reportPath, ""); err == nil {
		t.Fatal("旧库不存在应失败")
	}
	if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
		t.Fatalf("打开失败阶段无报告，不应落盘: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("只读打开失败不应创建旧库文件: %v", err)
	}
}

// TestRunReportWrittenOnFailure 迁移失败（旧库缺业务表，runGuard 阶段出错）时
// 报告仍落盘供人工排查，内容为合法 JSON 且 ok=false。
func TestRunReportWrittenOnFailure(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.db")
	// 空库（无任何业务表）：只读打开成功但查询 options 失败 → rep 非空 + err 非空。
	st, err := store.Open(legacy)
	if err != nil {
		t.Fatalf("建空 fixture 库失败: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关闭 fixture 库失败: %v", err)
	}

	reportPath := filepath.Join(dir, "report.json")
	if err := run(legacy, filepath.Join(dir, "hui.db"), reportPath, ""); err == nil {
		t.Fatal("缺业务表的旧库应迁移失败")
	}

	b, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("失败路径应落盘报告: %v", err)
	}
	var rep migrate.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("报告应为合法 JSON: %v\n%s", err, b)
	}
	if rep.OK || rep.LegacyPath != legacy {
		t.Fatalf("失败报告内容错误: ok=%v legacy=%s", rep.OK, rep.LegacyPath)
	}
}

// TestPrintSummaryRobust 摘要打印对 nil 与零值报告均稳健（不 panic）。
func TestPrintSummaryRobust(t *testing.T) {
	printSummary(nil)
	printSummary(&migrate.Report{})
}

// TestExportTokens 导出目标库令牌明文清单（TSV），行序按 id 升序。
func TestExportTokens(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hui.db")
	st, err := store.Open(target)
	if err != nil {
		t.Fatalf("建目标库失败: %v", err)
	}
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	keys := []string{"sk-first-token-aa", "sk-second-token-bb"}
	for i, key := range keys {
		if err := st.Write.Create(&model.Token{ID: int64(i + 1), UserID: int64(i + 1),
			Key: key, KeyHash: fmt.Sprintf("hash-%d", i)}).Error; err != nil {
			t.Fatalf("插令牌失败: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关闭目标库失败: %v", err)
	}

	out := filepath.Join(dir, "tokens.tsv")
	if err := exportTokens(target, out); err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("读导出文件失败: %v", err)
	}
	want := "id\tuser_id\tkey\n1\t1\tsk-first-token-aa\n2\t2\tsk-second-token-bb\n"
	if string(b) != want {
		t.Fatalf("导出内容错误:\n got  %q\n want %q", string(b), want)
	}
	if !strings.Contains(string(b), "sk-first-token-aa") {
		t.Fatal("明文 key 应在导出清单中")
	}
}
