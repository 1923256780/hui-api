// 预扣费账本（docs/04 第三、四节）：冻结 / 多退少补 / 全额退款。
//
// 防透支核心：冻结走「事务内条件 UPDATE」，只有 tokens.remain_quota >= delta
// 的行会被扣减，RowsAffected=0 即余额不足（并发下由 SQLite 写池单连接串行化 +
// 条件谓词保证原子判定，不依赖应用层读改写）。
// 补扣允许把余额扣到负数（docs/04：负数余额触发拒绝后续请求，由冻结路径拒绝）。
// users.quota 与 tokens.remain_quota 同步增减（同事务）。
package billing

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// ErrInsufficientQuota 表示令牌余额不足以完成预扣冻结（gateway 映射 HTTP 403）。
var ErrInsufficientQuota = errors.New("insufficient quota")

// Ledger 是预扣费账本：基于 store 写池（单连接）的事务性余额操作。并发安全。
type Ledger struct {
	st *store.Store
}

// NewLedger 构造账本。
func NewLedger(st *store.Store) *Ledger { return &Ledger{st: st} }

// Freeze 预扣冻结：条件扣减 tokens.remain_quota 并同步扣减 users.quota（同事务）。
// delta <= 0 直接返回 nil（unlimited 令牌由调用方传 0 跳过冻结）。
// 余额不足返回 ErrInsufficientQuota（事务回滚，无副作用）。
func (l *Ledger) Freeze(tokenID, userID, delta int64) error {
	if delta <= 0 {
		return nil
	}
	return l.st.Write.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Token{}).
			Where("id = ? AND remain_quota >= ?", tokenID, delta).
			Update("remain_quota", gorm.Expr("remain_quota - ?", delta))
		if res.Error != nil {
			return fmt.Errorf("冻结令牌额度: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrInsufficientQuota
		}
		return addUserQuotaDelta(tx, userID, -delta)
	})
}

// Settle 按实际用量多退少补：diff = frozen - actual，正数退还、负数补扣
// （补扣允许透支到负数，无下限条件）。frozen == actual 时幂等无操作。
func (l *Ledger) Settle(tokenID, userID, frozen, actual int64) error {
	return l.adjust(tokenID, userID, frozen-actual)
}

// RefundFull 全额退还冻结额（失败 / 流中断场景，docs/04 第四节）。
func (l *Ledger) RefundFull(tokenID, userID, frozen int64) error {
	return l.adjust(tokenID, userID, frozen)
}

// adjust 无条件调整余额：delta>0 退还（加回），delta<0 补扣（继续扣减）。
func (l *Ledger) adjust(tokenID, userID, delta int64) error {
	if delta == 0 {
		return nil
	}
	return l.st.Write.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Token{}).
			Where("id = ?", tokenID).
			Update("remain_quota", gorm.Expr("remain_quota + ?", delta))
		if res.Error != nil {
			return fmt.Errorf("调整令牌额度(%+d): %w", delta, res.Error)
		}
		// 令牌行不存在（异常删除等）时 RowsAffected=0：用户侧照常入账，
		// 主链路不因对账尾巴失败。
		return addUserQuotaDelta(tx, userID, delta)
	})
}

// addUserQuotaDelta 同步调整用户余额；userID <= 0 视为无归属用户，静默跳过。
func addUserQuotaDelta(tx *gorm.DB, userID, delta int64) error {
	if userID <= 0 || delta == 0 {
		return nil
	}
	if err := tx.Model(&model.User{}).
		Where("id = ?", userID).
		Update("quota", gorm.Expr("quota + ?", delta)).Error; err != nil {
		return fmt.Errorf("调整用户余额(%+d): %w", delta, err)
	}
	return nil
}
