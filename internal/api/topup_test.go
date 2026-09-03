// topup_test.go 兑换码核销状态机与额度划转测试（M2-wave3）：
// 覆盖正常核销入账、并发抢码原子性（-race 下仅一人成功）、过期码拒绝与惰性标记、
// 已使用/已作废拒绝、未知码 404、额度划转事务（余额扣减/令牌加额/越权/不足）。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// loginAndCookie 用口令登录并返回会话 cookie（name=value 对）。
func loginAndCookie(t *testing.T, r *gin.Engine, username, password string) string {
	t.Helper()
	w := doJSON(t, r, http.MethodPost, "/api/user/login", "", map[string]string{
		"username": username, "password": password,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("登录应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	return sessionCookieFrom(t, w)
}

// seedRedemption 写入一枚指定状态的兑换码并返回（key 明文）。
func seedRedemption(t *testing.T, st *store.Store, status int, quota, expiredTime int64) string {
	t.Helper()
	code := fmt.Sprintf("redd-test-%d-%d", time.Now().UnixNano(), status)
	r := model.Redemption{
		Key: code, Name: "test", Status: status, Quota: quota,
		ExpiredTime: expiredTime, CreatedTime: time.Now().Unix(),
	}
	if err := st.Write.Create(&r).Error; err != nil {
		t.Fatalf("写入兑换码失败: %v", err)
	}
	return code
}

// topupResp 是核销成功响应的 data 形状。
type topupResp struct {
	QuotaAdded int64 `json:"quota_added"`
	UserQuota  int64 `json:"user_quota"`
}

// respCode 解析管理面错误包裹中的语义码。
func respCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, body)
	}
	return env.Code
}

// userQuota 读取用户当前余额。
func userQuota(t *testing.T, st *store.Store, uid int64) int64 {
	t.Helper()
	var u model.User
	if err := st.Read.First(&u, uid).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	return u.Quota
}

// TestTopupRedeemSuccess 正常核销：入账 + 状态流转 + 归属记录 + 日志；重复核销 409。
func TestTopupRedeemSuccess(t *testing.T) {
	r, st, _ := newTestAPI(t)
	u := seedUser(t, st, "alice", "pw-alice", 1)
	cookie := loginAndCookie(t, r, "alice", "pw-alice")
	code := seedRedemption(t, st, model.RedemptionUnused, 500000, model.EpochForever)

	w := doJSON(t, r, http.MethodPost, "/api/user/topup", cookie, map[string]string{"key": code})
	if w.Code != http.StatusOK {
		t.Fatalf("核销应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var data topupResp
	if err := json.Unmarshal(w.Body.Bytes(), &struct {
		Data *topupResp `json:"data"`
	}{&data}); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if data.QuotaAdded != 500000 || data.UserQuota != 500000 {
		t.Fatalf("入账不符: %+v", data)
	}

	// 库内状态：已核销 + 归属 + 时间；用户余额入账。
	var red model.Redemption
	if err := st.Read.Where("key = ?", code).First(&red).Error; err != nil {
		t.Fatalf("读取兑换码失败: %v", err)
	}
	if red.Status != model.RedemptionRedeemed || red.UsedBy != u.ID || red.UsedTime == 0 {
		t.Fatalf("核销状态不符: %+v", red)
	}
	if got := userQuota(t, st, u.ID); got != 500000 {
		t.Fatalf("用户余额应 500000，实际 %d", got)
	}

	// 重复核销：409 redemption_used，余额不变。
	w2 := doJSON(t, r, http.MethodPost, "/api/user/topup", cookie, map[string]string{"key": code})
	if w2.Code != http.StatusConflict || respCode(t, w2.Body.Bytes()) != "redemption_used" {
		t.Fatalf("重复核销应 409 redemption_used，实际 %d body=%s", w2.Code, w2.Body.String())
	}
	if got := userQuota(t, st, u.ID); got != 500000 {
		t.Fatalf("重复核销不应重复入账，余额 %d", got)
	}

	// topup 日志恰好一条（同事务写入）。
	var logs []model.Log
	if err := st.Read.Where("protocol = ?", "topup").Find(&logs).Error; err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}
	if len(logs) != 1 || logs[0].UserID != u.ID || logs[0].Quota != 500000 {
		t.Fatalf("topup 日志不符: %+v", logs)
	}
}

// TestTopupConcurrentAtomic 并发抢码：N goroutine 同时核销同一码，恰好一人成功、
// 其余 409，余额恰好入账一次（-race 下验证事务条件 UPDATE 的原子性）。
func TestTopupConcurrentAtomic(t *testing.T) {
	r, st, _ := newTestAPI(t)
	u := seedUser(t, st, "bob", "pw-bob", 1)
	cookie := loginAndCookie(t, r, "bob", "pw-bob")
	code := seedRedemption(t, st, model.RedemptionUnused, 123456, model.EpochForever)

	const n = 12
	start := make(chan struct{})
	results := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // 全员就位后同时发起
			w := doJSON(t, r, http.MethodPost, "/api/user/topup", cookie, map[string]string{"key": code})
			results[idx] = w.Code
		}(i)
	}
	close(start)
	wg.Wait()

	ok, conflict := 0, 0
	for _, c := range results {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("并发核销出现意外状态 %d: %v", c, results)
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("应恰好 1 人成功 %d 人 409，实际 ok=%d conflict=%d", n-1, ok, conflict)
	}
	if got := userQuota(t, st, u.ID); got != 123456 {
		t.Fatalf("并发下应恰好入账一次（123456），实际 %d", got)
	}
}

// TestTopupExpired 过期码：拒绝核销、惰性标记 status=已过期、余额不变。
func TestTopupExpired(t *testing.T) {
	r, st, _ := newTestAPI(t)
	u := seedUser(t, st, "carol", "pw-carol", 1)
	cookie := loginAndCookie(t, r, "carol", "pw-carol")
	code := seedRedemption(t, st, model.RedemptionUnused, 1000, time.Now().Unix()-100)

	w := doJSON(t, r, http.MethodPost, "/api/user/topup", cookie, map[string]string{"key": code})
	if w.Code != http.StatusBadRequest || respCode(t, w.Body.Bytes()) != "redemption_expired" {
		t.Fatalf("过期码应 400 redemption_expired，实际 %d body=%s", w.Code, w.Body.String())
	}
	var red model.Redemption
	if err := st.Read.Where("key = ?", code).First(&red).Error; err != nil {
		t.Fatalf("读取兑换码失败: %v", err)
	}
	if red.Status != model.RedemptionExpired {
		t.Fatalf("过期码应被惰性标记为已过期(4)，实际 %d", red.Status)
	}
	if got := userQuota(t, st, u.ID); got != 0 {
		t.Fatalf("过期核销不应入账，余额 %d", got)
	}
}

// TestTopupUnknownAndVoided 未知码 404；作废码 400 redemption_voided。
func TestTopupUnknownAndVoided(t *testing.T) {
	r, st, _ := newTestAPI(t)
	seedUser(t, st, "dave", "pw-dave", 1)
	cookie := loginAndCookie(t, r, "dave", "pw-dave")

	w := doJSON(t, r, http.MethodPost, "/api/user/topup", cookie, map[string]string{"key": "redd-not-exist"})
	if w.Code != http.StatusNotFound || respCode(t, w.Body.Bytes()) != "not_found" {
		t.Fatalf("未知码应 404，实际 %d body=%s", w.Code, w.Body.String())
	}
	voided := seedRedemption(t, st, model.RedemptionVoided, 1000, model.EpochForever)
	w2 := doJSON(t, r, http.MethodPost, "/api/user/topup", cookie, map[string]string{"key": voided})
	if w2.Code != http.StatusBadRequest || respCode(t, w2.Body.Bytes()) != "redemption_voided" {
		t.Fatalf("作废码应 400 redemption_voided，实际 %d body=%s", w2.Code, w2.Body.String())
	}
	// 缺 key 400。
	w3 := doJSON(t, r, http.MethodPost, "/api/user/topup", cookie, map[string]string{"key": " "})
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("空 key 应 400，实际 %d", w3.Code)
	}
}

// seedTokenForUser 为指定用户写入一枚令牌并返回（id, 明文 key）。
func seedTokenForUser(t *testing.T, st *store.Store, uid int64, remain, quota int64, unlimited bool) (int64, string) {
	t.Helper()
	plain, err := generateTokenKey()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tok := model.Token{
		UserID: uid, Name: "assign-test", Key: plain, KeyHash: gateway.HashKey(plain),
		Status: model.StatusEnabled, Quota: quota, RemainQuota: remain,
		UnlimitedQuota: unlimited, ExpiredTime: model.EpochForever,
	}
	if err := st.Write.Create(&tok).Error; err != nil {
		t.Fatalf("写入令牌失败: %v", err)
	}
	return tok.ID, plain
}

// TestAssignQuotaTransfer 额度划转：用户余额扣减、令牌 remain/quota 同步增加、日志落库。
func TestAssignQuotaTransfer(t *testing.T) {
	r, st, _ := newTestAPI(t)
	u := seedUser(t, st, "erin", "pw-erin", 1)
	if err := st.Write.Model(&model.User{}).Where("id = ?", u.ID).Update("quota", 1000).Error; err != nil {
		t.Fatalf("设置余额失败: %v", err)
	}
	cookie := loginAndCookie(t, r, "erin", "pw-erin")
	tokID, _ := seedTokenForUser(t, st, u.ID, 10, 100, false)

	w := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/token/%d/assign", tokID), cookie, map[string]int64{"quota": 400})
	if w.Code != http.StatusOK {
		t.Fatalf("划转应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if got := userQuota(t, st, u.ID); got != 600 {
		t.Fatalf("用户余额应 600，实际 %d", got)
	}
	var tok model.Token
	if err := st.Read.First(&tok, tokID).Error; err != nil {
		t.Fatalf("读取令牌失败: %v", err)
	}
	if tok.RemainQuota != 410 || tok.Quota != 500 {
		t.Fatalf("令牌加额不符: remain=%d quota=%d", tok.RemainQuota, tok.Quota)
	}
	var logs []model.Log
	if err := st.Read.Where("model_name = ?", "token_assign").Find(&logs).Error; err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}
	if len(logs) != 1 || logs[0].TokenID != tokID || logs[0].Quota != 400 {
		t.Fatalf("划转日志不符: %+v", logs)
	}
}

// TestAssignQuotaRejections 划转拒绝路径：余额不足/越权/管理员放行/unlimited/非法值。
func TestAssignQuotaRejections(t *testing.T) {
	r, st, _ := newTestAPI(t)
	owner := seedUser(t, st, "frank", "pw-frank", 1)
	if err := st.Write.Model(&model.User{}).Where("id = ?", owner.ID).Update("quota", 100).Error; err != nil {
		t.Fatalf("设置余额失败: %v", err)
	}
	seedUser(t, st, "grace", "pw-grace", 1)
	seedUser(t, st, "adminx", "pw-adminx", 100)
	ownerCookie := loginAndCookie(t, r, "frank", "pw-frank")
	otherCookie := loginAndCookie(t, r, "grace", "pw-grace")
	adminCookie := loginAndCookie(t, r, "adminx", "pw-adminx")

	tokID, _ := seedTokenForUser(t, st, owner.ID, 0, 0, false)
	unlimID, _ := seedTokenForUser(t, st, owner.ID, 0, 0, true)

	// 余额不足：400 insufficient_quota，余额/令牌均不变。
	w := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/token/%d/assign", tokID), ownerCookie, map[string]int64{"quota": 999999})
	if w.Code != http.StatusBadRequest || respCode(t, w.Body.Bytes()) != "insufficient_quota" {
		t.Fatalf("余额不足应 400 insufficient_quota，实际 %d body=%s", w.Code, w.Body.String())
	}
	if got := userQuota(t, st, owner.ID); got != 100 {
		t.Fatalf("失败划转不应扣余额，实际 %d", got)
	}
	// 非归属者：403；管理员代划：200。
	w2 := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/token/%d/assign", tokID), otherCookie, map[string]int64{"quota": 50})
	if w2.Code != http.StatusForbidden || respCode(t, w2.Body.Bytes()) != "forbidden" {
		t.Fatalf("越权应 403，实际 %d body=%s", w2.Code, w2.Body.String())
	}
	w3 := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/token/%d/assign", tokID), adminCookie, map[string]int64{"quota": 50})
	if w3.Code != http.StatusOK {
		t.Fatalf("管理员代划应 200，实际 %d body=%s", w3.Code, w3.Body.String())
	}
	// unlimited 令牌：400。
	w4 := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/token/%d/assign", unlimID), ownerCookie, map[string]int64{"quota": 10})
	if w4.Code != http.StatusBadRequest {
		t.Fatalf("unlimited 划转应 400，实际 %d", w4.Code)
	}
	// 非法值：400。
	w5 := doJSON(t, r, http.MethodPost, fmt.Sprintf("/api/token/%d/assign", tokID), ownerCookie, map[string]int64{"quota": 0})
	if w5.Code != http.StatusBadRequest {
		t.Fatalf("quota=0 应 400，实际 %d", w5.Code)
	}
	// 不存在的令牌：404。
	w6 := doJSON(t, r, http.MethodPost, "/api/token/99999/assign", ownerCookie, map[string]int64{"quota": 10})
	if w6.Code != http.StatusNotFound {
		t.Fatalf("令牌不存在应 404，实际 %d", w6.Code)
	}
}

// TestSelfEndpoint 自身信息端点：登录可读、未登录 401。
func TestSelfEndpoint(t *testing.T) {
	r, st, _ := newTestAPI(t)
	u := seedUser(t, st, "henry", "pw-henry", 1)
	cookie := loginAndCookie(t, r, "henry", "pw-henry")

	w := doJSON(t, r, http.MethodGet, "/api/user/self", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("self 应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var self model.User
	if err := json.Unmarshal([]byte(w.Body.String()), &struct {
		Data *model.User `json:"data"`
	}{&self}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if self.ID != u.ID || self.Username != "henry" {
		t.Fatalf("self 数据不符: %+v", self)
	}
	w2 := doJSON(t, r, http.MethodGet, "/api/user/self", "", nil)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w2.Code)
	}
}
