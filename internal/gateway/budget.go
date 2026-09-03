// budget.go 令牌预算周期惰性重置（M2-wave3，docs/04）：
//
//   - tokens.budget_duration 声明预算窗口：”=无周期（一次额度用完为止）、
//     '24h' / '7d' / '30d' 按固定时长滚动、'monthly' 按自然月滚动（相位对齐）；
//   - tokens.budget_reset_at 记录下次重置时间（unix 秒；0=未初始化，首次请求落边界）；
//   - 窗口语义：周期内消耗受 remain_quota 封顶（与预扣费账本共用一列），窗口
//     滚动时 remain_quota 复原为 quota（周期总额），已用计数随之天然清零；
//   - 惰性重置：不靠定时器，请求到达时发现窗口过期才滚动，并写一条 budget 日志；
//   - 并发正确性：CAS 条件 UPDATE（WHERE budget_reset_at = 旧值），并发请求中
//     仅一个赢得滚动；其余按各自快照放行——冻结走 DB 条件扣减，最终一致；
//   - 窗口过期跨越多个周期时逐窗口步进推进，保持边界相位（如每月 15 号恒为
//     15 号）；monthly 按自然月推进并对目标月缺失同号日钳制到月末（1/31 → 2/28）。
package gateway

import (
	"encoding/json"
	"log"
	"time"

	"github.com/1923256780/hui-api/internal/model"
)

// BudgetDurationMonthly 是自然月预算窗口取值（tokens.budget_duration）。
const BudgetDurationMonthly = "monthly"

// budgetWindowFixed 返回固定时长窗口；monthly 与未知取值返回 0（前者走自然月
// 推进，后者按无周期降级——非法配置静默忽略，不影响请求放行）。
func budgetWindowFixed(duration string) time.Duration {
	switch duration {
	case "24h":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	default:
		return 0
	}
}

// nextBudgetBoundary 从 from 推进一个窗口返回下一边界。monthly 按自然月推进
// （同号日缺失时钳制到月末），其余按固定时长。
func nextBudgetBoundary(duration string, from time.Time) time.Time {
	if duration == BudgetDurationMonthly {
		return addMonthsClamped(from, 1)
	}
	return from.Add(budgetWindowFixed(duration))
}

// addMonthsClamped 推进 n 个自然月：目标月不存在同号日时钳制到月末
// （1/31 → 2/28，闰年 1/29 → 2/29）。time.Date 的月份溢出规范化保证 m+n>12 自动进位年份。
func addMonthsClamped(from time.Time, n int) time.Time {
	first := time.Date(from.Year(), from.Month()+time.Month(n), 1,
		from.Hour(), from.Minute(), from.Second(), from.Nanosecond(), from.Location())
	lastDay := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, from.Location()).Day()
	day := from.Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(first.Year(), first.Month(), day,
		from.Hour(), from.Minute(), from.Second(), from.Nanosecond(), from.Location())
}

// rollBudget 检查令牌预算窗口并在过期时执行惰性重置，返回本次请求使用的令牌
// 快照。无周期、未知取值或 unlimited 令牌直接返回原快照（零成本路径）。
// 任何 DB 失败只记日志不阻断请求：按当前快照继续，下次请求重试滚动。
func (g *Gateway) rollBudget(tok *model.Token) *model.Token {
	if tok.BudgetDuration == "" || tok.UnlimitedQuota {
		return tok
	}
	if budgetWindowFixed(tok.BudgetDuration) == 0 && tok.BudgetDuration != BudgetDurationMonthly {
		return tok // 未知取值：按无周期处理（非法配置静默降级）
	}
	now := time.Now()

	// 未初始化边界：首次请求落定首个窗口边界（只定界，不重置额度——remain 已是初始值）。
	if tok.BudgetResetAt <= 0 {
		next := nextBudgetBoundary(tok.BudgetDuration, now)
		res := g.st.Write.Model(&model.Token{}).
			Where("id = ? AND budget_reset_at <= 0", tok.ID).
			Update("budget_reset_at", next.Unix())
		if res.Error != nil {
			log.Printf("[budget] 令牌 %d 初始化预算边界失败: %v", tok.ID, res.Error)
			return tok
		}
		if res.RowsAffected > 0 {
			tok.BudgetResetAt = next.Unix()
		}
		// CAS 落空 = 并发请求已完成初始化：按原快照放行（remain 未变）。
		return tok
	}

	// 窗口未过期：直接放行。
	resetAt := time.Unix(tok.BudgetResetAt, 0)
	if now.Before(resetAt) {
		return tok
	}

	// 窗口过期：从旧边界逐窗口步进到覆盖 now 的下一边界（保持相位对齐），
	// 再以 CAS 滚动（remain_quota 复原为 quota）。并发下仅一个请求赢得滚动。
	cursor := resetAt
	for !cursor.After(now) {
		cursor = nextBudgetBoundary(tok.BudgetDuration, cursor)
	}
	res := g.st.Write.Model(&model.Token{}).
		Where("id = ? AND budget_reset_at = ?", tok.ID, tok.BudgetResetAt).
		Updates(map[string]any{
			"remain_quota":    tok.Quota,
			"budget_reset_at": cursor.Unix(),
		})
	if res.Error != nil {
		log.Printf("[budget] 令牌 %d 滚动预算窗口失败: %v", tok.ID, res.Error)
		return tok
	}
	if res.RowsAffected == 0 {
		// 并发请求已完成滚动（旧边界已变）：按原快照放行，冻结走 DB 条件扣减。
		return tok
	}
	log.Printf("[budget] 令牌 %d 预算窗口滚动 duration=%s reset_at=%d remain=%d",
		tok.ID, tok.BudgetDuration, cursor.Unix(), tok.Quota)
	g.writeBudgetResetLog(tok, cursor.Unix())
	tok.RemainQuota = tok.Quota
	tok.BudgetResetAt = cursor.Unix()
	return tok
}

// writeBudgetResetLog 写一条预算重置日志（对账事实源；budget 日志与请求日志共用
// logs 表，Protocol 区分语义）。重置为每令牌每周期至多一次的罕见事件，同步写盘。
func (g *Gateway) writeBudgetResetLog(tok *model.Token, resetAt int64) {
	detail, err := json.Marshal(map[string]any{
		"event":        "budget_reset",
		"duration":     tok.BudgetDuration,
		"reset_at":     resetAt,
		"remain_quota": tok.Quota,
	})
	if err != nil {
		log.Printf("[budget] 构造重置日志失败 token=%d: %v", tok.ID, err)
		return
	}
	row := model.Log{
		UserID:      tok.UserID,
		TokenID:     tok.ID,
		Protocol:    "budget",
		ModelName:   "budget_reset",
		Detail:      string(detail),
		CreatedTime: time.Now().Unix(),
	}
	if err := g.st.Write.Create(&row).Error; err != nil {
		log.Printf("[budget] 写预算重置日志失败 token=%d: %v", tok.ID, err)
	}
}
