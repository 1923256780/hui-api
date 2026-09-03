// 迁移引擎集成测试：raw SQL 造旧库 fixture（最小旧库 schema）→ Run 迁移 →
// 断言报告/对账/目标库内容/幂等重跑确定性/守卫拒绝/行失败对账语义。
package migrate

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// legacyFixtureSpec 控制 fixture 数据的变体。
type legacyFixtureSpec struct {
	quotaPerUnit    string // 非空则落库 QuotaPerUnit 键（守卫测试用）
	breakUserPasswd bool   // true 则第二个用户密码为明文（bcrypt 守卫行失败测试用）
}

// buildLegacyFixture 用 raw SQL 建一个最小旧库（列集与真实旧库对齐的子集）。
func buildLegacyFixture(t *testing.T, path string, spec legacyFixtureSpec) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("建 fixture 库失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	ddls := []string{
		`CREATE TABLE users (id integer PRIMARY KEY, username text, password text, display_name text,
			role int, status int, email text, quota numeric, used_quota numeric, "group" text,
			created_at numeric, last_login_at numeric)`,
		`CREATE TABLE tokens (id integer PRIMARY KEY, user_id int, key text, status int, name text,
			created_time numeric, accessed_time numeric, expired_time numeric, remain_quota numeric,
			unlimited_quota numeric, model_limits_enabled numeric, model_limits text, allow_ips text, "group" text)`,
		`CREATE TABLE channels (id integer PRIMARY KEY, type int, key text, status int, name text,
			weight numeric, created_time numeric, base_url text, models text, "group" text,
			model_mapping text, priority numeric, param_override text)`,
		`CREATE TABLE options (key text PRIMARY KEY, value text)`,
		`CREATE TABLE redemptions (id integer PRIMARY KEY, user_id numeric, key text, status int,
			name text, quota numeric, created_time numeric, redeemed_time numeric,
			used_user_id numeric, expired_time numeric)`,
		`CREATE TABLE logs (id integer PRIMARY KEY, user_id numeric, created_at numeric, type int,
			content text, token_name text, model_name text, quota numeric, prompt_tokens numeric,
			completion_tokens numeric, use_time numeric, is_stream numeric, channel_id numeric,
			token_id numeric, other text)`,
	}
	for _, ddl := range ddls {
		if err := st.Write.Exec(ddl).Error; err != nil {
			t.Fatalf("建 fixture 表失败: %v\n%s", err, ddl)
		}
	}

	ins := func(q string, args ...any) {
		t.Helper()
		if err := st.Write.Exec(q, args...).Error; err != nil {
			t.Fatalf("插 fixture 数据失败: %v\n%s %v", err, q, args)
		}
	}

	// users：root（合法）+ alice（合法或按 spec 弄坏）。
	pass1 := "$2a$10$" + strings.Repeat("a", 53)
	pass2 := pass1
	if spec.breakUserPasswd {
		pass2 = "plaintext-not-bcrypt"
	}
	ins(`INSERT INTO users VALUES (1,'root',?,'管理员',100,1,'root@example.com',1000,100,'',1788160000,1788340000)`, pass1)
	ins(`INSERT INTO users VALUES (2,'alice',?,'',1,1,'',500,0,'vip',1788160100,0)`, pass2)

	// tokens：t1 无限/无组；t2 无限（组继承 vip）；t3 非无限预算快照/禁用。
	ins(`INSERT INTO tokens VALUES (1,1,?,1,'k1',1788158937,1788158937,-1,0,1,0,'','','')`, strings.Repeat("a", 48))
	ins(`INSERT INTO tokens VALUES (2,2,?,1,'k2',1788158937,1788158937,-1,0,1,0,'','','')`, strings.Repeat("b", 48))
	ins(`INSERT INTO tokens VALUES (3,1,?,2,'k3',1788158937,1788158937,-1,600,0,0,'','','')`, strings.Repeat("c", 48))

	// channels：ch1 启用带 envelope；ch2 DeepSeek 型（空 base_url + model_mapping 缺口）；
	// ch3 方舟型多组。启用渠道模型最小集 = {model-a, model-b, model-missing}。
	ins(`INSERT INTO channels VALUES (1,1,'sk-up1',1,'main',0,1788160841,'https://up.example/v1',
		'model-a,model-b,model-missing','default','',3,?)`, realEnvelope)
	ins(`INSERT INTO channels VALUES (2,43,'sk-up2',2,'ds',0,1788160841,'','model-a','default',
		'{"model-a":"upstream-a"}',0,'')`)
	ins(`INSERT INTO channels VALUES (3,14,'sk-up3',1,'ark',0,1788160841,'https://ark.example/v1',
		'model-a','default,vip,trial','',5,'')`)

	// options：白名单 3 键（billing_mode/expr 旧库未落 → 跳过）+ 换皮键 2 个。
	ins(`INSERT INTO options VALUES ('ModelRatio','{"model-a":0.5,"model-b":0.25}')`)
	ins(`INSERT INTO options VALUES ('CompletionRatio','{"model-a":2}')`)
	ins(`INSERT INTO options VALUES ('GroupRatio','{"default":1,"vip":0.8}')`)
	ins(`INSERT INTO options VALUES ('SystemName','TestSite')`)
	ins(`INSERT INTO options VALUES ('HomePageContent','<html>很长</html>')`)
	if spec.quotaPerUnit != "" {
		ins(`INSERT INTO options VALUES ('QuotaPerUnit',?)`, spec.quotaPerUnit)
	}

	// redemptions：r1 已核销（面值 730000，topup 日志关联它）；r2 未核销未过期。
	now := time.Now().Unix()
	ins(`INSERT INTO redemptions VALUES (1,1,'red-key-1',3,'rc1',730000,1788259604,1788319022,1,0)`)
	ins(`INSERT INTO redemptions VALUES (2,1,'red-key-2',1,'rc2',100000,1788259604,0,0,?)`, now+31536000)

	// logs：consume 3 行（detail 保留/合法/超长置空）+ topup 2 行（关联/未关联）+ 未迁类型 1 行。
	ins(`INSERT INTO logs VALUES (11,1,1788319000,2,'','k1','model-a',10,100,10,3,1,1,1,'{"model_ratio":0.5}')`)
	ins(`INSERT INTO logs VALUES (12,2,1788319001,2,'','k2','model-a',20,200,20,4,0,1,2,'{"a":1}')`)
	ins(`INSERT INTO logs VALUES (13,1,1788319002,2,'','k1','model-a',30,300,30,5,1,1,1,'{"k":"' || ? || '"}')`,
		strings.Repeat("x", 3000))
	ins(`INSERT INTO logs VALUES (21,1,1788319022,1,'通过兑换码充值 $1.460000 额度，兑换码ID 1','', '',0,0,0,0,0,0,0,'')`)
	ins(`INSERT INTO logs VALUES (22,1,1788319023,1,'兑换码ID 99','', '',0,0,0,0,0,0,0,'')`)
	ins(`INSERT INTO logs VALUES (31,1,1788319024,3,'系统日志','', '',0,0,0,0,0,0,0,'')`)
}

// dumpTarget 输出目标库确定性快照（每表按主键排序的拼接串），用于幂等比对。
func dumpTarget(t *testing.T, st *store.Store) []string {
	t.Helper()
	queries := []string{
		`SELECT id||'|'||username||'|'||password_hash||'|'||role||'|'||status||'|'||quota||'|'||used_quota||'|'||"group" FROM users ORDER BY id`,
		`SELECT id||'|'||user_id||'|'||key_hash||'|'||status||'|'||quota||'|'||remain_quota||'|'||unlimited_quota||'|'||"group" FROM tokens ORDER BY id`,
		`SELECT id||'|'||type||'|'||base_url||'|'||status||'|'||priority||'|'||param_override||'|'||models FROM channels ORDER BY id`,
		`SELECT key||'='||value FROM options ORDER BY key`,
		`SELECT id||'|'||key||'|'||status||'|'||quota||'|'||used_by||'|'||expired_time FROM redemptions ORDER BY id`,
		`SELECT id||'|'||user_id||'|'||protocol||'|'||model_name||'|'||quota||'|'||prompt_tokens||'|'||completion_tokens||'|'||detail FROM logs ORDER BY id`,
	}
	var out []string
	for _, q := range queries {
		var rows []string
		if err := st.Read.Raw(q).Scan(&rows).Error; err != nil {
			t.Fatalf("dump 查询失败: %v\n%s", err, q)
		}
		out = append(out, rows...)
	}
	return out
}

// runMigrate 对 fixture 跑一次迁移并返回报告。
func runMigrate(t *testing.T, legacyPath, targetPath string) (*Report, error) {
	t.Helper()
	return Run(Options{LegacyPath: legacyPath, TargetPath: targetPath})
}

// TestRunMigratesAndReconciles 主链路：干净 fixture → 迁移成功、对账一致、内容正确。
func TestRunMigratesAndReconciles(t *testing.T) {
	dir := t.TempDir()
	legacyPath, targetPath := dir+"/legacy.db", dir+"/hui.db"
	buildLegacyFixture(t, legacyPath, legacyFixtureSpec{})

	rep, err := runMigrate(t, legacyPath, targetPath)
	if err != nil {
		t.Fatalf("迁移失败: %v\n报告: %+v", err, rep)
	}
	if !rep.OK {
		t.Fatalf("报告应 OK: %+v", rep)
	}
	if !rep.Reconciliation.Match {
		t.Fatalf("对账应一致: %+v", rep.Reconciliation.Items)
	}

	// 计数断言。
	if rep.Users.Read != 2 || rep.Users.Migrated != 2 || rep.Users.Failed != 0 {
		t.Fatalf("users 计数错误: %+v", rep.Users)
	}
	if rep.Tokens.Read != 3 || rep.Tokens.Migrated != 3 {
		t.Fatalf("tokens 计数错误: %+v", rep.Tokens)
	}
	if rep.Channels.Read != 3 || rep.Channels.Migrated != 3 || rep.Channels.GroupDropped != 3 {
		t.Fatalf("channels 计数错误: %+v", rep.Channels)
	}
	if len(rep.Channels.ModelMappingGaps) != 1 || rep.Channels.ModelMappingGaps[0].ID != 2 {
		t.Fatalf("model_mapping 缺口应 1 条（ch2）: %+v", rep.Channels.ModelMappingGaps)
	}
	if rep.Redemptions.Migrated != 2 {
		t.Fatalf("redemptions 计数错误: %+v", rep.Redemptions)
	}
	if rep.Logs.ConsumeRead != 3 || rep.Logs.ConsumeMigrated != 3 || rep.Logs.ConsumeFailed != 0 {
		t.Fatalf("consume 日志计数错误: %+v", rep.Logs)
	}
	if rep.Logs.TopupSynthesized != 2 || rep.Logs.TopupAmountLinked != 1 {
		t.Fatalf("topup 合成计数错误: %+v", rep.Logs)
	}
	if rep.Logs.DetailKept != 2 || rep.Logs.DetailDropped != 1 {
		t.Fatalf("detail 处置计数错误: %+v", rep.Logs)
	}
	if len(rep.Logs.SkippedByType) != 1 || rep.Logs.SkippedByType[0].Type != 3 || rep.Logs.SkippedByType[0].Count != 1 {
		t.Fatalf("未迁类型应仅 type=3×1: %+v", rep.Logs.SkippedByType)
	}

	// options：3 白名单键迁移、2 换皮键跳过、ModelRatio 换算（2 迁 1 缺）。
	wantKeys := "CompletionRatio,GroupRatio,ModelRatio"
	if strings.Join(rep.Options.MigratedKeys, ",") != wantKeys {
		t.Fatalf("迁移键应 %s，实际 %v", wantKeys, rep.Options.MigratedKeys)
	}
	if len(rep.Options.Skipped) != 2 {
		t.Fatalf("未迁清单应 2 项: %+v", rep.Options.Skipped)
	}
	if rep.Options.ModelRatio.SourceModels != 2 ||
		strings.Join(rep.Options.ModelRatio.MigratedModels, ",") != "model-a,model-b" ||
		strings.Join(rep.Options.ModelRatio.MissingModels, ",") != "model-missing" {
		t.Fatalf("ModelRatio 报告错误: %+v", rep.Options.ModelRatio)
	}
	if rep.Options.ModelRatio.Conversion != "legacy×2" {
		t.Fatalf("换算口径标注错误: %q", rep.Options.ModelRatio.Conversion)
	}

	// 守卫：QuotaPerUnit 未落库按缺省放行；空库起点守卫决策 empty。
	if !rep.Guard.QuotaPerUnitIsDefault {
		t.Fatalf("fixture 未落 QuotaPerUnit，应为缺省放行: %+v", rep.Guard)
	}
	if rep.Guard.TargetState != targetStateEmpty || rep.Guard.TargetForced {
		t.Fatalf("空库起点守卫决策应为 empty/未强制: %+v", rep.Guard)
	}

	// 目标库内容断言。
	tgt, err := store.Open(targetPath)
	if err != nil {
		t.Fatalf("打开目标库失败: %v", err)
	}
	defer func() { _ = tgt.Close() }()

	// users：quota 余额与组。
	var u model.User
	if err := tgt.Read.Where("username = 'alice'").First(&u).Error; err != nil {
		t.Fatalf("alice 应已迁入: %v", err)
	}
	if u.Group != "vip" || u.Quota != 500 {
		t.Fatalf("alice 迁移结果错误: %+v", u)
	}
	// tokens：key_hash 口径 + 组继承 + 预算快照 + 禁用态。
	var tok model.Token
	if err := tgt.Read.Where("id = 2").First(&tok).Error; err != nil {
		t.Fatalf("token 2 应已迁入: %v", err)
	}
	if tok.KeyHash != gateway.HashKey(TokenPlainPrefix+strings.Repeat("b", 48)) {
		t.Fatal("key_hash 必须等于 gateway.HashKey(sk-+48 位)")
	}
	if tok.Group != "vip" {
		t.Fatalf("token 2 组应继承 alice 的 vip，实际 %q", tok.Group)
	}
	tok = model.Token{} // 重置零值：GORM First 会把非零主键作为隐式条件
	if err := tgt.Read.Where("id = 3").First(&tok).Error; err != nil {
		t.Fatalf("token 3 应已迁入: %v", err)
	}
	if tok.Status != model.StatusDisabled || tok.Quota != 600 || tok.RemainQuota != 600 || tok.UnlimitedQuota {
		t.Fatalf("token 3 预算快照/禁用态错误: %+v", tok)
	}
	// channels：type 归一 + base_url 回填 + 扁平 ops。
	var ch model.Channel
	if err := tgt.Read.Where("id = 2").First(&ch).Error; err != nil {
		t.Fatalf("channel 2 应已迁入: %v", err)
	}
	if ch.Type != model.ChannelTypeOpenAICompatible || ch.BaseURL != "https://api.deepseek.com" ||
		ch.Status != model.StatusDisabled {
		t.Fatalf("channel 2 映射错误: %+v", ch)
	}
	ch = model.Channel{} // 重置零值：GORM First 会把非零主键作为隐式条件
	if err := tgt.Read.Where("id = 1").First(&ch).Error; err != nil {
		t.Fatalf("channel 1 应已迁入: %v", err)
	}
	if ch.ParamOverride != realEnvelopeFlat() {
		t.Fatalf("ch1 param_override 应为实测 envelope 的扁平 ops:\n got  %s\n want %s", ch.ParamOverride, realEnvelopeFlat())
	}
	// options：换算后的 ModelRatio。
	var opt model.Option
	if err := tgt.Read.Where("key = 'ModelRatio'").First(&opt).Error; err != nil {
		t.Fatalf("ModelRatio 应已迁入: %v", err)
	}
	if opt.Value != `{"model-a":1,"model-b":0.5}` {
		t.Fatalf("ModelRatio 换算结果错误: %s", opt.Value)
	}
	if err := tgt.Read.Where("key = 'SystemName'").First(&opt).Error; err == nil {
		t.Fatal("换皮键 SystemName 不应迁入")
	}
	// logs：protocol 语义 + topup 合成面值。
	var lg model.Log
	if err := tgt.Read.Where("id = 21").First(&lg).Error; err != nil {
		t.Fatalf("topup 日志应已合成: %v", err)
	}
	if lg.Protocol != "topup" || lg.Quota != 730000 || lg.ModelName != "redemption" {
		t.Fatalf("topup 日志形状错误: %+v", lg)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(lg.Detail), &d); err != nil || d["ref_id"] != "legacy-redemption-1" {
		t.Fatalf("topup detail 应关联 r1: %v %v", err, d)
	}
	lg = model.Log{} // 重置零值：GORM First 会把非零主键作为隐式条件
	if err := tgt.Read.Where("id = 12").First(&lg).Error; err != nil {
		t.Fatalf("consume 日志应已迁入: %v", err)
	}
	if lg.Protocol != "openai" || lg.Detail != `{"a":1}` {
		t.Fatalf("consume 日志 detail 保留错误: %+v", lg)
	}
	// redemptions：已核销映射。
	var rd model.Redemption
	if err := tgt.Read.Where("id = 1").First(&rd).Error; err != nil {
		t.Fatalf("redemption 1 应已迁入: %v", err)
	}
	if rd.Status != model.RedemptionRedeemed || rd.ExpiredTime != model.EpochForever || rd.UsedBy != 1 {
		t.Fatalf("redemption 1 映射错误: %+v", rd)
	}
}

// realEnvelopeFlat 是实测 envelope 经 TransformParamOverride 的期望产物。
func realEnvelopeFlat() string {
	got, err := TransformParamOverride(realEnvelope)
	if err != nil {
		panic(err)
	}
	return got
}

// TestRunIdempotentRerun 幂等可重跑：重置目标库（删除重建空库，对齐 runbook
// 切换步骤的重置语义）后第二遍迁移，目标库快照与报告 JSON 逐字节一致。
// 同库不重置的重跑由目标库状态守卫拒绝（见 TestRunLiveTargetRejected），
// 幂等覆盖式同步语义建立在空库起点之上。
func TestRunIdempotentRerun(t *testing.T) {
	dir := t.TempDir()
	legacyPath, targetPath := dir+"/legacy.db", dir+"/hui.db"
	buildLegacyFixture(t, legacyPath, legacyFixtureSpec{})

	rep1, err := runMigrate(t, legacyPath, targetPath)
	if err != nil {
		t.Fatalf("第一遍迁移失败: %v", err)
	}
	st1 := mustOpen(t, targetPath)
	snap1 := dumpTarget(t, st1)
	if err := st1.Close(); err != nil {
		t.Fatalf("关闭目标库失败: %v", err)
	}
	b1, err := json.Marshal(rep1)
	if err != nil {
		t.Fatalf("报告序列化失败: %v", err)
	}

	// 重置目标库：删除重建空库（WAL 模式连同 -wal/-shm 一并清理）。
	resetTarget(t, targetPath)

	rep2, err := runMigrate(t, legacyPath, targetPath)
	if err != nil {
		t.Fatalf("第二遍迁移失败: %v", err)
	}
	st2 := mustOpen(t, targetPath)
	snap2 := dumpTarget(t, st2)
	if err := st2.Close(); err != nil {
		t.Fatalf("关闭目标库失败: %v", err)
	}
	b2, err := json.Marshal(rep2)
	if err != nil {
		t.Fatalf("报告序列化失败: %v", err)
	}

	if strings.Join(snap1, "\n") != strings.Join(snap2, "\n") {
		t.Fatalf("两遍迁移后目标库快照不一致:\n--- 第一遍 ---\n%s\n--- 第二遍 ---\n%s",
			strings.Join(snap1, "\n"), strings.Join(snap2, "\n"))
	}
	if string(b1) != string(b2) {
		t.Fatalf("两遍迁移报告应逐字节一致（确定性）:\n--- 第一遍 ---\n%s\n--- 第二遍 ---\n%s", b1, b2)
	}
}

// resetTarget 重置目标库：删除库文件及 WAL 附属文件，下次打开即重建空库。
func resetTarget(t *testing.T, path string) {
	t.Helper()
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			t.Fatalf("重置目标库失败（%s）: %v", f, err)
		}
	}
}

// TestRunLiveTargetRejected 目标库状态守卫：同库重跑（users/tokens 已有数据）
// 拒绝（ErrLiveTarget），报告 guard 决策为 has_data/未强制，且既有数据零变化
// （守卫在任何 upsert 之前）。
func TestRunLiveTargetRejected(t *testing.T) {
	dir := t.TempDir()
	legacyPath, targetPath := dir+"/legacy.db", dir+"/hui.db"
	buildLegacyFixture(t, legacyPath, legacyFixtureSpec{})

	if _, err := runMigrate(t, legacyPath, targetPath); err != nil {
		t.Fatalf("首次迁移（空库起点）应放行: %v", err)
	}
	st1 := mustOpen(t, targetPath)
	snap := dumpTarget(t, st1)
	if err := st1.Close(); err != nil {
		t.Fatalf("关闭目标库失败: %v", err)
	}

	rep, err := runMigrate(t, legacyPath, targetPath)
	if !errors.Is(err, ErrLiveTarget) {
		t.Fatalf("非空目标库重跑应拒绝（ErrLiveTarget），实际 err=%v", err)
	}
	if rep == nil || rep.OK {
		t.Fatalf("拒绝时应返回非 OK 报告供人工排查: %+v", rep)
	}
	if rep.Guard.TargetState != targetStateHasData || rep.Guard.TargetForced {
		t.Fatalf("guard 决策应为 has_data/未强制: %+v", rep.Guard)
	}
	if !strings.Contains(err.Error(), "-allow-live-target") {
		t.Fatalf("错误信息应提示放行途径: %v", err)
	}

	// 守卫拒绝后目标库数据零变化。
	st2 := mustOpen(t, targetPath)
	defer func() { _ = st2.Close() }()
	snap2 := dumpTarget(t, st2)
	if strings.Join(snap, "\n") != strings.Join(snap2, "\n") {
		t.Fatal("守卫拒绝后目标库数据不应有任何变化")
	}
}

// TestRunLiveTargetForced -allow-live-target 显式放行：非空目标库强制重跑成功
// （对齐覆盖式同步语义），guard 决策为 has_data+forced。
func TestRunLiveTargetForced(t *testing.T) {
	dir := t.TempDir()
	legacyPath, targetPath := dir+"/legacy.db", dir+"/hui.db"
	buildLegacyFixture(t, legacyPath, legacyFixtureSpec{})

	if _, err := runMigrate(t, legacyPath, targetPath); err != nil {
		t.Fatalf("首次迁移应放行: %v", err)
	}
	rep, err := Run(Options{LegacyPath: legacyPath, TargetPath: targetPath, AllowLiveTarget: true})
	if err != nil {
		t.Fatalf("显式放行应成功: %v", err)
	}
	if !rep.OK {
		t.Fatalf("强制重跑应 OK: %+v", rep)
	}
	if rep.Guard.TargetState != targetStateHasData || !rep.Guard.TargetForced {
		t.Fatalf("guard 决策应为 has_data+forced: %+v", rep.Guard)
	}
}

// TestRunGuardRejectsWrongQuotaPerUnit 守卫：QuotaPerUnit≠500000 拒跑且不写目标库。
func TestRunGuardRejectsWrongQuotaPerUnit(t *testing.T) {
	dir := t.TempDir()
	legacyPath, targetPath := dir+"/legacy.db", dir+"/hui.db"
	buildLegacyFixture(t, legacyPath, legacyFixtureSpec{quotaPerUnit: "600000"})

	rep, err := runMigrate(t, legacyPath, targetPath)
	if err == nil {
		t.Fatalf("QuotaPerUnit=600000 应拒绝迁移: %+v", rep)
	}
	if rep == nil || rep.Guard.QuotaPerUnit != 600000 {
		t.Fatalf("守卫报告应记录 600000: %+v", rep)
	}
	if rep.OK {
		t.Fatal("守卫失败时 OK 应为 false")
	}
	// 目标库未写入任何数据（守卫在全部写入之前）。
	tgt := mustOpen(t, targetPath)
	defer func() { _ = tgt.Close() }()
	var n int64
	if err := tgt.Read.Model(&model.User{}).Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("守卫失败后目标库不应有用户数据: n=%d err=%v", n, err)
	}
	if err := tgt.Read.Model(&model.Option{}).Where("key <> 'schema_version'").Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("守卫失败后目标库不应有业务 options: n=%d err=%v", n, err)
	}
}

// TestRunRowFailureBreaksReconciliation 行失败语义：bcrypt 不合规行硬失败并计入报告，
// 其余行照常迁移，行数对账不一致 → ErrReconMismatch。
func TestRunRowFailureBreaksReconciliation(t *testing.T) {
	dir := t.TempDir()
	legacyPath, targetPath := dir+"/legacy.db", dir+"/hui.db"
	buildLegacyFixture(t, legacyPath, legacyFixtureSpec{breakUserPasswd: true})

	rep, err := runMigrate(t, legacyPath, targetPath)
	if !errors.Is(err, ErrReconMismatch) {
		t.Fatalf("行失败应导致对账失败（ErrReconMismatch），实际 err=%v", err)
	}
	if rep.OK {
		t.Fatal("报告不应 OK")
	}
	if rep.Users.Failed != 1 || len(rep.Users.Failures) != 1 || rep.Users.Failures[0].ID != 2 {
		t.Fatalf("失败行应记入报告: %+v", rep.Users)
	}
	if !strings.Contains(rep.Users.Failures[0].Reason, "bcrypt") {
		t.Fatalf("失败原因应说明 bcrypt: %q", rep.Users.Failures[0].Reason)
	}
	if rep.Users.Migrated != 1 {
		t.Fatalf("其余行应照常迁移: %+v", rep.Users)
	}
	// 对账差异：users.rows legacy=2 target=1。
	found := false
	for _, it := range rep.Reconciliation.Items {
		if it.Table == "users" && it.Metric == "rows" {
			found = true
			if it.Match || it.Legacy != 2 || it.Target != 1 {
				t.Fatalf("users.rows 对账项应 2≠1: %+v", it)
			}
		}
	}
	if !found {
		t.Fatal("对账应包含 users.rows 条目")
	}
}

// TestRunLegacyMissingFile 旧库不存在：只读打开失败（mode=ro 不创建文件）。
func TestRunLegacyMissingFile(t *testing.T) {
	dir := t.TempDir()
	rep, err := runMigrate(t, dir+"/no-such.db", dir+"/hui.db")
	if err == nil {
		t.Fatalf("旧库不存在应失败: %+v", rep)
	}
	if _, statErr := statFile(dir + "/no-such.db"); statErr == nil {
		t.Fatal("只读打开失败不应创建旧库文件")
	}
}

func mustOpen(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	return st
}

func statFile(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
