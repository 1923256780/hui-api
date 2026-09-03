// cmd/migrate：旧网关 SQLite 库 → hui-api 库的一次性迁移工具（M4，docs/07、ADR-0008）。
//
// 用法：
//
//	migrate -legacy <旧库路径> -target <hui库路径> [-report out.json]
//
// 纪律（ADR-0008）：
//   - 旧库绝对只读：内部以 mode=ro + query_only 双层防御打开，工具不具备写旧库能力；
//   - 幂等可重跑：目标库按自然键 upsert，重复执行结果一致（报告亦逐字节确定）；
//   - 强对账：完成后逐表行数/配额和比对，任一不等退出码非零。
//
// 退出码：0 成功且对账一致；1 迁移失败/对账不一致/报告落盘失败；2 参数错误；
// 3 目标库已有数据（重跑守卫拒绝；救援场景确认需覆盖时用 -allow-live-target）。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/1923256780/hui-api/internal/migrate"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

func main() {
	legacy := flag.String("legacy", "", "旧网关 SQLite 库路径（绝对只读打开）")
	target := flag.String("target", "", "hui-api SQLite 库路径（可不存在，自动建表）")
	report := flag.String("report", "", "JSON 报告输出路径（缺省不落盘）")
	exportTokens := flag.String("export-tokens", "", "迁移令牌明文清单输出路径（TSV；迁移成功后导出；敏感文件，仅迁移接管期使用，用后即删）")
	allowLiveTarget := flag.Bool("allow-live-target", false, "目标库已有数据时强制放行重跑（仅用于演练与救援场景；正常切换应先删除重建空库再迁移）")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "hui-api 旧库迁移工具（一次性，M4）")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "用法:")
		fmt.Fprintln(os.Stderr, "  migrate -legacy <旧库路径> -target <hui库路径> [-report out.json]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "参数:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "退出码: 0 成功且对账一致；1 迁移失败/对账不一致/报告落盘失败；2 参数错误；3 目标库已有数据（守卫拒绝）")
	}
	flag.Parse()

	if *legacy == "" || *target == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*legacy, *target, *report, *exportTokens, *allowLiveTarget); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		if errors.Is(err, migrate.ErrLiveTarget) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

// run 执行一次迁移：调用 migrate.Run，报告（非 nil 时）落盘，迁移成功且指定
// -export-tokens 时导出令牌明文清单，最后向 stdout 打印摘要。
// 迁移失败（含对账不一致、目标库守卫拒绝）也尽力落盘报告供人工排查；返回首个错误。
func run(legacyPath, targetPath, reportPath, exportTokensPath string, allowLiveTarget bool) error {
	rep, err := migrate.Run(migrate.Options{
		LegacyPath: legacyPath, TargetPath: targetPath, AllowLiveTarget: allowLiveTarget,
	})
	if rep != nil && reportPath != "" {
		if werr := writeReport(reportPath, rep); werr != nil {
			if err != nil {
				return fmt.Errorf("%w（且报告落盘失败: %v）", err, werr)
			}
			return werr
		}
		fmt.Printf("报告已写入 %s\n", reportPath)
	}
	if err == nil && exportTokensPath != "" {
		if xerr := exportTokens(targetPath, exportTokensPath); xerr != nil {
			return fmt.Errorf("导出令牌清单: %w", xerr)
		}
		fmt.Printf("令牌清单已写入 %s（敏感：仅迁移接管期使用，用后即删）\n", exportTokensPath)
	}
	printSummary(rep)
	return err
}

// exportTokens 读目标库 tokens 明文 key（迁移后 tokens.key 列保存 sk- 前缀明文），
// 写 TSV（id, user_id, key）。仅迁移接管期用于令牌分发/演练核对：转发面鉴权
// 只依赖 key_hash，明文清单丢失不影响网关运行。文件敏感，权限 0600。
func exportTokens(targetPath, outPath string) error {
	st, err := store.Open(targetPath)
	if err != nil {
		return fmt.Errorf("打开目标库: %w", err)
	}
	defer func() { _ = st.Close() }()
	var rows []model.Token
	if err := st.Read.Model(&model.Token{}).Order("id").Find(&rows).Error; err != nil {
		return fmt.Errorf("读取令牌: %w", err)
	}
	var b strings.Builder
	b.WriteString("id\tuser_id\tkey\n")
	for _, t := range rows {
		fmt.Fprintf(&b, "%d\t%d\t%s\n", t.ID, t.UserID, t.Key)
	}
	if err := os.WriteFile(outPath, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("写出文件: %w", err)
	}
	return nil
}

// writeReport 将报告以缩进 JSON 落盘（末尾换行；内容确定性，不含时间戳）。
func writeReport(path string, rep *migrate.Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化报告: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("写出文件: %w", err)
	}
	return nil
}

// printSummary 向 stdout 打印人类可读摘要（确定性输出，不含时间戳）。
func printSummary(rep *migrate.Report) {
	if rep == nil {
		fmt.Println("迁移未产生报告（旧库打开或目标库 schema 阶段即失败，详见错误输出）")
		return
	}
	fmt.Printf("结果: ok=%v（legacy=%s target=%s）\n", rep.OK, rep.LegacyPath, rep.TargetPath)
	fmt.Printf("守卫: quota_per_unit=%d 缺省放行=%v 目标库=%s 强制放行=%v\n",
		rep.Guard.QuotaPerUnit, rep.Guard.QuotaPerUnitIsDefault, rep.Guard.TargetState, rep.Guard.TargetForced)
	fmt.Printf("users: 读=%d 迁=%d 失败=%d\n", rep.Users.Read, rep.Users.Migrated, rep.Users.Failed)
	fmt.Printf("tokens: 读=%d 迁=%d 失败=%d\n", rep.Tokens.Read, rep.Tokens.Migrated, rep.Tokens.Failed)
	fmt.Printf("channels: 读=%d 迁=%d 失败=%d 组丢弃=%d model_mapping缺口=%d\n",
		rep.Channels.Read, rep.Channels.Migrated, rep.Channels.Failed,
		rep.Channels.GroupDropped, len(rep.Channels.ModelMappingGaps))
	fmt.Printf("options: 迁移键=%v 未迁=%d\n", rep.Options.MigratedKeys, len(rep.Options.Skipped))
	fmt.Printf("model_ratio: 源=%d 迁=%v 缺口=%v 口径=%s\n",
		rep.Options.ModelRatio.SourceModels, rep.Options.ModelRatio.MigratedModels,
		rep.Options.ModelRatio.MissingModels, rep.Options.ModelRatio.Conversion)
	fmt.Printf("redemptions: 读=%d 迁=%d 失败=%d\n", rep.Redemptions.Read, rep.Redemptions.Migrated, rep.Redemptions.Failed)
	fmt.Printf("logs: consume读=%d 迁=%d 失败=%d topup合成=%d 面值关联=%d 未迁类型=%d种 detail保留=%d 置空=%d\n",
		rep.Logs.ConsumeRead, rep.Logs.ConsumeMigrated, rep.Logs.ConsumeFailed,
		rep.Logs.TopupSynthesized, rep.Logs.TopupAmountLinked,
		len(rep.Logs.SkippedByType), rep.Logs.DetailKept, rep.Logs.DetailDropped)
	for _, f := range rep.Users.Failures {
		fmt.Printf("  行失败 users id=%d: %s\n", f.ID, f.Reason)
	}
	for _, f := range rep.Tokens.Failures {
		fmt.Printf("  行失败 tokens id=%d: %s\n", f.ID, f.Reason)
	}
	for _, f := range rep.Channels.Failures {
		fmt.Printf("  行失败 channels id=%d: %s\n", f.ID, f.Reason)
	}
	for _, f := range rep.Channels.ModelMappingGaps {
		fmt.Printf("  缺口 channels id=%d: %s\n", f.ID, f.Reason)
	}
	for _, f := range rep.Redemptions.Failures {
		fmt.Printf("  行失败 redemptions id=%d: %s\n", f.ID, f.Reason)
	}
	for _, f := range rep.Logs.ConsumeFailures {
		fmt.Printf("  行失败 logs id=%d: %s\n", f.ID, f.Reason)
	}
	for _, s := range rep.Options.Skipped {
		fmt.Printf("  未迁 options %s: %s\n", s.Key, s.Reason)
	}
	for _, it := range rep.Reconciliation.Items {
		mark := "OK"
		if !it.Match {
			mark = "MISMATCH"
		}
		fmt.Printf("对账 %s.%s legacy=%d target=%d [%s]\n", it.Table, it.Metric, it.Legacy, it.Target, mark)
	}
}
