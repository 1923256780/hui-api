package store

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/model"
)

// openTestStore 在临时目录打开一个已迁移的 Store。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return st
}

// TestOpenMigrateIdempotent 验证迁移幂等：重复 Migrate 不报错且 schema_version 恒为 1。
func TestOpenMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idempotent.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	v, err := st.Migrate()
	if err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("期望 schema 版本 %d，实际 %d", SchemaVersion, v)
	}
	recorded, err := st.SchemaVersionRead()
	if err != nil {
		t.Fatalf("读取 schema 版本失败: %v", err)
	}
	if recorded != SchemaVersion {
		t.Fatalf("options 记录的版本期望 %d，实际 %d", SchemaVersion, recorded)
	}

	if _, err := st.Migrate(); err != nil {
		t.Fatalf("重复迁移失败（应幂等）: %v", err)
	}
	recorded, err = st.SchemaVersionRead()
	if err != nil {
		t.Fatalf("重复迁移后读取版本失败: %v", err)
	}
	if recorded != SchemaVersion {
		t.Fatalf("重复迁移后版本应仍为 %d，实际 %d", SchemaVersion, recorded)
	}
}

// TestWritePoolSingleConnection 固化连接模型：写池单连接，读池独立多连接。
func TestWritePoolSingleConnection(t *testing.T) {
	st := openTestStore(t)

	writeStats, err := st.Write.DB()
	if err != nil {
		t.Fatalf("获取写池失败: %v", err)
	}
	if got := writeStats.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("写池期望最大 1 连接，实际 %d", got)
	}

	readStats, err := st.Read.DB()
	if err != nil {
		t.Fatalf("获取读池失败: %v", err)
	}
	if got := readStats.Stats().MaxOpenConnections; got < 2 {
		t.Fatalf("读池应独立且支持并发（>1），实际 %d", got)
	}
	if readStats == writeStats {
		t.Fatal("读写应是两个独立的 sql.DB 连接池")
	}
}

// TestPragmas 验证每个连接建立时执行的 PRAGMA：WAL、NORMAL、busy_timeout=5000。
func TestPragmas(t *testing.T) {
	st := openTestStore(t)

	var journal string
	if err := st.Read.Raw("PRAGMA journal_mode").Scan(&journal).Error; err != nil {
		t.Fatalf("查询 journal_mode 失败: %v", err)
	}
	if !strings.EqualFold(journal, PragmaJournalMode) {
		t.Fatalf("期望 journal_mode=%s，实际 %s", PragmaJournalMode, journal)
	}

	var synchronous int
	if err := st.Read.Raw("PRAGMA synchronous").Scan(&synchronous).Error; err != nil {
		t.Fatalf("查询 synchronous 失败: %v", err)
	}
	// SQLite 数值语义：1 = NORMAL，2 = FULL。
	if synchronous != 1 {
		t.Fatalf("期望 synchronous=1(NORMAL)，实际 %d", synchronous)
	}

	var busyTimeout int
	if err := st.Write.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error; err != nil {
		t.Fatalf("查询 busy_timeout 失败: %v", err)
	}
	if busyTimeout != PragmaBusyTimeout {
		t.Fatalf("期望 busy_timeout=%d，实际 %d", PragmaBusyTimeout, busyTimeout)
	}
}

// TestSixTablesCRUD 六表读写回环：插入后经读池读回，断言关键字段语义。
func TestSixTablesCRUD(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().Unix()

	ch := &model.Channel{
		Name: "主渠道", Type: model.ChannelTypeOpenAICompatible,
		BaseURL: "https://example.internal/v1", Key: "upstream-key",
		Models: "model-a,model-b", Priority: 10, Weight: 5,
		Status: model.StatusEnabled, CreatedTime: now,
	}
	if err := st.Write.Create(ch).Error; err != nil {
		t.Fatalf("插入 channel 失败: %v", err)
	}
	var gotCh model.Channel
	if err := st.Read.First(&gotCh, ch.ID).Error; err != nil {
		t.Fatalf("读取 channel 失败: %v", err)
	}
	if gotCh.BaseURL != ch.BaseURL || gotCh.Priority != 10 {
		t.Fatalf("channel 字段回读不一致: %+v", gotCh)
	}

	tk := &model.Token{
		UserID: 1, Name: "测试令牌", KeyHash: "aaaa1111",
		Quota: 2 * model.QuotaPerDollar, RemainQuota: model.QuotaPerDollar,
		BudgetDuration: "24h", TPMRPM: `{"tpm":100,"rpm":10}`,
		Tags: `["team-a"]`, ExpiredTime: model.EpochForever, CreatedTime: now,
	}
	if err := st.Write.Create(tk).Error; err != nil {
		t.Fatalf("插入 token 失败: %v", err)
	}
	var gotTk model.Token
	if err := st.Read.First(&gotTk, tk.ID).Error; err != nil {
		t.Fatalf("读取 token 失败: %v", err)
	}
	if gotTk.ExpiredTime != model.EpochForever {
		t.Fatalf("expired_time 默认语义应为 -1，实际 %d", gotTk.ExpiredTime)
	}
	if gotTk.Quota != 1000000 {
		t.Fatalf("quota 换算应为 1000000（$2），实际 %d", gotTk.Quota)
	}

	dup := &model.Token{UserID: 1, KeyHash: tk.KeyHash, CreatedTime: now}
	if err := st.Write.Create(dup).Error; err == nil {
		t.Fatal("key_hash 唯一索引未生效：重复插入应报错")
	}

	u := &model.User{
		Username: "admin", Role: model.RoleAdmin,
		Quota: model.QuotaPerDollar, CreatedTime: now,
	}
	if err := st.Write.Create(u).Error; err != nil {
		t.Fatalf("插入 user 失败: %v", err)
	}
	var gotU model.User
	if err := st.Read.First(&gotU, u.ID).Error; err != nil {
		t.Fatalf("读取 user 失败: %v", err)
	}
	if gotU.Role != model.RoleAdmin {
		t.Fatalf("user role 回读不一致: %+v", gotU)
	}

	rd := &model.Redemption{
		Key: "r-deadbeef", Quota: model.QuotaPerDollar,
		Status: model.RedemptionUnused, CreatedBy: u.ID,
		ExpiredTime: model.EpochForever, CreatedTime: now,
	}
	if err := st.Write.Create(rd).Error; err != nil {
		t.Fatalf("插入 redemption 失败: %v", err)
	}

	lg := &model.Log{
		UserID: u.ID, TokenID: tk.ID, ChannelID: ch.ID,
		Protocol: "openai", ModelName: "model-a",
		PromptTokens: 100, CompletionTokens: 50, Quota: 1234,
		UseTime: 2, IsStream: true, Detail: `{"mode":"classic_ratio"}`,
		CreatedTime: now,
	}
	if err := st.Write.Create(lg).Error; err != nil {
		t.Fatalf("插入 log 失败: %v", err)
	}
	var gotLg model.Log
	if err := st.Read.First(&gotLg, lg.ID).Error; err != nil {
		t.Fatalf("读取 log 失败: %v", err)
	}
	if gotLg.ModelName != "model-a" || gotLg.Quota != 1234 {
		t.Fatalf("log 字段回读不一致: %+v", gotLg)
	}

	// options 仓储语义。
	if err := st.SetOption("display.root", "Hui Api"); err != nil {
		t.Fatalf("SetOption 失败: %v", err)
	}
	if err := st.SetOption("display.root", "Hui Api v2"); err != nil {
		t.Fatalf("SetOption 覆盖失败: %v", err)
	}
	values, err := st.GetAllOptions()
	if err != nil {
		t.Fatalf("GetAllOptions 失败: %v", err)
	}
	if values["display.root"] != "Hui Api v2" {
		t.Fatalf("option 覆盖后期望 Hui Api v2，实际 %q", values["display.root"])
	}
	if err := st.DeleteOption("display.root"); err != nil {
		t.Fatalf("DeleteOption 失败: %v", err)
	}
	if err := st.DeleteOption("display.root"); err != nil {
		t.Fatalf("DeleteOption 幂等失败: %v", err)
	}
}

// TestDDLEquivalence 双源一致性：0001_init.sql 建出的表与 AutoMigrate 建出的表列集合一致。
func TestDDLEquivalence(t *testing.T) {
	dir := t.TempDir()
	autoPath := filepath.Join(dir, "auto.db")
	ddlPath := filepath.Join(dir, "ddl.db")

	// 代码源：AutoMigrate。
	autoSt, err := Open(autoPath)
	if err != nil {
		t.Fatalf("打开 auto 库失败: %v", err)
	}
	defer func() { _ = autoSt.Close() }()
	if _, err := autoSt.Migrate(); err != nil {
		t.Fatalf("auto 库迁移失败: %v", err)
	}

	// 文档源：直接执行 0001_init.sql。
	ddlSt, err := Open(ddlPath)
	if err != nil {
		t.Fatalf("打开 ddl 库失败: %v", err)
	}
	defer func() { _ = ddlSt.Close() }()
	ddlBytes, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatalf("读取 0001_init.sql 失败: %v", err)
	}
	for _, stmt := range splitSQLStatements(string(ddlBytes)) {
		if err := ddlSt.Write.Exec(stmt).Error; err != nil {
			t.Fatalf("执行 DDL 语句失败: %v\n语句: %s", err, stmt)
		}
	}

	// 对比六表列名集合。
	tables := []string{"channels", "tokens", "users", "redemptions", "options", "logs"}
	for _, table := range tables {
		autoCols, err := tableColumns(autoSt.Read, table)
		if err != nil {
			t.Fatalf("读取 %s 列失败(auto): %v", table, err)
		}
		ddlCols, err := tableColumns(ddlSt.Read, table)
		if err != nil {
			t.Fatalf("读取 %s 列失败(ddl): %v", table, err)
		}
		if len(autoCols) != len(ddlCols) {
			t.Fatalf("表 %s 列数不一致: auto=%v ddl=%v", table, autoCols, ddlCols)
		}
		for i := range autoCols {
			if autoCols[i] != ddlCols[i] {
				t.Fatalf("表 %s 列不一致: auto=%v ddl=%v", table, autoCols, ddlCols)
			}
		}
	}
}

// splitSQLStatements 把 SQL 文本拆成可逐条执行的语句（去注释、按分号切分）。
func splitSQLStatements(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		lines = append(lines, line)
	}
	cleaned := strings.Join(lines, "\n")
	var stmts []string
	for _, part := range strings.Split(cleaned, ";") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		stmts = append(stmts, strings.TrimSpace(part))
	}
	return stmts
}

// tableColumns 返回表的列名列表（PRAGMA table_info，按 cid 排序）。
func tableColumns(db *gorm.DB, table string) ([]string, error) {
	type colInfo struct {
		CID  int
		Name string
	}
	var cols []colInfo
	if err := db.Raw("PRAGMA table_info(`" + table + "`)").Scan(&cols).Error; err != nil {
		return nil, err
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].CID < cols[j].CID })
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names, nil
}
