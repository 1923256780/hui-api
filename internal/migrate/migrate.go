// 迁移引擎：编排 只读读源 → 变换 → 幂等 upsert → 内置对账 → 报告。
//
// 幂等语义（ADR-0008）：全部写入按自然键 upsert（ON CONFLICT DO UPDATE 覆盖式同步，
// 只覆盖迁移归属列，目标库其他列保留），重复执行结果一致。冲突目标：
// users.username / tokens.key_hash / channels.id（旧库无自然键）/ redemptions.key /
// logs.id（旧库主键锚点，保证 token_id/channel_id 关联一致）/ options.key。
// 自然键唯一约束冲突且 id 不同的脏数据由 SQLite 约束显式报错拒绝（宁可拒绝不可错账）。
//
// 对账语义：迁移完成后逐表比对 行数 / SUM(quota) / SUM(used_quota)（旧侧只读 vs 新侧），
// 任一指标不一致即返回 ErrReconMismatch（报告随返回值给出差异明细），调用方应 exit 1。
// 前提：目标库为尚未上线的库（一次性切换工具），logs 对账以 protocol 语义子集进行。
package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/1923256780/hui-api/internal/billing"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// ErrReconMismatch 是内置对账不一致的哨兵错误（可用 errors.Is 判断）。
var ErrReconMismatch = errors.New("内置对账不一致")

// legacyQuotaPerUnitDefault 是旧网关 QuotaPerUnit 的缺省值（未落库时），与 hui 口径一致。
const legacyQuotaPerUnitDefault = int64(model.QuotaPerDollar)

// 旧库日志类型（docs/07：仅迁移 2=consume 全量与 1=充值合成，其余类型计数入报告）。
const (
	legacyLogTypeTopup   = 1
	legacyLogTypeConsume = 2
)

// upsert 分块大小：单事务行数上限，防超长事务锁库。
const (
	upsertBatchRows = 200
	logBatchRows    = 500
	// modelMappingSummaryMax 是报告里 model_mapping 原文摘要的最大长度。
	modelMappingSummaryMax = 120
)

// Options 是迁移运行参数。
type Options struct {
	LegacyPath string // 旧库路径（只读打开，绝不写入）
	TargetPath string // hui 库路径（读写打开；空库自动建表）
}

// Run 执行完整迁移并返回报告。语义：
//   - 返回 (rep, nil)：迁移完成且对账一致（rep.OK=true）；
//   - 返回 (rep, err)：流程中断（守卫失败/写入失败/对账不一致）。
//     对账不一致时 err 可用 errors.Is(err, ErrReconMismatch) 判断，rep 含差异明细；
//     守卫失败时 rep.Guard 已填充。两种情况调用方都应 exit 非零并输出报告。
func Run(opts Options) (*Report, error) {
	rep := &Report{LegacyPath: opts.LegacyPath, TargetPath: opts.TargetPath}

	ro, err := store.OpenReadOnly(opts.LegacyPath)
	if err != nil {
		return nil, fmt.Errorf("只读打开旧库失败: %w", err)
	}
	defer func() { _ = store.CloseReadOnly(ro) }()

	tgt, err := store.Open(opts.TargetPath)
	if err != nil {
		return nil, fmt.Errorf("打开目标库失败: %w", err)
	}
	defer func() { _ = tgt.Close() }()

	// 目标库 schema 就绪：空库自动建表；已有库幂等校验（GORM AutoMigrate 语义）。
	if _, err := tgt.Migrate(); err != nil {
		return nil, fmt.Errorf("目标库 schema 迁移失败: %w", err)
	}

	if err := runGuard(ro, rep); err != nil {
		return rep, err
	}

	users, err := migrateUsers(ro, tgt, rep)
	if err != nil {
		return rep, err
	}
	if err := migrateTokens(ro, tgt, rep, users); err != nil {
		return rep, err
	}
	channels, err := migrateChannels(ro, tgt, rep)
	if err != nil {
		return rep, err
	}
	expect, err := migrateOptions(ro, tgt, rep, channels)
	if err != nil {
		return rep, err
	}
	redemptionQuota, err := migrateRedemptions(ro, tgt, rep)
	if err != nil {
		return rep, err
	}
	if err := migrateLogs(ro, tgt, rep, redemptionQuota); err != nil {
		return rep, err
	}
	if err := reconcile(ro, tgt, rep, expect); err != nil {
		return rep, err
	}

	rep.OK = rep.Reconciliation.Match
	if !rep.OK {
		return rep, fmt.Errorf("%w：%d 项指标不一致（差异明细见报告 reconciliation.items）",
			ErrReconMismatch, countMismatch(rep.Reconciliation.Items))
	}
	return rep, nil
}

// runGuard 前置守卫：旧库 QuotaPerUnit 未落库按缺省 500000 放行；显式配置必须等于
// 500000，否则拒绝迁移（计费换算锚定该口径，漂移即错账）。
func runGuard(ro *gorm.DB, rep *Report) error {
	qpu, isDefault, err := readLegacyQuotaPerUnit(ro)
	if err != nil {
		return err
	}
	rep.Guard = GuardReport{QuotaPerUnit: qpu, QuotaPerUnitIsDefault: isDefault}
	effective := qpu
	if isDefault {
		effective = legacyQuotaPerUnitDefault
	}
	if effective != legacyQuotaPerUnitDefault {
		return fmt.Errorf("前置守卫失败：旧库 QuotaPerUnit=%d ≠ %d，迁移中止", effective, legacyQuotaPerUnitDefault)
	}
	return nil
}

// ---- 逐表迁移 ----

// migrateUsers 迁移 users；返回迁移后的行（tokens 继承用户组用）。
func migrateUsers(ro *gorm.DB, tgt *store.Store, rep *Report) ([]model.User, error) {
	src, err := readLegacyUsers(ro)
	if err != nil {
		return nil, err
	}
	rep.Users.Read = len(src)
	out := make([]model.User, 0, len(src))
	for _, u := range src {
		m, err := TransformUser(u)
		if err != nil {
			rep.Users.Failed++
			rep.Users.Failures = append(rep.Users.Failures, RowFailure{ID: u.ID, Reason: err.Error()})
			continue
		}
		out = append(out, m)
	}
	if err := upsertUsers(tgt.Write, out); err != nil {
		return nil, fmt.Errorf("写入 users: %w", err)
	}
	rep.Users.Migrated = len(out)
	return out, nil
}

// migrateTokens 迁移 tokens：group 空继承所属用户组（users 已 default 化）。
func migrateTokens(ro *gorm.DB, tgt *store.Store, rep *Report, users []model.User) error {
	src, err := readLegacyTokens(ro)
	if err != nil {
		return err
	}
	rep.Tokens.Read = len(src)
	userGroup := make(map[int64]string, len(users))
	for _, u := range users {
		userGroup[u.ID] = u.Group
	}
	out := make([]model.Token, 0, len(src))
	for _, t := range src {
		m, err := TransformToken(t, userGroup[t.UserID])
		if err != nil {
			rep.Tokens.Failed++
			rep.Tokens.Failures = append(rep.Tokens.Failures, RowFailure{ID: t.ID, Reason: err.Error()})
			continue
		}
		out = append(out, m)
	}
	if err := upsertTokens(tgt.Write, out); err != nil {
		return fmt.Errorf("写入 tokens: %w", err)
	}
	rep.Tokens.Migrated = len(out)
	return nil
}

// migrateChannels 迁移 channels；返回迁移后的行（ModelRatio 最小集依据）。
func migrateChannels(ro *gorm.DB, tgt *store.Store, rep *Report) ([]model.Channel, error) {
	src, err := readLegacyChannels(ro)
	if err != nil {
		return nil, err
	}
	rep.Channels.Read = len(src)
	out := make([]model.Channel, 0, len(src))
	gaps := []RowFailure{}
	dropped := 0
	for _, c := range src {
		res, err := TransformChannel(c)
		if err != nil {
			rep.Channels.Failed++
			rep.Channels.Failures = append(rep.Channels.Failures, RowFailure{ID: c.ID, Reason: err.Error()})
			continue
		}
		out = append(out, res.Channel)
		if res.ModelMappingRaw != "" {
			gaps = append(gaps, RowFailure{ID: c.ID,
				Reason: "model_mapping 未迁移（hui 无对应列）： " + res.ModelMappingRaw})
		}
		if res.GroupDropped {
			dropped++
		}
	}
	if err := upsertChannels(tgt.Write, out); err != nil {
		return nil, fmt.Errorf("写入 channels: %w", err)
	}
	rep.Channels.Migrated = len(out)
	rep.Channels.ModelMappingGaps = gaps
	rep.Channels.GroupDropped = dropped
	return out, nil
}

// migrateOptions 迁移 options：白名单 4 键逐字 + ModelRatio 按启用渠道最小集换算；
// 其余键全部计入未迁清单。返回对账期望值（键 → 迁移后值）。
func migrateOptions(ro *gorm.DB, tgt *store.Store, rep *Report, channels []model.Channel) (map[string]string, error) {
	src, err := readLegacyOptions(ro)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]string, len(src))
	for _, o := range src {
		byKey[o.Key] = o.Value
	}

	expect := map[string]string{}
	migrated := []string{}
	// 逐字键（白名单前 4 个）。
	for _, k := range migrateOptionKeys[:4] {
		v, ok := byKey[k]
		if !ok {
			continue // 旧库未配置：目标库保持代码内缺省，不落键
		}
		expect[k] = v
		migrated = append(migrated, k)
	}

	// ModelRatio：启用渠道模型最小集 × 2 换算。
	rawMR := byKey[billing.OptionKeyModelRatio]
	mr, err := TransformModelRatio(rawMR, ActiveModelsFromChannels(channels))
	if err != nil {
		return nil, err
	}
	if rawMR != "" {
		expect[billing.OptionKeyModelRatio] = mr.MigratedJSON
		migrated = append(migrated, billing.OptionKeyModelRatio)
	}
	var legacyMRCount int
	if strings.TrimSpace(rawMR) != "" {
		var legacyMR map[string]float64
		if err := json.Unmarshal([]byte(rawMR), &legacyMR); err != nil {
			return nil, fmt.Errorf("旧库 ModelRatio JSON 解析失败: %w", err)
		}
		legacyMRCount = len(legacyMR)
	}

	// 写入目标库（单事务逐键 upsert）。
	if err := tgt.Write.Transaction(func(tx *gorm.DB) error {
		for k, v := range expect {
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoUpdates: clause.AssignmentColumns([]string{"value"}),
			}).Create(&model.Option{Key: k, Value: v}).Error; err != nil {
				return fmt.Errorf("写入 option %s: %w", k, err)
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("写入 options: %w", err)
	}

	sort.Strings(migrated)
	// 未迁清单：全部旧键 - 已迁键（按 key 升序）。
	migratedSet := make(map[string]struct{}, len(migrated))
	for _, k := range migrated {
		migratedSet[k] = struct{}{}
	}
	skipped := []SkippedOption{}
	for _, o := range src { // src 已按 key 升序
		if _, ok := migratedSet[o.Key]; ok {
			continue
		}
		skipped = append(skipped, SkippedOption{Key: o.Key,
			Reason: "非迁移白名单键（换皮/运营/运行时行为键），hui 无对应语义"})
	}
	rep.Options = OptionsReport{
		MigratedKeys: migrated,
		Skipped:      skipped,
		ModelRatio: ModelRatioReport{
			SourceModels:   legacyMRCount,
			MigratedModels: mr.Migrated,
			MissingModels:  mr.Missing,
			Conversion:     modelRatioConversionNote,
		},
	}
	return expect, nil
}

// migrateRedemptions 迁移 redemptions；返回旧兑换码 id → 面值（topup 日志合成关联用，
// 含变换失败的行：面值属旧库事实）。
func migrateRedemptions(ro *gorm.DB, tgt *store.Store, rep *Report) (map[int64]int64, error) {
	src, err := readLegacyRedemptions(ro)
	if err != nil {
		return nil, err
	}
	rep.Redemptions.Read = len(src)
	now := time.Now().Unix()
	out := make([]model.Redemption, 0, len(src))
	quotaByID := make(map[int64]int64, len(src))
	for _, r := range src {
		quotaByID[r.ID] = r.Quota
		m, err := TransformRedemption(r, now)
		if err != nil {
			rep.Redemptions.Failed++
			rep.Redemptions.Failures = append(rep.Redemptions.Failures, RowFailure{ID: r.ID, Reason: err.Error()})
			continue
		}
		out = append(out, m)
	}
	if err := upsertRedemptions(tgt.Write, out); err != nil {
		return nil, fmt.Errorf("写入 redemptions: %w", err)
	}
	rep.Redemptions.Migrated = len(out)
	return quotaByID, nil
}

// migrateLogs 迁移日志：type=2 consume 全量 + type=1 合成 topup 对账日志；
// 其余类型计数入报告不迁。
func migrateLogs(ro *gorm.DB, tgt *store.Store, rep *Report, redemptionQuota map[int64]int64) error {
	consume, err := readLegacyLogsByType(ro, legacyLogTypeConsume)
	if err != nil {
		return err
	}
	rep.Logs.ConsumeRead = len(consume)
	out := make([]model.Log, 0, len(consume))
	for _, l := range consume {
		m, outcome, err := TransformConsumeLog(l)
		if err != nil {
			rep.Logs.ConsumeFailed++
			rep.Logs.ConsumeFailures = append(rep.Logs.ConsumeFailures, RowFailure{ID: l.ID, Reason: err.Error()})
			continue
		}
		switch outcome {
		case DetailKept:
			rep.Logs.DetailKept++
		case DetailDropped:
			rep.Logs.DetailDropped++
		}
		out = append(out, m)
	}
	if err := upsertLogs(tgt.Write, out); err != nil {
		return fmt.Errorf("写入 consume 日志: %w", err)
	}
	rep.Logs.ConsumeMigrated = len(out)

	tops, err := readLegacyLogsByType(ro, legacyLogTypeTopup)
	if err != nil {
		return err
	}
	topRows := make([]model.Log, 0, len(tops))
	for _, l := range tops {
		m, linked, err := TransformTopupLog(l, redemptionQuota)
		if err != nil {
			return fmt.Errorf("合成 topup 日志: %w", err)
		}
		if linked {
			rep.Logs.TopupAmountLinked++
		}
		topRows = append(topRows, m)
	}
	if err := upsertLogs(tgt.Write, topRows); err != nil {
		return fmt.Errorf("写入 topup 日志: %w", err)
	}
	rep.Logs.TopupSynthesized = len(topRows)

	skipped, err := readLegacyLogSkippedTypes(ro, legacyLogTypeTopup, legacyLogTypeConsume)
	if err != nil {
		return err
	}
	rep.Logs.SkippedByType = skipped
	return nil
}

// ---- upsert（自然键覆盖式同步，分块事务） ----

// upsertBatch 按 batchRows 分块、每块一个事务执行自然键 upsert；
// assignCols 是 DO UPDATE 覆盖列（excluded 引用，即"以迁移产物为准"）。
func upsertBatch[T any](db *gorm.DB, rows []T, batchRows int, conflictCol string, assignCols []string) error {
	if len(rows) == 0 {
		return nil
	}
	for start := 0; start < len(rows); start += batchRows {
		end := min(start+batchRows, len(rows))
		batch := rows[start:end]
		err := db.Transaction(func(tx *gorm.DB) error {
			return tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: conflictCol}},
				DoUpdates: clause.AssignmentColumns(assignCols),
			}).Create(&batch).Error
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func upsertUsers(db *gorm.DB, rows []model.User) error {
	return upsertBatch(db, rows, upsertBatchRows, "username", []string{
		"username", "password_hash", "display_name", "role", "status",
		"quota", "used_quota", "email", "group", "created_time", "last_login_time",
	})
}

func upsertTokens(db *gorm.DB, rows []model.Token) error {
	return upsertBatch(db, rows, upsertBatchRows, "key_hash", []string{
		"user_id", "name", "key", "key_hash", "status", "quota", "remain_quota",
		"unlimited_quota", "model_limits", "allow_ips", "group", "expired_time",
		"created_time", "accessed_time",
	})
}

func upsertChannels(db *gorm.DB, rows []model.Channel) error {
	return upsertBatch(db, rows, upsertBatchRows, "id", []string{
		"name", "type", "base_url", "key", "models", "priority", "weight",
		"status", "param_override", "created_time",
	})
}

func upsertRedemptions(db *gorm.DB, rows []model.Redemption) error {
	return upsertBatch(db, rows, upsertBatchRows, "key", []string{
		"key", "name", "status", "quota", "created_by", "used_by",
		"used_time", "expired_time", "created_time",
	})
}

func upsertLogs(db *gorm.DB, rows []model.Log) error {
	return upsertBatch(db, rows, logBatchRows, "id", []string{
		"user_id", "token_id", "channel_id", "protocol", "model_name",
		"prompt_tokens", "completion_tokens", "quota", "use_time", "is_stream",
		"detail", "created_time",
	})
}

// ---- 内置对账 ----

// reconcile 逐表比对旧侧（只读）与新侧；optionExpect 是迁移键的期望值（键 → 迁移后值）。
// 任一查询失败即返回 error（对账无法完成视为失败）。
func reconcile(ro *gorm.DB, tgt *store.Store, rep *Report, optionExpect map[string]string) error {
	items := []ReconItem{}
	t := tgt.Read

	type pair struct {
		table, metric string
		lq, tq        string
		larg          any // 旧侧查询占位参数（无则 nil）
	}
	pairs := []pair{
		{table: "users", metric: "rows", lq: `SELECT COUNT(*) FROM users`, tq: `SELECT COUNT(*) FROM users`},
		{table: "users", metric: "sum_quota", lq: `SELECT COALESCE(SUM(quota),0) FROM users`, tq: `SELECT COALESCE(SUM(quota),0) FROM users`},
		{table: "users", metric: "sum_used_quota", lq: `SELECT COALESCE(SUM(used_quota),0) FROM users`, tq: `SELECT COALESCE(SUM(used_quota),0) FROM users`},
		{table: "tokens", metric: "rows", lq: `SELECT COUNT(*) FROM tokens`, tq: `SELECT COUNT(*) FROM tokens`},
		{table: "tokens", metric: "rows_unlimited", lq: `SELECT COALESCE(SUM(unlimited_quota),0) FROM tokens`,
			tq: `SELECT COALESCE(SUM(CASE WHEN unlimited_quota THEN 1 ELSE 0 END),0) FROM tokens`},
		{table: "tokens", metric: "sum_remain_non_unlimited", lq: `SELECT COALESCE(SUM(CASE WHEN unlimited_quota=0 THEN remain_quota ELSE 0 END),0) FROM tokens`,
			tq: `SELECT COALESCE(SUM(CASE WHEN unlimited_quota=0 THEN remain_quota ELSE 0 END),0) FROM tokens`},
		{table: "channels", metric: "rows", lq: `SELECT COUNT(*) FROM channels`, tq: `SELECT COUNT(*) FROM channels`},
		{table: "channels", metric: "sum_priority", lq: `SELECT COALESCE(SUM(priority),0) FROM channels`, tq: `SELECT COALESCE(SUM(priority),0) FROM channels`},
		{table: "redemptions", metric: "rows", lq: `SELECT COUNT(*) FROM redemptions`, tq: `SELECT COUNT(*) FROM redemptions`},
		{table: "redemptions", metric: "sum_quota", lq: `SELECT COALESCE(SUM(quota),0) FROM redemptions`, tq: `SELECT COALESCE(SUM(quota),0) FROM redemptions`},
		{table: "logs", metric: "consume_rows", lq: `SELECT COUNT(*) FROM logs WHERE type = ?`, tq: `SELECT COUNT(*) FROM logs WHERE protocol = 'openai'`, larg: legacyLogTypeConsume},
		{table: "logs", metric: "consume_sum_quota", lq: `SELECT COALESCE(SUM(quota),0) FROM logs WHERE type = ?`,
			tq: `SELECT COALESCE(SUM(quota),0) FROM logs WHERE protocol = 'openai'`, larg: legacyLogTypeConsume},
		{table: "logs", metric: "topup_rows", lq: `SELECT COUNT(*) FROM logs WHERE type = ?`, tq: `SELECT COUNT(*) FROM logs WHERE protocol = 'topup'`, larg: legacyLogTypeTopup},
	}
	for _, p := range pairs {
		lv, err := scalar(ro, p.lq, p.larg)
		if err != nil {
			return fmt.Errorf("对账旧侧 %s.%s: %w", p.table, p.metric, err)
		}
		tv, err := scalar(t, p.tq)
		if err != nil {
			return fmt.Errorf("对账新侧 %s.%s: %w", p.table, p.metric, err)
		}
		items = append(items, ReconItem{
			Table: p.table, Metric: p.metric, Legacy: lv, Target: tv, Match: lv == tv,
		})
	}

	// options：迁移键逐个值一致性（Legacy 恒 1，Target=1 表示与迁移期望一致）。
	if err := reconcileOptions(t, optionExpect, &items); err != nil {
		return err
	}
	rep.Reconciliation = ReconciliationReport{
		Match: allMatch(items),
		Items: items,
	}
	return nil
}

// reconcileOptions 比对目标库 options 与迁移期望值（键升序，报告确定性）。
func reconcileOptions(t *gorm.DB, expect map[string]string, items *[]ReconItem) error {
	keys := make([]string, 0, len(expect))
	for k := range expect {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var opt model.Option
		err := t.Where("key = ?", k).First(&opt).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			*items = append(*items, ReconItem{Table: "options", Metric: "key:" + k, Legacy: 1, Target: 0, Match: false})
			continue
		}
		if err != nil {
			return fmt.Errorf("对账新侧 options.%s: %w", k, err)
		}
		match := opt.Value == expect[k]
		*items = append(*items, ReconItem{Table: "options", Metric: "key:" + k, Legacy: 1, Target: b2i(match), Match: match})
	}
	return nil
}

// scalar 执行单值查询（COALESCE 兜底 SUM 空集的 NULL）。
func scalar(db *gorm.DB, q string, args ...any) (int64, error) {
	var v int64
	if err := db.Raw(q, args...).Scan(&v).Error; err != nil {
		return 0, err
	}
	return v, nil
}

// allMatch 判断全部对账条目是否一致。
func allMatch(items []ReconItem) bool {
	for _, it := range items {
		if !it.Match {
			return false
		}
	}
	return true
}

// countMismatch 统计不一致条目数。
func countMismatch(items []ReconItem) int {
	n := 0
	for _, it := range items {
		if !it.Match {
			n++
		}
	}
	return n
}

// b2i bool → 0/1。
func b2i(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// oneLine 压缩空白并把文本截断到 max 字节内（报告摘要用）。
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
