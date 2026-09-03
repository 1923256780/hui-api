// totp.go 两步验证（TOTP）端点（M3-wave2，docs/05 §5.9）：
//
//   - POST /api/user/totp/setup：生成密钥落库（enabled=false 待确认），返回
//     secret 与 otpauth:// URI 文本（用户导入认证器 App）；
//   - POST /api/user/totp/enable：验码通过后启用（enabled=true）；
//   - POST /api/user/totp/disable：验码通过后双列清空（secret + enabled）。
//
// 三端点均要求登录态（RequireAuth）。依赖 github.com/pquerna/otp（RFC 6238，
// SHA1/6 位/30s 窗口/±1 窗口容差为库默认值，与主流认证器 App 兼容）。
// 安全语义：setup 在已启用时拒绝（防覆盖在用密钥）；enable/disable 均需
// 持有当前有效密钥并验码，禁用必须凭验证码（防会话被劫持后一键关闭 2FA）；
// secret 序列化豁免（model.User.TOTPSecret json:"-"），接口只回显一次性
// otpauth URI 与 secret 文本（绑定流程必需），不落日志。
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"

	"github.com/1923256780/hui-api/internal/model"
)

// totpIssuer 是 otpauth URI 中的 issuer 标识（认证器 App 列表展示名）。
const totpIssuer = "Hui Api"

// TOTPSetup 生成本用户 TOTP 密钥（登录态）：已启用时拒绝（防覆盖在用密钥）；
// 生成的密钥落库但 enabled 保持 false，需 enable 验码后生效。
// 返回 {secret, otpauth_uri}——secret 供手动录入，URI 供扫码。
func (h *Handler) TOTPSetup(c *gin.Context) {
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
	if row.TOTPEnabled {
		writeErr(c, http.StatusBadRequest, "totp_already_enabled", "两步验证已启用，如需重置请先禁用")
		return
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: row.Username,
	})
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "totp_setup_failed", "生成两步验证密钥失败")
		return
	}
	if err := h.st.Write.Model(&model.User{}).Where("id = ?", row.ID).
		Updates(map[string]any{"totp_secret": key.Secret(), "totp_enabled": false}).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "totp_setup_failed", "保存两步验证密钥失败")
		return
	}
	writeOK(c, gin.H{"secret": key.Secret(), "otpauth_uri": key.URL()})
}

// TOTPEnable 启用两步验证（登录态）：凭 setup 落库的密钥验码，通过后
// enabled=true。未 setup / 已启用 / 错码分别拒绝（错码不消费密钥可重试）。
func (h *Handler) TOTPEnable(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "缺少验证码")
		return
	}
	var row model.User
	if err := h.st.Read.First(&row, u.ID).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if row.TOTPSecret == "" {
		writeErr(c, http.StatusBadRequest, "totp_not_setup", "请先获取两步验证密钥")
		return
	}
	if row.TOTPEnabled {
		writeErr(c, http.StatusBadRequest, "totp_already_enabled", "两步验证已启用")
		return
	}
	if !totp.Validate(req.Code, row.TOTPSecret) {
		writeErr(c, http.StatusBadRequest, "totp_code_invalid", "验证码不正确")
		return
	}
	if err := h.st.Write.Model(&model.User{}).Where("id = ?", row.ID).
		Update("totp_enabled", true).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "totp_enable_failed", "启用两步验证失败")
		return
	}
	writeOK(c, gin.H{"enabled": true})
}

// TOTPDisable 禁用两步验证（登录态）：验码通过后双列清空，即 secret 置空串、
// enabled 置 false，回到未绑定状态；此后登录免验码。禁用必须凭当前有效
// 验证码——防止会话被劫持后直接关闭 2FA 绕过二次验证。
func (h *Handler) TOTPDisable(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		writeErr(c, http.StatusBadRequest, "invalid_request", "缺少验证码")
		return
	}
	var row model.User
	if err := h.st.Read.First(&row, u.ID).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if !row.TOTPEnabled || row.TOTPSecret == "" {
		writeErr(c, http.StatusBadRequest, "totp_not_enabled", "两步验证未启用")
		return
	}
	if !totp.Validate(req.Code, row.TOTPSecret) {
		writeErr(c, http.StatusBadRequest, "totp_code_invalid", "验证码不正确")
		return
	}
	if err := h.st.Write.Model(&model.User{}).Where("id = ?", row.ID).
		Updates(map[string]any{"totp_secret": "", "totp_enabled": false}).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "totp_disable_failed", "禁用两步验证失败")
		return
	}
	writeOK(c, gin.H{"enabled": false})
}

// otpValidateCode 供登录二段式复用的验码包装（当前直通 totp.Validate；
// 独立函数便于未来统一调整容差策略）。
func otpValidateCode(code, secret string) bool {
	return totp.Validate(code, secret)
}
