// Package api 实现管理面 HTTP API（M2-wave1，docs/05）：
// 登录会话（HMAC 签名 cookie + auth_version 失效机制）、root 管理中间件，
// 以及渠道/令牌/用户/兑换码/日志/配置的管理端点。
//
// 响应统一为 {"success":bool,"message":string,"data":...}；鉴权失败 401、
// 权限不足 403、请求非法 400。
package api

import "golang.org/x/crypto/bcrypt"

// HashPassword 生成 bcrypt 哈希（默认 cost，单次 ~60ms，兼顾安全与登录体验）。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword 校验明文口令与 bcrypt 哈希是否匹配。
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
