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

// TestOpenMigrateIdempotent 验证迁移幂等：重复 Migrate 不报错且 schema_version 恒为当前版本。
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

// TestM3TablesCRUD M3-wave1 两张新表读写回环：user_identities 复合唯一约束、
// topup_orders order_no 唯一约束与状态/金额字段回读（schema v4，迁移 0004）。
func TestM3TablesCRUD(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().Unix()

	uid := &model.UserIdentity{
		UserID: 7, Provider: "github", ProviderUID: "10001", CreatedTime: now,
	}
	if err := st.Write.Create(uid).Error; err != nil {
		t.Fatalf("插入 user_identity 失败: %v", err)
	}
	var gotUID model.UserIdentity
	if err := st.Read.Where("provider = ? AND provider_uid = ?", "github", "10001").
		First(&gotUID).Error; err != nil {
		t.Fatalf("按复合键读取 user_identity 失败: %v", err)
	}
	if gotUID.UserID != 7 {
		t.Fatalf("user_identity 回读不一致: %+v", gotUID)
	}
	dupUID := &model.UserIdentity{UserID: 8, Provider: "github", ProviderUID: "10001", CreatedTime: now}
	if err := st.Write.Create(dupUID).Error; err == nil {
		t.Fatal("(provider,provider_uid) 复合唯一未生效：重复插入应报错")
	}
	otherProvider := &model.UserIdentity{UserID: 7, Provider: "linuxdo", ProviderUID: "10001", CreatedTime: now}
	if err := st.Write.Create(otherProvider).Error; err != nil {
		t.Fatalf("同 UID 不同 provider 应可共存: %v", err)
	}

	order := &model.TopupOrder{
		OrderNo: "T20260903ABCD0001", UserID: 7, Gateway: "epay",
		AmountCents: 1000, Currency: "CNY", Quota: 500000, Rate: 500000,
		Status: model.TopupOrderPending, TradeNo: "202609031200001",
		Detail: `{"ip":"127.0.0.1"}`, CreatedTime: now,
	}
	if err := st.Write.Create(order).Error; err != nil {
		t.Fatalf("插入 topup_order 失败: %v", err)
	}
	var gotOrder model.TopupOrder
	if err := st.Read.Where("order_no = ?", order.OrderNo).First(&gotOrder).Error; err != nil {
		t.Fatalf("按 order_no 读取失败: %v", err)
	}
	if gotOrder.Status != model.TopupOrderPending || gotOrder.AmountCents != 1000 || gotOrder.Currency != "CNY" {
		t.Fatalf("topup_order 回读不一致: %+v", gotOrder)
	}
	dupOrder := &model.TopupOrder{OrderNo: order.OrderNo, UserID: 7, CreatedTime: now}
	if err := st.Write.Create(dupOrder).Error; err == nil {
		t.Fatal("order_no 唯一索引未生效：重复插入应报错")
	}

	// 状态迁移语义：pending → paid（条件 UPDATE 恰一次，供 M3-wave2 回调复用）。
	res := st.Write.Model(&model.TopupOrder{}).
		Where("id = ? AND status = ?", order.ID, model.TopupOrderPending).
		Updates(map[string]any{"status": model.TopupOrderPaid, "paid_time": now})
	if res.Error != nil {
		t.Fatalf("订单状态迁移失败: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("pending → paid 应影响 1 行，实际 %d", res.RowsAffected)
	}
	res = st.Write.Model(&model.TopupOrder{}).
		Where("id = ? AND status = ?", order.ID, model.TopupOrderPending).
		Update("status", model.TopupOrderPaid)
	if res.Error != nil || res.RowsAffected != 0 {
		t.Fatalf("重复迁移应命中 0 行（防重复入账），实际 %d err=%v", res.RowsAffected, res.Error)
	}
}

// TestUserAffColumns M3-wave1 users 新列（邀请/TOTP）默认值与回读。
func TestUserAffColumns(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().Unix()
	u := &model.User{Username: "affuser", CreatedTime: now}
	if err := st.Write.Create(u).Error; err != nil {
		t.Fatalf("插入 user 失败: %v", err)
	}
	var got model.User
	if err := st.Read.First(&got, u.ID).Error; err != nil {
		t.Fatalf("读取 user 失败: %v", err)
	}
	if got.AffCode != "" || got.InviterID != 0 || got.AffHistoryQuota != 0 {
		t.Fatalf("邀请列默认值应为零值: %+v", got)
	}
	if got.TOTPSecret != "" || got.TOTPEnabled {
		t.Fatalf("TOTP 列默认值应为关闭: %+v", got)
	}
	got.AffCode = "INV01A2B"
	got.InviterID = 42
	got.AffHistoryQuota = 12345
	if err := st.Write.Save(&got).Error; err != nil {
		t.Fatalf("更新邀请列失败: %v", err)
	}
	var re model.User
	if err := st.Read.First(&re, u.ID).Error; err != nil {
		t.Fatalf("回读 user 失败: %v", err)
	}
	if re.AffCode != "INV01A2B" || re.InviterID != 42 || re.AffHistoryQuota != 12345 {
		t.Fatalf("邀请列回读不一致: %+v", re)
	}
}

// TestDDLEquivalence 双源一致性：migrations/*.sql 按文件名顺序执行建出的表，
// 与 AutoMigrate 建出的表列集合一致（0001 基线 + 后续 up-only 迁移叠加）。
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

	// 文档源：按文件名顺序执行 migrations 目录全部 .sql（0001 基线 + 后续迁移）。
	ddlSt, err := Open(ddlPath)
	if err != nil {
		t.Fatalf("打开 ddl 库失败: %v", err)
	}
	defer func() { _ = ddlSt.Close() }()
	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("读取 migrations 目录失败: %v", err)
	}
	var sqlFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			sqlFiles = append(sqlFiles, e.Name())
		}
	}
	sort.Strings(sqlFiles)
	if len(sqlFiles) == 0 {
		t.Fatal("migrations 目录无 SQL 迁移脚本")
	}
	for _, name := range sqlFiles {
		raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		for _, stmt := range splitSQLStatements(string(raw)) {
			if err := ddlSt.Write.Exec(stmt).Error; err != nil {
				t.Fatalf("执行 %s 语句失败: %v\n语句: %s", name, err, stmt)
			}
		}
	}

	// 对比全部表列名集合（含 M3-wave1 新增 user_identities/topup_orders）。
	tables := []string{
		"channels", "tokens", "users", "redemptions", "options", "logs",
		"user_identities", "topup_orders",
	}
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
