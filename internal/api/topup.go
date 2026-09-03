// topup.go 登录用户自服务端点（M2-wave3，docs/05）：
//
//   - POST /api/user/topup：兑换码核销状态机——事务内原子核销（条件 UPDATE
//     status 未用→已用，防并发重复兑换）→ 面值入账 users.quota → 同事务写一条
//     topup 日志；过期码惰性标记 status=已过期并拒绝。
//   - POST /api/token/:id/assign：额度划转——users.quota → tokens.remain_quota
//     的转移事务（条件扣减用户余额防透支，令牌余额与总额同步增加）。
//   - GET /api/user/self：当前登录用户信息（余额展示用）。
//
// 三者均只要求登录态（RequireAuth），不要求 root。核销与划转共用 SQLite 单写池
// 串行化 + 条件谓词保证并发正确性（与 billing.Ledger 同一并发模型）。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/model"
)

// 兑换/划转语义错误（映射 HTTP 状态与语义码，见 docs/05 错误码表）。
var (
	errTopupNotFound    = errors.New("topup: code not found")
	errTopupUsed        = errors.New("topup: code already redeemed")
	errTopupVoided      = errors.New("topup: code voided")
	errTopupExpired     = errors.New("topup: code expired")
	errTopupUserMissing = errors.New("topup: user missing")
	errAssignNotFound   = errors.New("assign: token not found")
	errAssignForbidden  = errors.New("assign: not token owner")
	errAssignUnlimited  = errors.New("assign: unlimited token")
	errAssignInsuff     = errors.New("assign: insufficient user quota")
)

// TopupRedeem 兑换码核销：登录用户提交 key，事务内完成「校验 → 原子核销 →
// 入账 → 日志」。任何一步失败整体回滚（无部分入账）。
func (h *Handler) TopupRedeem(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "缺少兑换码")
		return
	}
	now := time.Now().Unix()
	added := int64(0)
	expired := false // 惰性过期标记已提交（区别于事务内失败回滚）
	err := h.st.Write.Transaction(func(tx *gorm.DB) error {
		var r model.Redemption
		if err := tx.Where("key = ?", key).First(&r).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errTopupNotFound
			}
			return errWrap("查询兑换码", err)
		}
		switch r.Status {
		case model.RedemptionRedeemed:
			return errTopupUsed
		case model.RedemptionVoided:
			return errTopupVoided
		case model.RedemptionExpired:
			return errTopupExpired
		}
		// 未使用码：过期校验。超期时惰性标记 status=已过期并随本事务提交
		//（返回 nil 而非错误，避免标记随回滚丢失），事后映射为拒绝响应。
		if r.ExpiredTime != model.EpochForever && r.ExpiredTime < now {
			res := tx.Model(&model.Redemption{}).
				Where("id = ? AND status = ?", r.ID, model.RedemptionUnused).
				Update("status", model.RedemptionExpired)
			if res.Error != nil {
				return errWrap("标记兑换码过期", res.Error)
			}
			expired = true
			return nil
		}
		// 原子核销：条件 UPDATE 未用→已用，RowsAffected=0 即并发竞争失败。
		res := tx.Model(&model.Redemption{}).
			Where("id = ? AND status = ?", r.ID, model.RedemptionUnused).
			Updates(map[string]any{
				"status":    model.RedemptionRedeemed,
				"used_by":   u.ID,
				"used_time": now,
			})
		if res.Error != nil {
			return errWrap("核销兑换码", res.Error)
		}
		if res.RowsAffected == 0 {
			return errTopupUsed
		}
		// 面值入账用户余额（兑换后由用户自行划转到令牌）。
		res = tx.Model(&model.User{}).Where("id = ?", u.ID).
			Update("quota", gorm.Expr("quota + ?", r.Quota))
		if res.Error != nil {
			return errWrap("入账用户余额", res.Error)
		}
		if res.RowsAffected == 0 {
			return errTopupUserMissing
		}
		// 同事务写一条 topup 日志（对账事实源；明细不含兑换码明文）。
		if err := tx.Create(&model.Log{
			UserID:      u.ID,
			Protocol:    "topup",
			ModelName:   "redemption",
			Quota:       r.Quota,
			Detail:      topupDetail("topup", r.ID, r.Quota),
			CreatedTime: now,
		}).Error; err != nil {
			return errWrap("写兑换日志", err)
		}
		added = r.Quota
		return nil
	})
	if err != nil {
		writeTopupErr(c, err)
		return
	}
	if expired {
		writeErr(c, http.StatusBadRequest, "redemption_expired", "兑换码已过期")
		return
	}
	var user model.User
	if err := h.st.Read.First(&user, u.ID).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "topup_failed", "核销成功但查询余额失败")
		return
	}
	writeOK(c, gin.H{"quota_added": added, "user_quota": user.Quota})
}

// AssignTokenQuota 额度划转：把当前登录用户余额划入其令牌（user.quota →
// token.remain_quota 同事务转移）。条件扣减用户余额（quota >= delta）防透支；
// 令牌 remain_quota 与 quota（周期总额）同步增加保持 remain <= quota 不变式。
// 仅令牌归属者或管理员可操作；unlimited 令牌无余额语义，拒绝划转。
func (h *Handler) AssignTokenQuota(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var req struct {
		Quota int64 `json:"quota"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Quota <= 0 {
		writeErr(c, http.StatusBadRequest, "invalid_request", "quota 必须大于 0")
		return
	}
	now := time.Now().Unix()
	err := h.st.Write.Transaction(func(tx *gorm.DB) error {
		var tok model.Token
		if err := tx.First(&tok, paramID(c)).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errAssignNotFound
			}
			return errWrap("查询令牌", err)
		}
		if tok.UserID != u.ID {
			var actor model.User
			if err := tx.First(&actor, u.ID).Error; err != nil {
				return errWrap("查询操作者", err)
			}
			if actor.Role != model.RoleAdmin {
				return errAssignForbidden
			}
		}
		if tok.UnlimitedQuota {
			return errAssignUnlimited
		}
		// 条件扣减令牌归属者的用户余额：余额不足 RowsAffected=0（防透支，
		// 无副作用）。账源恒为归属者（管理员代划时从归属者余额转移，不凭空注入）。
		res := tx.Model(&model.User{}).
			Where("id = ? AND quota >= ?", tok.UserID, req.Quota).
			Update("quota", gorm.Expr("quota - ?", req.Quota))
		if res.Error != nil {
			return errWrap("扣减用户余额", res.Error)
		}
		if res.RowsAffected == 0 {
			return errAssignInsuff
		}
		// 令牌加额：remain（可用余额）与 quota（周期总额）同步增加。
		if err := tx.Model(&model.Token{}).Where("id = ?", tok.ID).
			Updates(map[string]any{
				"remain_quota": gorm.Expr("remain_quota + ?", req.Quota),
				"quota":        gorm.Expr("quota + ?", req.Quota),
			}).Error; err != nil {
			return errWrap("令牌加额", err)
		}
		if err := tx.Create(&model.Log{
			UserID:      tok.UserID, // 被划转账户（账本口径）
			TokenID:     tok.ID,
			Protocol:    "topup",
			ModelName:   "token_assign",
			Quota:       req.Quota,
			Detail:      topupDetail("token_assign", tok.ID, req.Quota),
			CreatedTime: now,
		}).Error; err != nil {
			return errWrap("写划转日志", err)
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errAssignNotFound):
			writeErr(c, http.StatusNotFound, "not_found", "令牌不存在")
		case errors.Is(err, errAssignForbidden):
			writeErr(c, http.StatusForbidden, "forbidden", "仅令牌归属者或管理员可划转")
		case errors.Is(err, errAssignUnlimited):
			writeErr(c, http.StatusBadRequest, "invalid_request", "不限额令牌无需划转额度")
		case errors.Is(err, errAssignInsuff):
			writeErr(c, http.StatusBadRequest, "insufficient_quota", "用户余额不足")
		default:
			writeErr(c, http.StatusInternalServerError, "assign_failed", "额度划转失败")
		}
		return
	}
	var tok model.Token
	if err := h.st.Read.First(&tok, paramID(c)).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "assign_failed", "划转成功但查询令牌失败")
		return
	}
	writeOK(c, gin.H{"quota_assigned": req.Quota, "remain_quota": tok.RemainQuota})
}

// GetSelf 返回当前登录用户信息（充值页余额展示用）。
func (h *Handler) GetSelf(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var row model.User
	if err := h.st.Read.First(&row, u.ID).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	writeOK(c, row)
}

// writeTopupErr 把核销事务的语义错误映射为 HTTP 响应。
func writeTopupErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errTopupNotFound):
		writeErr(c, http.StatusNotFound, "not_found", "兑换码不存在")
	case errors.Is(err, errTopupUsed):
		writeErr(c, http.StatusConflict, "redemption_used", "兑换码已被使用")
	case errors.Is(err, errTopupVoided):
		writeErr(c, http.StatusBadRequest, "redemption_voided", "兑换码已作废")
	case errors.Is(err, errTopupExpired):
		writeErr(c, http.StatusBadRequest, "redemption_expired", "兑换码已过期")
	case errors.Is(err, errTopupUserMissing):
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
	default:
		writeErr(c, http.StatusInternalServerError, "topup_failed", "兑换失败")
	}
}

// topupDetail 构造 topup 类日志的 detail JSON（不含兑换码明文，防泄露）。
func topupDetail(event string, refID, quota int64) string {
	b, err := json.Marshal(map[string]any{
		"event":  event,
		"ref_id": refID,
		"quota":  quota,
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// errWrap 为存储层错误附业务上下文（对齐仓库 %w 包装纪律）。
func errWrap(action string, err error) error {
	return fmt.Errorf("%s: %w", action, err)
}
