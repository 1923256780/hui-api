// oauth.go 第三方登录与身份绑定（M3-wave2，docs/05 §5.8）：
//
//   - GET /api/oauth/:provider（公开）：登录模式发起——校验 provider 已配置，
//     生成 32hex state 写短 TTL HttpOnly cookie（5min，防 CSRF）后 302 authorize；
//   - GET /api/oauth/:provider/callback（公开）：校验 state → form POST 换
//     access_token → 拉 userinfo → user_identities 命中即签发完整会话 302
//     /console；未命中且注册开放则自动建户（事务建户 + 身份绑定原子）；
//   - GET /api/oauth/:provider/bind（登录态）：绑定模式发起（state cookie 标
//     bind+uid），callback 绑定当前登录用户；
//   - GET/DELETE /api/user/identities(/:id)（登录态）：本人身份列表 / 解绑
//     （校验归属 + 无口令用户须保留至少一个身份防锁死）。
//
// provider：github（固定端点，scope=read:user）；linuxdo / oidc（从
// {issuer}/.well-known/openid-configuration 发现 authorize/token/userinfo
// 端点，结果缓存 1h）。redirect_uri 由请求 Host 推导，scheme 信任
// X-Forwarded-Proto（反代部署义务，注记 docs/11）。
// userinfo 的 uid 提取：github 取 id 字段；oidc 取 sub（回退 id）——信任
// TLS 通道不验 ID token 签名的权衡注记见 docs/11。
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/model"
)

// 运行轨配置键（options 白名单 oauth.* 前缀，docs/05 键表）。
const (
	OptionKeyOAuthGitHubClientID      = "oauth.github.client_id"
	OptionKeyOAuthGitHubClientSecret  = "oauth.github.client_secret"
	OptionKeyOAuthLinuxDOClientID     = "oauth.linuxdo.client_id"
	OptionKeyOAuthLinuxDOClientSecret = "oauth.linuxdo.client_secret"
	OptionKeyOAuthOIDCClientID        = "oauth.oidc.client_id"
	OptionKeyOAuthOIDCClientSecret    = "oauth.oidc.client_secret"
	OptionKeyOAuthOIDCIssuer          = "oauth.oidc.issuer"
)

// 支持的 provider 名单（路径 :provider 白名单外一律 404）。
const (
	ProviderGitHub  = "github"
	ProviderLinuxDO = "linuxdo"
	ProviderOIDC    = "oidc"
)

// OAuth 流程常量。
const (
	oauthStateCookie = "oauth_state"   // state cookie 名（HttpOnly，5min）
	oauthStateTTL    = 5 * time.Minute // state cookie 有效期
	oauthStateBytes  = 16              // 16 字节 → 32 hex 字符
	oauthHTTPTimeout = 10 * time.Second
	oidcDiscoveryTTL = time.Hour // 发现文档缓存
)

// 绑定模式标记（state cookie 第二段）。
const (
	oauthModeLogin = "login"
	oauthModeBind  = "bind"
)

// errOAuthNotConfigured 表示 provider 未配置（映射 404 oauth_not_configured）。
var errOAuthNotConfigured = errors.New("oauth: provider 未配置")

// oauthProviderCfg 是一次授权流程所需的 provider 解析结果。
type oauthProviderCfg struct {
	clientID     string
	clientSecret string
	authorizeURL string
	tokenURL     string
	userinfoURL  string
	scope        string // github=read:user；oidc/linuxdo=openid
	uidField     string // userinfo uid 字段（github=id；oidc/linuxdo=sub 回退 id）
}

// oidcDiscoveryDoc 是 openid-configuration 中本流程用到的三个端点。
type oidcDiscoveryDoc struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// OIDC 发现缓存（issuer → 文档 + 抓取时间）；多副本各自缓存，语义一致。
var (
	oidcCacheMu  sync.Mutex
	oidcCacheDoc = map[string]oidcDiscoveryDoc{}
	oidcCacheAt  = map[string]time.Time{}
)

// oauthProviderConfigured 判断 provider 是否已配置（GET /api/setup 的 oauth
// 块与端点 404 门控共用同一判定）。
func oauthProviderConfigured(rt *config.Runtime, provider string) bool {
	switch provider {
	case ProviderGitHub:
		id, _ := rt.Get(OptionKeyOAuthGitHubClientID)
		sec, _ := rt.Get(OptionKeyOAuthGitHubClientSecret)
		return id != "" && sec != ""
	case ProviderLinuxDO:
		id, _ := rt.Get(OptionKeyOAuthLinuxDOClientID)
		sec, _ := rt.Get(OptionKeyOAuthLinuxDOClientSecret)
		return id != "" && sec != ""
	case ProviderOIDC:
		issuer, _ := rt.Get(OptionKeyOAuthOIDCIssuer)
		id, _ := rt.Get(OptionKeyOAuthOIDCClientID)
		sec, _ := rt.Get(OptionKeyOAuthOIDCClientSecret)
		return issuer != "" && id != "" && sec != ""
	}
	return false
}

// oidcDiscover 拉取并缓存 issuer 的 openid-configuration（TTL 1h）。
// 出站地址由管理员配置（oauth.oidc.issuer / linuxdo 固定 issuer），非终端
// 用户输入（SSRF 面受管理面 root 门控约束），可达性与信任边界由配置方
// 负责（与 SMTP/Turnstile/支付网关同一边界，注记 docs/05/11）。
func (h *Handler) oidcDiscover(ctx context.Context, issuer string) (*oidcDiscoveryDoc, error) {
	key := strings.TrimSuffix(issuer, "/")
	now := time.Now()
	oidcCacheMu.Lock()
	if at, ok := oidcCacheAt[key]; ok && now.Sub(at) < oidcDiscoveryTTL {
		doc := oidcCacheDoc[key]
		oidcCacheMu.Unlock()
		return &doc, nil
	}
	oidcCacheMu.Unlock()
	wellKnown := key + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return nil, fmt.Errorf("oauth: 构造发现请求: %w", err)
	}
	req.Header.Set("User-Agent", "hui-api")
	resp, err := h.oauthHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: 请求发现文档: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: 发现文档状态码 %d", resp.StatusCode)
	}
	var doc oidcDiscoveryDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("oauth: 解析发现文档: %w", err)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.UserinfoEndpoint == "" {
		return nil, errors.New("oauth: 发现文档缺少必要端点")
	}
	oidcCacheMu.Lock()
	oidcCacheDoc[key] = doc
	oidcCacheAt[key] = now
	oidcCacheMu.Unlock()
	return &doc, nil
}

// oauthResolve 解析 provider 的端点与凭据；未配置返回 errOAuthNotConfigured。
func (h *Handler) oauthResolve(ctx context.Context, provider string) (*oauthProviderCfg, error) {
	switch provider {
	case ProviderGitHub:
		id, _ := h.rt.Get(OptionKeyOAuthGitHubClientID)
		sec, _ := h.rt.Get(OptionKeyOAuthGitHubClientSecret)
		if id == "" || sec == "" {
			return nil, errOAuthNotConfigured
		}
		return &oauthProviderCfg{
			clientID: id, clientSecret: sec,
			authorizeURL: h.oauthGithubAuthorizeURL,
			tokenURL:     h.oauthGithubTokenURL,
			userinfoURL:  h.oauthGithubUserinfoURL,
			scope:        "read:user", uidField: "id",
		}, nil
	case ProviderLinuxDO:
		id, _ := h.rt.Get(OptionKeyOAuthLinuxDOClientID)
		sec, _ := h.rt.Get(OptionKeyOAuthLinuxDOClientSecret)
		if id == "" || sec == "" {
			return nil, errOAuthNotConfigured
		}
		doc, err := h.oidcDiscover(ctx, h.oauthLinuxDOIssuer)
		if err != nil {
			return nil, err
		}
		return &oauthProviderCfg{
			clientID: id, clientSecret: sec,
			authorizeURL: doc.AuthorizationEndpoint,
			tokenURL:     doc.TokenEndpoint,
			userinfoURL:  doc.UserinfoEndpoint,
			scope:        "openid", uidField: "sub",
		}, nil
	case ProviderOIDC:
		issuer, _ := h.rt.Get(OptionKeyOAuthOIDCIssuer)
		id, _ := h.rt.Get(OptionKeyOAuthOIDCClientID)
		sec, _ := h.rt.Get(OptionKeyOAuthOIDCClientSecret)
		if issuer == "" || id == "" || sec == "" {
			return nil, errOAuthNotConfigured
		}
		doc, err := h.oidcDiscover(ctx, issuer)
		if err != nil {
			return nil, err
		}
		return &oauthProviderCfg{
			clientID: id, clientSecret: sec,
			authorizeURL: doc.AuthorizationEndpoint,
			tokenURL:     doc.TokenEndpoint,
			userinfoURL:  doc.UserinfoEndpoint,
			scope:        "openid", uidField: "sub",
		}, nil
	}
	return nil, errOAuthNotConfigured
}

// oauthRandomState 生成 32hex 随机 state（crypto/rand，防 CSRF）。
func oauthRandomState() (string, error) {
	buf := make([]byte, oauthStateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauth: 生成 state: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// oauthRedirectURI 由请求推导回调地址：Host 取请求头，scheme 优先信任
// 反代注入的 X-Forwarded-Proto（多值取第一段），无代理头时回退 TLS 探测
// 与 http 缺省——信任边界与部署义务注记见 docs/11。
func oauthRedirectURI(r *http.Request, provider string) string {
	scheme := ""
	if xp := r.Header.Get("X-Forwarded-Proto"); xp != "" {
		scheme = strings.TrimSpace(strings.Split(xp, ",")[0])
	}
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host + "/api/oauth/" + provider + "/callback"
}

// oauthSetStateCookie 写 state cookie：值 = state|mode|uid（uid 仅 bind 模式
// 有意义；HttpOnly + 短 TTL；SameSite=Lax 允许 provider 顶层跳转带回会话）。
// mode/uid 的真实性由两端点（发起时写入、回调时比对会话）保证，伪造只会
// 作用到伪造者自己的会话，无跨用户风险。
func oauthSetStateCookie(c *gin.Context, state, mode string, uid int64) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state + "|" + mode + "|" + strconv.FormatInt(uid, 10),
		Path:     "/",
		MaxAge:   int(oauthStateTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// oauthClearStateCookie 一次性消费：回调入口立即清除 state cookie。
func oauthClearStateCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// OAuthAuthorize 登录模式发起（公开）：provider 未配置 404 oauth_not_configured。
func (h *Handler) OAuthAuthorize(c *gin.Context) {
	h.oauthStart(c, oauthModeLogin, 0)
}

// OAuthBindAuthorize 绑定模式发起（登录态）：state cookie 标记 bind+uid。
func (h *Handler) OAuthBindAuthorize(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	h.oauthStart(c, oauthModeBind, u.ID)
}

// oauthStart 校验 provider → 生成 state → 写 cookie → 302 authorize URL。
func (h *Handler) oauthStart(c *gin.Context, mode string, uid int64) {
	provider := c.Param("provider")
	cfg, err := h.oauthResolve(c.Request.Context(), provider)
	if err != nil {
		writeErr(c, http.StatusNotFound, "oauth_not_configured", "该第三方登录未配置或不可用")
		return
	}
	state, err := oauthRandomState()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "oauth_start_failed", "发起登录失败")
		return
	}
	oauthSetStateCookie(c, state, mode, uid)
	q := url.Values{}
	q.Set("client_id", cfg.clientID)
	q.Set("redirect_uri", oauthRedirectURI(c.Request, provider))
	q.Set("response_type", "code")
	q.Set("state", state)
	if cfg.scope != "" {
		q.Set("scope", cfg.scope)
	}
	http.Redirect(c.Writer, c.Request, cfg.authorizeURL+"?"+q.Encode(), http.StatusFound)
}

// OAuthCallback provider 回调（公开）。任一步失败一律 302
// /login?oauth_failed=1（不向 URL 暴露细节）；成功路径：
// 命中身份且用户启用 → 签发完整会话 302 /console；bind 模式 → 绑定当前
// 用户 302 /console/profile；未命中且注册开放 → 自动建户 302 /console。
func (h *Handler) OAuthCallback(c *gin.Context) {
	provider := c.Param("provider")
	fail := func() {
		http.Redirect(c.Writer, c.Request, "/login?oauth_failed=1", http.StatusFound)
	}
	cfg, err := h.oauthResolve(c.Request.Context(), provider)
	if err != nil {
		fail()
		return
	}
	// state 校验（防 CSRF）：cookie 中的 state 与 query state 恒时比较；
	// cookie 立即清除（一次性），缺失/过期/篡改均拒绝。
	cookieVal, err := c.Cookie(oauthStateCookie)
	oauthClearStateCookie(c)
	if err != nil || cookieVal == "" {
		fail()
		return
	}
	parts := strings.Split(cookieVal, "|")
	if len(parts) != 3 {
		fail()
		return
	}
	state, mode, bindUIDStr := parts[0], parts[1], parts[2]
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(c.Query("state"))) != 1 {
		fail()
		return
	}
	code := c.Query("code")
	if code == "" {
		fail()
		return
	}
	redirectURI := oauthRedirectURI(c.Request, provider)
	token, err := h.oauthExchangeToken(c.Request.Context(), cfg, code, redirectURI)
	if err != nil || token == "" {
		fail()
		return
	}
	remoteUID, email, err := h.oauthFetchUserinfo(c.Request.Context(), cfg, token)
	if err != nil || remoteUID == "" {
		fail()
		return
	}

	// bind 模式：不签发会话、不自动建户，一律交由绑定逻辑处理
	//（身份已绑给本人视为幂等成功，已绑给他人则拒绝）。
	if mode == oauthModeBind {
		h.oauthCallbackBind(c, provider, remoteUID, bindUIDStr, fail)
		return
	}

	var ident model.UserIdentity
	err = h.st.Read.Where("provider = ? AND provider_uid = ?", provider, remoteUID).
		First(&ident).Error
	switch {
	case err == nil:
		// 命中：用户启用 → 完整会话 302 /console；禁用/删除 → 拒绝。
		var u model.User
		if err := h.st.Read.First(&u, ident.UserID).Error; err != nil || u.Status != model.StatusEnabled {
			fail()
			return
		}
		h.sess.Issue(c, u.ID, u.AuthVersion)
		_ = h.st.Write.Model(&model.User{}).Where("id = ?", u.ID).
			Update("last_login_time", time.Now().Unix()).Error
		http.Redirect(c.Writer, c.Request, "/console", http.StatusFound)
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 未绑定 + 注册开放 → 自动建户（事务：建户 + 身份绑定原子）。
		if !h.rt.GetBool(OptionKeyRegisterEnabled, false) {
			fail()
			return
		}
		u, err := h.oauthAutoRegister(provider, remoteUID, email)
		if err != nil {
			fail()
			return
		}
		h.sess.Issue(c, u.ID, u.AuthVersion)
		http.Redirect(c.Writer, c.Request, "/console", http.StatusFound)
	default:
		fail()
	}
}

// oauthCallbackBind 绑定模式回调：校验当前会话（完整会话 + 与 state cookie
// 中 uid 一致，双保险防 cookie 串改）后落库绑定；身份已绑给本人 → 幂等成功，
// 已绑给他人（复合唯一冲突）与其他失败均拒绝。bind 模式永不签发/顶替会话。
func (h *Handler) oauthCallbackBind(c *gin.Context, provider, remoteUID, bindUIDStr string, fail func()) {
	bindUID, err := strconv.ParseInt(bindUIDStr, 10, 64)
	if err != nil {
		fail()
		return
	}
	sUID, _, stage, ok := h.sess.Verify(c)
	if !ok || stage != stageFull || sUID != bindUID {
		fail()
		return
	}
	var u model.User
	if err := h.st.Read.First(&u, bindUID).Error; err != nil || u.Status != model.StatusEnabled {
		fail()
		return
	}
	var exist model.UserIdentity
	err = h.st.Read.Where("provider = ? AND provider_uid = ?", provider, remoteUID).
		First(&exist).Error
	switch {
	case err == nil && exist.UserID == bindUID:
		// 已绑定本人 → 幂等成功，不重复落库。
		http.Redirect(c.Writer, c.Request, "/console/profile", http.StatusFound)
		return
	case err == nil:
		fail() // 已被他人绑定
		return
	case !errors.Is(err, gorm.ErrRecordNotFound):
		fail()
		return
	}
	ident := model.UserIdentity{
		UserID:      bindUID,
		Provider:    provider,
		ProviderUID: remoteUID,
		CreatedTime: time.Now().Unix(),
	}
	if err := h.st.Write.Create(&ident).Error; err != nil {
		fail() // 含 (provider,provider_uid) 复合唯一冲突
		return
	}
	http.Redirect(c.Writer, c.Request, "/console/profile", http.StatusFound)
}

// oauthAutoRegister 自动建户（事务：users 建户 + user_identities 绑定，
// 任一失败整体回滚——如 username=<provider>_<uid> 撞名）。无口令账号：
// 密码登录不可用，可在个人中心设置口令；email 取 userinfo 提供值，
// 与既有账号冲突时置空（不因邮箱被占阻断登录）。
func (h *Handler) oauthAutoRegister(provider, remoteUID, email string) (model.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email != "" {
		var n int64
		if err := h.st.Read.Model(&model.User{}).Where("email = ?", email).Count(&n).Error; err != nil || n > 0 {
			email = ""
		}
	}
	u := model.User{
		Username:     provider + "_" + remoteUID,
		PasswordHash: "",
		DisplayName:  provider + "_" + remoteUID,
		Role:         model.RoleUser,
		Status:       model.StatusEnabled,
		Quota:        h.rt.GetInt64(OptionKeyRegisterQuotaForNewUser, 0),
		Email:        email,
		Group:        "default",
		AuthVersion:  1,
		AffCode:      generateAffCode(),
		CreatedTime:  time.Now().Unix(),
	}
	err := h.st.Write.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		return tx.Create(&model.UserIdentity{
			UserID:      u.ID,
			Provider:    provider,
			ProviderUID: remoteUID,
			CreatedTime: time.Now().Unix(),
		}).Error
	})
	if err != nil {
		return model.User{}, err
	}
	return u, nil
}

// oauthExchangeToken 用授权码换 access_token：form POST（client_id/secret/
// code/grant_type/redirect_uri），Accept: application/json（GitHub 缺省回
// form 编码，显式声明统一解析）。网络/解析失败返回空 token + 错误。
func (h *Handler) oauthExchangeToken(ctx context.Context, cfg *oauthProviderCfg, code, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("client_id", cfg.clientID)
	form.Set("client_secret", cfg.clientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("oauth: 构造 token 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "hui-api")
	resp, err := h.oauthHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth: 请求 token 端点: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth: token 端点状态码 %d", resp.StatusCode)
	}
	var out struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("oauth: 解析 token 响应: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("oauth: token 端点错误 %s", out.Error)
	}
	return out.AccessToken, nil
}

// oauthFetchUserinfo 拉 userinfo 并提取 uid/email：github 取 id 字段；
// oidc 取 sub（回退 id，兼容 userinfo 非标准实现）。email 缺失不阻断。
// 信任权衡：不验 ID token 签名，身份结论完全依赖 userinfo 响应 + TLS
// 通道（端点来自管理员配置或 HTTPS 发现文档），注记见 docs/11。
func (h *Handler) oauthFetchUserinfo(ctx context.Context, cfg *oauthProviderCfg, token string) (uid, email string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.userinfoURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("oauth: 构造 userinfo 请求: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "hui-api")
	resp, err := h.oauthHTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("oauth: 请求 userinfo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("oauth: userinfo 状态码 %d", resp.StatusCode)
	}
	var claims map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&claims); err != nil {
		return "", "", fmt.Errorf("oauth: 解析 userinfo: %w", err)
	}
	uid = oauthClaimString(claims, cfg.uidField)
	if uid == "" && cfg.uidField != "id" {
		uid = oauthClaimString(claims, "id")
	}
	if v := oauthClaimString(claims, "email"); v != "" {
		email = v
	}
	return uid, email, nil
}

// oauthClaimString 从 userinfo claims 取字符串：字符串直取；数字（JSON
// 解码为 float64）转整数格式（GitHub id 为数值）。
func oauthClaimString(claims map[string]any, field string) string {
	v, ok := claims[field]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	}
	return ""
}

// ListMyIdentities 本人身份绑定列表（登录态，所有权作用域强制会话用户）。
func (h *Handler) ListMyIdentities(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var rows []model.UserIdentity
	if err := h.st.Read.Where("user_id = ?", u.ID).Order("id asc").Find(&rows).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "query_failed", "查询身份绑定失败")
		return
	}
	writeOK(c, gin.H{"items": rows})
}

// DeleteMyIdentity 解绑（登录态）：校验归属（他人身份 404 不泄露存在性）；
// 防锁死——无口令用户解绑最后一个身份前必须先设置口令（或有其他身份）。
func (h *Handler) DeleteMyIdentity(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, "unauthorized", "登录状态无效")
		return
	}
	var ident model.UserIdentity
	if err := h.st.Read.First(&ident, paramID(c)).Error; err != nil || ident.UserID != u.ID {
		writeErr(c, http.StatusNotFound, "not_found", "身份绑定不存在")
		return
	}
	var uRow model.User
	if err := h.st.Read.First(&uRow, u.ID).Error; err != nil {
		writeErr(c, http.StatusNotFound, "not_found", "用户不存在")
		return
	}
	if uRow.PasswordHash == "" {
		var n int64
		if err := h.st.Read.Model(&model.UserIdentity{}).Where("user_id = ?", u.ID).Count(&n).Error; err != nil {
			writeErr(c, http.StatusInternalServerError, "query_failed", "查询身份绑定失败")
			return
		}
		if n <= 1 {
			writeErr(c, http.StatusBadRequest, "identity_last", "请先设置密码或绑定其他身份后再解绑")
			return
		}
	}
	if err := h.st.Write.Delete(&ident).Error; err != nil {
		writeErr(c, http.StatusInternalServerError, "delete_failed", "解绑失败")
		return
	}
	writeOK(c, nil)
}
