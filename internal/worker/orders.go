// orders.go 充值订单超时关单任务（M3-wave4，docs/05 §5.10 注记）。
//
// 语义：pending（status=1）且创建时间早于 now-timeout 的订单一次性置为
// expired（status=4）——单条条件 UPDATE WHERE status=1，与支付回调结算
// （pending/expired→paid 条件 UPDATE）构成状态机两侧：SQLite 单写池串行化
// 下，任一方先完成迁移，另一方 RowsAffected=0，不存在「过期关单覆盖已
// 支付」的交错。expired 并非终态：结算侧把 expired 一并纳入结算条件
// （验签+金额校验通过即支付事实），关单后到达的真实支付通知仍会复活入账
// （status 落 paid），本任务只迁移 pending、永不覆盖 paid。已支付/失败单
// 不被本任务触及（已支付订单永不关单）。关单是幂等操作，失败留待下一
// 周期自然重试。
//
// 阈值：options 键 topup.order_timeout_minutes（分钟，管理员可调、热生效）
// 优先，未配置/非法回退 DefaultOrderTimeout=15min（设计默认：覆盖主流支付
// 网关订单有效期下限；装配见 cmd/hui-api/main.go，文档注记 docs/05）。
package worker

import (
	"log"
	"time"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// DefaultOrderTimeout 订单超时关单默认阈值（15 分钟）。
const DefaultOrderTimeout = 15 * time.Minute

// OptionKeyTopupOrderTimeoutMinutes 关单阈值 options 键（分钟；topup.* 白名单
// 前缀，管理面可写、热生效；<=0 视为未配置走默认值）。
const OptionKeyTopupOrderTimeoutMinutes = "topup.order_timeout_minutes"

// ExpireStaleTopupOrders 将创建超过 timeout 的 pending 订单置为 expired，
// 返回迁移行数。timeout<=0 时采用 DefaultOrderTimeout。
func ExpireStaleTopupOrders(st *store.Store, timeout time.Duration) int64 {
	if timeout <= 0 {
		timeout = DefaultOrderTimeout
	}
	cutoff := time.Now().Add(-timeout).Unix()
	res := st.Write.Model(&model.TopupOrder{}).
		Where("status = ? AND created_time < ?", model.TopupOrderPending, cutoff).
		Update("status", model.TopupOrderExpired)
	if res.Error != nil {
		log.Printf("worker: 订单超时关单失败: %v", res.Error)
		return 0
	}
	if res.RowsAffected > 0 {
		log.Printf("worker: 订单超时关单 %d 笔（阈值 %s）", res.RowsAffected, timeout)
	}
	return res.RowsAffected
}
