// Package migrate 实现旧网关 SQLite 库 → hui-api 库的一次性数据迁移工具（M4，docs/07）。
//
// 核心纪律（ADR-0008）：
//   - 旧库绝对只读：唯一读取通道是 store.OpenReadOnly（mode=ro + query_only 双层防御）；
//   - 幂等可重跑：全部写入按自然键 upsert（ON CONFLICT DO UPDATE），重复执行结果一致；
//   - 强对账：迁移完成后逐表行数/配额和比对，不一致即失败，宁可拒绝不可错账。
//
// 本文件定义迁移报告（JSON 产物）的结构。报告必须完全确定性：
// 不含时间戳，切片按稳定顺序输出，两次全量迁移（同源同目标起点）产出逐字节一致的报告。
package migrate

// Report 是迁移报告的顶层结构（-report 输出与 stdout 摘要共用）。
type Report struct {
	LegacyPath string `json:"legacy_path"` // 旧库文件路径（只读打开）
	TargetPath string `json:"target_path"` // 目标库文件路径
	OK         bool   `json:"ok"`          // 全部表迁移完成且对账一致

	Guard GuardReport `json:"guard"`

	Users       TableReport        `json:"users"`
	Tokens      TableReport        `json:"tokens"`
	Channels    ChannelTableReport `json:"channels"`
	Options     OptionsReport      `json:"options"`
	Redemptions TableReport        `json:"redemptions"`
	Logs        LogsReport         `json:"logs"`

	Reconciliation ReconciliationReport `json:"reconciliation"`
}

// GuardReport 是前置守卫结果。QuotaPerUnitIsDefault=true 表示旧库未落库该键，
// 按旧网关缺省 500000 放行；false 表示旧库显式配置，值必须等于 500000 才能通过。
type GuardReport struct {
	QuotaPerUnit          int64 `json:"quota_per_unit"`
	QuotaPerUnitIsDefault bool  `json:"quota_per_unit_is_default"`
}

// RowFailure 是单行硬失败记录（该行未迁移，不影响其余行继续）。
type RowFailure struct {
	ID     int64  `json:"id"`     // 旧库行 id（日志等无业务 id 场景即主键）
	Reason string `json:"reason"` // 失败原因（中文，供人工排查）
}

// TableReport 是通用单表迁移结果。
type TableReport struct {
	Read     int          `json:"read"`               // 旧库读出行数
	Migrated int          `json:"migrated"`           // 成功写入行数
	Failed   int          `json:"failed"`             // 硬失败行数
	Failures []RowFailure `json:"failures,omitempty"` // 硬失败明细（按旧库 id 升序）
}

// ChannelTableReport 是 channels 表迁移结果：通用计数 + 特有缺口清单。
type ChannelTableReport struct {
	TableReport
	GroupDropped     int          `json:"group_dropped"`                // 渠道多组信息（旧库 group 列）在 hui 无对应列而丢弃的行数
	ModelMappingGaps []RowFailure `json:"model_mapping_gaps,omitempty"` // model_mapping 未迁移的渠道（Reason 为原文摘要）
}

// OptionsReport 是 options 表迁移结果：白名单键逐字迁移 + ModelRatio 换算 + 未迁清单。
type OptionsReport struct {
	MigratedKeys []string         `json:"migrated_keys"` // 成功写入目标库的键（升序）
	Skipped      []SkippedOption  `json:"skipped"`       // 未迁清单（按 key 升序）
	ModelRatio   ModelRatioReport `json:"model_ratio"`
}

// SkippedOption 是未迁移的旧库 option 键及原因。
type SkippedOption struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// ModelRatioReport 是 ModelRatio 换算明细。
type ModelRatioReport struct {
	SourceModels   int      `json:"source_models"`   // 旧 ModelRatio 配置的模型总数
	MigratedModels []string `json:"migrated_models"` // 启用渠道最小集内成功换算的模型（升序）
	MissingModels  []string `json:"missing_models"`  // 最小集内旧库无价的模型（升序，缺口）
	Conversion     string   `json:"conversion"`      // 换算口径说明（固定 "legacy×2"）
}

// LogsReport 是 logs 表迁移结果：consume 全量 + topup 合成 + 其余类型计数。
type LogsReport struct {
	ConsumeRead       int          `json:"consume_read"`               // 旧库 type=2 行数
	ConsumeMigrated   int          `json:"consume_migrated"`           // 成功写入行数
	ConsumeFailed     int          `json:"consume_failed"`             // 硬失败行数
	ConsumeFailures   []RowFailure `json:"consume_failures,omitempty"` // 硬失败明细
	TopupSynthesized  int          `json:"topup_synthesized"`          // type=1 合成 topup 对账日志条数
	TopupAmountLinked int          `json:"topup_amount_linked"`        // 成功关联旧兑换码面值的合成条数
	SkippedByType     []TypeCount  `json:"skipped_by_type,omitempty"`  // 未迁移类型计数（type 升序）
	DetailKept        int          `json:"detail_kept"`                // 旧 other 字段保留为 detail 的条数
	DetailDropped     int          `json:"detail_dropped"`             // 旧 other 非法 JSON 或超长被置空的条数
}

// TypeCount 是未迁移日志类型的计数条目。
type TypeCount struct {
	Type  int   `json:"type"`
	Count int64 `json:"count"`
}

// ReconciliationReport 是内置对账结果：逐表指标比对，任一不等即 Match=false。
type ReconciliationReport struct {
	Match bool        `json:"match"`
	Items []ReconItem `json:"items"` // 比对条目（固定顺序：users/tokens/channels/redemptions/logs/options）
}

// ReconItem 是单条对账指标。options 键值一致性条目的语义：
// Legacy 恒为 1（存在期望值），Target=1 表示目标值与期望一致、0 表示不一致。
type ReconItem struct {
	Table  string `json:"table"`  // users/tokens/channels/redemptions/logs/options
	Metric string `json:"metric"` // rows/sum_quota/sum_used_quota/sum_priority/rows_unlimited/sum_remain_non_unlimited/consume_rows/consume_sum_quota/topup_rows/key:<键名>
	Legacy int64  `json:"legacy"`
	Target int64  `json:"target"`
	Match  bool   `json:"match"`
}
