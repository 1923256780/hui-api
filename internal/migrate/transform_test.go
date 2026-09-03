// transform 层全覆盖测试：正反例 + 幂等确定性 + ModelRatio 换算黄金锚定
// （锚点取自旧库真实账单，见 ADR-0008：两计费口径下同 tokens 同 quota）。
package migrate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/1923256780/hui-api/internal/billing"
	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/override"
)

func strPtr(s string) *string { return &s }
func i64Ptr(v int64) *int64   { return &v }

// bcrypt 返回一个合法形状的标准 bcrypt 哈希（恒 60 字符：$2a$10$ + 53 位哈希体）。
func bcrypt() string      { return "$2a$10$" + strings.Repeat("a", 53) }
func legacyKey48() string { return "aPQfFaUf2hNTPA6PHuY8k9GDYOuKFfWgj9PTixAUuDsaKSwZ" }
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ---- users ----

func TestTransformUserOK(t *testing.T) {
	u, err := TransformUser(legacyUser{
		ID: 3, Username: "tangjiamin", Password: bcrypt(), DisplayName: strPtr("老唐"),
		Role: 1, Status: 1, Email: strPtr("t@example.com"), Quota: 5000, UsedQuota: 34144,
		Group: nil, CreatedAt: i64Ptr(1788160000), LastLoginAt: i64Ptr(1788343675),
	})
	if err != nil {
		t.Fatalf("合法用户变换失败: %v", err)
	}
	if u.ID != 3 || u.Username != "tangjiamin" || u.PasswordHash != bcrypt() {
		t.Fatalf("基础列迁移错误: %+v", u)
	}
	if u.Group != "default" { // group 空 → default
		t.Fatalf("空 group 应为 default，实际 %q", u.Group)
	}
	if u.DisplayName != "老唐" || u.Email != "t@example.com" {
		t.Fatalf("可选列迁移错误: %+v", u)
	}
	if u.CreatedTime != 1788160000 || u.LastLoginTime != 1788343675 {
		t.Fatalf("时间列迁移错误: %+v", u)
	}
	// aff/邀请/两步验证相关列不迁移，保持零值。
	if u.AffCode != "" || u.InviterID != 0 || u.AffHistoryQuota != 0 || u.AuthVersion != 0 || u.TOTPEnabled {
		t.Fatalf("aff/会话列应保持零值: %+v", u)
	}
}

func TestTransformUserGroupKept(t *testing.T) {
	u, err := TransformUser(legacyUser{ID: 2, Username: "test1", Password: bcrypt(), Role: 1, Status: 1, Group: strPtr("vip")})
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if u.Group != "vip" {
		t.Fatalf("非空 group 应保留，实际 %q", u.Group)
	}
}

func TestTransformUserBcryptGuard(t *testing.T) {
	for _, bad := range []string{"", "plaintext", "$2b$10$somehash", "$2y$10$somehash", "$2a$"} {
		if _, err := TransformUser(legacyUser{ID: 1, Username: "root", Password: bad, Role: 100, Status: 1}); err == nil {
			t.Fatalf("密码 %q 缺少 $2a$ 前缀应硬失败", bad)
		} else if !strings.Contains(err.Error(), "拒绝迁移") {
			t.Fatalf("错误信息应说明拒绝迁移: %v", err)
		}
	}
}

func TestTransformUserStatusGuard(t *testing.T) {
	if _, err := TransformUser(legacyUser{ID: 1, Username: "x", Password: bcrypt(), Status: 7}); err == nil {
		t.Fatal("未知用户状态应硬失败")
	}
}

// ---- tokens ----

func TestTransformTokenOK(t *testing.T) {
	tok, err := TransformToken(legacyToken{
		ID: 3, UserID: 3, Key: legacyKey48(), Status: 1, Name: strPtr("key"),
		CreatedTime: i64Ptr(1788339851), AccessedTime: i64Ptr(1788343675),
		ExpiredTime: i64Ptr(-1), RemainQuota: i64Ptr(-34144), UnlimitedQuota: 1,
	}, "default")
	if err != nil {
		t.Fatalf("合法令牌变换失败: %v", err)
	}
	plain := TokenPlainPrefix + legacyKey48()
	if tok.Key != plain {
		t.Fatalf("明文应为 sk-+48 位，实际 %q", tok.Key)
	}
	if tok.KeyHash != gateway.HashKey(plain) { // 口径必须与运行时鉴权一致
		t.Fatalf("key_hash 必须等于 gateway.HashKey(plain)")
	}
	if !tok.UnlimitedQuota || tok.Quota != 0 || tok.RemainQuota != 0 {
		t.Fatalf("unlimited 应归一化为 quota=0: %+v", tok)
	}
	if tok.Group != "default" {
		t.Fatalf("令牌 group 空+用户组 default 应为 default，实际 %q", tok.Group)
	}
	if tok.ExpiredTime != model.EpochForever {
		t.Fatalf("expired_time=-1 应保持 EpochForever，实际 %d", tok.ExpiredTime)
	}
	if tok.Status != model.StatusEnabled {
		t.Fatalf("status=1 应为启用，实际 %d", tok.Status)
	}
}

func TestTransformTokenGroupInheritance(t *testing.T) {
	// 空 group 继承用户组。
	tok, err := TransformToken(legacyToken{ID: 1, UserID: 2, Key: legacyKey48(), Status: 1, UnlimitedQuota: 1}, "vip")
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if tok.Group != "vip" {
		t.Fatalf("应继承用户组 vip，实际 %q", tok.Group)
	}
	// 令牌自带 group 优先于用户组。
	tok2, err := TransformToken(legacyToken{ID: 1, UserID: 2, Key: legacyKey48(), Status: 1, UnlimitedQuota: 1, Group: strPtr("trial")}, "vip")
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if tok2.Group != "trial" {
		t.Fatalf("令牌自带 group 应优先，实际 %q", tok2.Group)
	}
	// 用户组也空 → default。
	tok3, err := TransformToken(legacyToken{ID: 1, UserID: 9, Key: legacyKey48(), Status: 1, UnlimitedQuota: 1}, "")
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if tok3.Group != "default" {
		t.Fatalf("双侧 group 均空应为 default，实际 %q", tok3.Group)
	}
}

func TestTransformTokenBudgetSnapshot(t *testing.T) {
	tok, err := TransformToken(legacyToken{ID: 5, UserID: 1, Key: legacyKey48(), Status: 1, RemainQuota: i64Ptr(700000), UnlimitedQuota: 0}, "default")
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if tok.UnlimitedQuota {
		t.Fatal("非无限令牌不应标记 unlimited")
	}
	if tok.Quota != 700000 || tok.RemainQuota != 700000 {
		t.Fatalf("非无限令牌预算快照应 quota=remain=旧 remain_quota，实际 quota=%d remain=%d", tok.Quota, tok.RemainQuota)
	}
}

func TestTransformTokenStatusMapping(t *testing.T) {
	cases := map[int]int{1: model.StatusEnabled, 2: model.StatusDisabled, 3: model.StatusEnabled, 4: model.StatusEnabled}
	for legacyStatus, want := range cases {
		tok, err := TransformToken(legacyToken{ID: 1, UserID: 1, Key: legacyKey48(), Status: legacyStatus, UnlimitedQuota: 1}, "default")
		if err != nil {
			t.Fatalf("status=%d 变换失败: %v", legacyStatus, err)
		}
		if tok.Status != want {
			t.Fatalf("status %d 应映射为 %d，实际 %d", legacyStatus, want, tok.Status)
		}
	}
	if _, err := TransformToken(legacyToken{ID: 1, UserID: 1, Key: legacyKey48(), Status: 9, UnlimitedQuota: 1}, "default"); err == nil {
		t.Fatal("未知令牌状态应硬失败")
	}
}

func TestTransformTokenGuards(t *testing.T) {
	if _, err := TransformToken(legacyToken{ID: 1, UserID: 1, Key: "  ", Status: 1, UnlimitedQuota: 1}, "default"); err == nil {
		t.Fatal("空 key 应硬失败")
	}
	tok, err := TransformToken(legacyToken{
		ID: 2, UserID: 1, Key: legacyKey48(), Status: 1, UnlimitedQuota: 0,
		ModelLimitsEnabled: i64Ptr(1), ModelLimits: strPtr("glm-5.3-flash,deepseek-v4-flash"),
		AllowIPs: strPtr("1.2.3.4"), ExpiredTime: i64Ptr(0),
	}, "default")
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if tok.ModelLimits != "glm-5.3-flash,deepseek-v4-flash" || tok.AllowIPs != "1.2.3.4" {
		t.Fatalf("model_limits/allow_ips 应直迁: %+v", tok)
	}
	if tok.ExpiredTime != model.EpochForever {
		t.Fatalf("expired_time=0 应归一化为 EpochForever，实际 %d", tok.ExpiredTime)
	}
	// 未启用模型白名单 → 空。
	tok2, err := TransformToken(legacyToken{ID: 3, UserID: 1, Key: legacyKey48(), Status: 1, UnlimitedQuota: 1,
		ModelLimitsEnabled: i64Ptr(0), ModelLimits: strPtr("glm-5.3-flash")}, "default")
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if tok2.ModelLimits != "" {
		t.Fatalf("model_limits 未启用应为空，实际 %q", tok2.ModelLimits)
	}
}

// ---- param_override：envelope → 扁平 ops ----

// 实测旧库渠道 #3/#4 的 envelope 原文。
const realEnvelope = `{"mode":"advanced","operations":[{"mode":"delete","path":"thinking"},{"mode":"set","path":"thinking","value":{"type":"enabled","effort":"max"}}]}`

func TestTransformParamOverrideRealEnvelope(t *testing.T) {
	got, err := TransformParamOverride(realEnvelope)
	if err != nil {
		t.Fatalf("实测 envelope 变换失败: %v", err)
	}
	want := `{"delete":["thinking"],"set":{"thinking":{"effort":"max","type":"enabled"}},"append":{},"replace":{},"regex_replace":{}}`
	if got != want {
		t.Fatalf("扁平 ops 与期望不符:\n got  %s\n want %s", got, want)
	}
	// 幂等：重跑结果一致。
	got2, err := TransformParamOverride(realEnvelope)
	if err != nil || got2 != got {
		t.Fatalf("变换应确定性: %v %s", err, got2)
	}
}

func TestTransformParamOverrideAllOps(t *testing.T) {
	raw := `{"mode":"advanced","operations":[
		{"mode":"delete","path":"a.b"},
		{"mode":"set","path":"x","value":123},
		{"mode":"append","path":"sys","value":"tail"},
		{"mode":"replace","path":"m","old":"old","new":"new"},
		{"mode":"regex_replace","path":"p","pattern":"a+","replacement":"b"}]}`
	got, err := TransformParamOverride(raw)
	if err != nil {
		t.Fatalf("全操作 envelope 变换失败: %v", err)
	}
	want := `{"delete":["a.b"],"set":{"x":123},"append":{"sys":"tail"},"replace":{"m":{"old":"old","new":"new"}},"regex_replace":{"p":{"pattern":"a+","replacement":"b"}}}`
	if got != want {
		t.Fatalf("扁平 ops 与期望不符:\n got  %s\n want %s", got, want)
	}
	// 语义冒烟：hui override 管道对样例请求体的应用结果与 envelope 语义一致。
	body := []byte(`{"a":{"b":1},"x":0,"sys":"head","m":"old-old","p":"aaa"}`)
	out, err := override.Apply(body, got)
	if err != nil {
		t.Fatalf("hui 管道应用失败: %v", err)
	}
	wantBody := mustJSON(t, map[string]any{
		"a": map[string]any{}, "x": float64(123), "sys": "headtail", "m": "new-new", "p": "b",
	})
	if string(out) != wantBody {
		t.Fatalf("应用结果不符:\n got  %s\n want %s", out, wantBody)
	}
}

func TestTransformParamOverrideRejects(t *testing.T) {
	cases := map[string]string{
		"空 path(delete)":  `{"mode":"advanced","operations":[{"mode":"delete","path":" "}]}`,
		"set 缺 value":     `{"mode":"advanced","operations":[{"mode":"set","path":"x"}]}`,
		"未知 mode":         `{"mode":"advanced","operations":[{"mode":"insert","path":"x","value":1}]}`,
		"mode 非 advanced": `{"mode":"basic","operations":[]}`,
		"非法 JSON":         `{not-json`,
		"非法正则":            `{"mode":"advanced","operations":[{"mode":"regex_replace","path":"p","pattern":"([a","replacement":"b"}]}`,
		"operations 缺失":   `{"mode":"advanced"}`,
		"value 非法 JSON":   `{"mode":"advanced","operations":[{"mode":"set","path":"x","value":nope}]}`,
	}
	for name, raw := range cases {
		if _, err := TransformParamOverride(raw); err == nil {
			t.Fatalf("%s 应硬失败", name)
		}
	}
	if got, err := TransformParamOverride(""); err != nil || got != "" {
		t.Fatalf("空输入应得空串: %q %v", got, err)
	}
	if got, err := TransformParamOverride("   "); err != nil || got != "" {
		t.Fatalf("纯空白输入应得空串: %q %v", got, err)
	}
}

// ---- param_override：顺序等价性守卫 ----

// applyEnvelopeInOrder 按数组顺序逐个应用单操作，模拟旧 envelope 的顺序语义：
// 每个单操作 envelope 经 TransformParamOverride 得扁平产物后走真实 override.Apply，
// 保证模拟口径与 hui 管道完全一致（单操作序列天然通过顺序等价检查）。
func applyEnvelopeInOrder(t *testing.T, body []byte, opsJSON string) []byte {
	t.Helper()
	var env legacyOverrideEnvelope
	if err := json.Unmarshal([]byte(opsJSON), &env); err != nil {
		t.Fatalf("解析测试 envelope 失败: %v", err)
	}
	for _, op := range *env.Operations {
		single, err := json.Marshal(legacyOverrideEnvelope{Mode: "advanced",
			Operations: &[]legacyOverrideOp{op}})
		if err != nil {
			t.Fatalf("marshal 单操作: %v", err)
		}
		flat, err := TransformParamOverride(string(single))
		if err != nil {
			t.Fatalf("单操作变换失败（%s %s）: %v", op.Mode, op.Path, err)
		}
		body, err = override.Apply(body, flat)
		if err != nil {
			t.Fatalf("顺序应用失败（%s %s）: %v", op.Mode, op.Path, err)
		}
	}
	return body
}

// TestTransformParamOverrideOrderRejects 顺序等价性守卫反例：归桶后语义漂移的
// 序列必须硬失败（错误信息说明不可等价），绝不静默产出错误语义的扁平 ops。
// 覆盖评审指出的四类反转/丢失样例 + delete 收尾复活样例。
func TestTransformParamOverrideOrderRejects(t *testing.T) {
	cases := map[string]string{
		// 反转类 1：set→delete——旧语义 X 被删，扁平会 delete→set 使其复活。
		"set后delete反转": `{"mode":"advanced","operations":[
			{"mode":"set","path":"x","value":1},{"mode":"delete","path":"x"}]}`,
		// 丢失类 2：同 path 两次 append——归桶 map 仅留最后一个，前值丢失。
		"append多次丢失": `{"mode":"advanced","operations":[
			{"mode":"append","path":"a","value":"A"},{"mode":"append","path":"a","value":"B"}]}`,
		// 丢失类 3：同 path 链式 replace——归桶仅留最后一个改写，前段丢失。
		"replace链丢失": `{"mode":"advanced","operations":[
			{"mode":"replace","path":"m","old":"x","new":"y"},
			{"mode":"replace","path":"m","old":"y","new":"z"}]}`,
		// 反转类 4：append→set——旧语义最终为 set 值，扁平为 set 后再 append。
		"append后set反转": `{"mode":"advanced","operations":[
			{"mode":"append","path":"a","value":"A"},{"mode":"set","path":"a","value":"S"}]}`,
		// 补充：delete 之后的 set 又被 delete 收尾——prefix 的 set 类别在 tail 缺失，
		// 扁平化为 delete→set 后 X 复活，与旧语义（最终被删）相反。
		"delete收尾复活": `{"mode":"advanced","operations":[
			{"mode":"delete","path":"x"},{"mode":"set","path":"x","value":1},{"mode":"delete","path":"x"}]}`,
	}
	for name, raw := range cases {
		got, err := TransformParamOverride(raw)
		if err == nil {
			t.Fatalf("%s 应硬失败，实际产出: %s", name, got)
		}
		if !strings.Contains(err.Error(), "不可等价") {
			t.Fatalf("%s 错误信息应说明不可等价: %v", name, err)
		}
	}
}

// TestTransformParamOverrideOrderRejectsMixedPath 多 path 混合：仅一个 path 违规
// 也必须整体硬失败（渠道行级拒绝，不做部分迁移）。
func TestTransformParamOverrideOrderRejectsMixedPath(t *testing.T) {
	raw := `{"mode":"advanced","operations":[
		{"mode":"delete","path":"clean"},{"mode":"set","path":"clean","value":true},
		{"mode":"set","path":"bad","value":1},{"mode":"delete","path":"bad"}]}`
	got, err := TransformParamOverride(raw)
	if err == nil {
		t.Fatalf("混合 path 中 bad 违规应整体硬失败，实际产出: %s", got)
	}
	if !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("错误信息应指出违规 path: %v", err)
	}
}

// TestTransformParamOverrideOrderAllows 可等价放行样例：迁移成功（产物经内置
// override.Parse 自校验），且扁平产物在真实 hui 管道上的应用结果与旧 envelope
// 顺序语义逐字节一致（语义级等价验证，非仅结构校验）。
func TestTransformParamOverrideOrderAllows(t *testing.T) {
	cases := map[string]string{
		// delete→set（实测旧库 #3/#4 的真实序列）。
		"delete后set": realEnvelope,
		// delete 后重新开始：prefix set 被 tail set 覆盖，等价 delete→set。
		"set-delete-set": `{"mode":"advanced","operations":[
			{"mode":"set","path":"x","value":1},{"mode":"delete","path":"x"},
			{"mode":"set","path":"x","value":2}]}`,
		// delete 后重新开始：prefix append 被 tail append 覆盖，等价 delete→append。
		"append-delete-append": `{"mode":"advanced","operations":[
			{"mode":"append","path":"p","value":"A"},{"mode":"delete","path":"p"},
			{"mode":"append","path":"p","value":"B"}]}`,
		// 同 path 各类严格顺序单次（set→append→replace→regex_replace）。
		"同类严格顺序": `{"mode":"advanced","operations":[
			{"mode":"set","path":"p","value":"v"},{"mode":"append","path":"p","value":"a"},
			{"mode":"replace","path":"p","old":"v","new":"w"},
			{"mode":"regex_replace","path":"p","pattern":"a","replacement":"b"}]}`,
		// 纯多次 delete：幂等无害。
		"多次delete": `{"mode":"advanced","operations":[
			{"mode":"delete","path":"a"},{"mode":"delete","path":"a"}]}`,
	}
	body := []byte(`{"x":0,"p":"seed","a":[1],"m":"x,y","thinking":{"type":"disabled"}}`)
	for name, raw := range cases {
		got, err := TransformParamOverride(raw)
		if err != nil {
			t.Fatalf("%s 应放行: %v", name, err)
		}
		if _, err := override.Parse(got); err != nil {
			t.Fatalf("%s 产物应可被 override.Parse 解析: %v", name, err)
		}
		// 语义等价：旧顺序语义模拟执行 vs 扁平产物单次应用，结果必须逐字节一致。
		want := applyEnvelopeInOrder(t, body, raw)
		have, err := override.Apply(body, got)
		if err != nil {
			t.Fatalf("%s 扁平产物应用失败: %v", name, err)
		}
		if string(have) != string(want) {
			t.Fatalf("%s 语义漂移:\n 旧顺序语义 %s\n 扁平产物   %s", name, want, have)
		}
	}
}

// ---- channels ----

func TestTransformChannelOpenAICompat(t *testing.T) {
	res, err := TransformChannel(legacyChannel{
		ID: 4, Type: 1, Key: "sk-upstream", Status: 1, Name: strPtr("opencode-go"),
		BaseURL: strPtr("https://opencode.ai/zen/go"),
		Models:  strPtr("glm-5.3-flash,deepseek-v4-flash"), Group: strPtr("default,vip,trial"),
		Priority: 9, ParamOverride: strPtr(realEnvelope), CreatedTime: i64Ptr(1788163957),
	})
	if err != nil {
		t.Fatalf("渠道变换失败: %v", err)
	}
	ch := res.Channel
	if ch.Type != model.ChannelTypeOpenAICompatible || ch.Status != model.StatusEnabled {
		t.Fatalf("type/status 映射错误: %+v", ch)
	}
	if ch.BaseURL != "https://opencode.ai/zen/go" || ch.Priority != 9 || ch.Weight != 0 {
		t.Fatalf("直迁列错误: %+v", ch)
	}
	if !strings.HasPrefix(ch.ParamOverride, `{"delete":["thinking"]`) {
		t.Fatalf("param_override 应为扁平 ops: %s", ch.ParamOverride)
	}
	if !res.GroupDropped {
		t.Fatal("多组信息应记 GroupDropped")
	}
	if res.ModelMappingRaw != "" {
		t.Fatalf("无 model_mapping 不应有缺口: %q", res.ModelMappingRaw)
	}
}

func TestTransformChannelBaseURLFallback(t *testing.T) {
	cases := map[int]string{
		legacyTypeDeepSeek:   "https://api.deepseek.com",
		legacyTypeOpenRouter: "https://openrouter.ai/api/v1",
		legacyTypeArk:        "https://ark.cn-beijing.volces.com/api/v1",
	}
	for typ, want := range cases {
		res, err := TransformChannel(legacyChannel{ID: 2, Type: typ, Key: "k", Status: 2, Models: strPtr("m")})
		if err != nil {
			t.Fatalf("type=%d 变换失败: %v", typ, err)
		}
		if res.Channel.BaseURL != want {
			t.Fatalf("type=%d 空 base_url 应补 %s，实际 %s", typ, want, res.Channel.BaseURL)
		}
		if res.Channel.Type != model.ChannelTypeOpenAICompatible {
			t.Fatalf("type=%d 应映射为 1，实际 %d", typ, res.Channel.Type)
		}
		if res.Channel.Status != model.StatusDisabled {
			t.Fatalf("旧 status=2 应为禁用，实际 %d", res.Channel.Status)
		}
	}
}

func TestTransformChannelModelMappingGap(t *testing.T) {
	res, err := TransformChannel(legacyChannel{ID: 6, Type: legacyTypeOpenRouter, Key: "k", Status: 2,
		ModelMapping: strPtr("{\n  \"glm-5.3-flash\": \"z-ai/glm-5.3-flash\"\n}"), Models: strPtr("glm-5.3-flash")})
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if res.ModelMappingRaw == "" {
		t.Fatal("model_mapping 非空应记缺口")
	}
	if !strings.Contains(res.ModelMappingRaw, "z-ai/glm-5.3-flash") {
		t.Fatalf("缺口应含原文摘要: %q", res.ModelMappingRaw)
	}
	if strings.Contains(res.ModelMappingRaw, "\n") {
		t.Fatalf("缺口摘要应为单行: %q", res.ModelMappingRaw)
	}
}

func TestTransformChannelRejects(t *testing.T) {
	if _, err := TransformChannel(legacyChannel{ID: 8, Type: 99, Key: "k", Status: 1}); err == nil {
		t.Fatal("未知渠道类型应硬失败")
	}
	if _, err := TransformChannel(legacyChannel{ID: 8, Type: 1, Key: "k", Status: 9}); err == nil {
		t.Fatal("未知渠道状态应硬失败")
	}
	if _, err := TransformChannel(legacyChannel{ID: 8, Type: 5, Key: "k", Status: 1}); err == nil {
		t.Fatal("空 base_url 且无官方端点映射应硬失败")
	}
	if _, err := TransformChannel(legacyChannel{ID: 8, Type: 1, Key: "k", Status: 1,
		ParamOverride: strPtr(`{"mode":"basic","operations":[]}`)}); err == nil {
		t.Fatal("非 envelope 的 param_override 应硬失败该行")
	}
}

// ---- redemptions ----

func TestTransformRedemptionStatusMapping(t *testing.T) {
	now := int64(1789000000)
	// 已核销 → hui 2。
	r, err := TransformRedemption(legacyRedemption{ID: 1, UserID: i64Ptr(1), Key: "ceb1", Status: 3,
		Name: strPtr("t6-test-01"), Quota: 100000, CreatedTime: i64Ptr(1788259604),
		RedeemedTime: i64Ptr(1788319022), UsedUserID: i64Ptr(1), ExpiredTime: i64Ptr(1788866655)}, now)
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if r.Status != model.RedemptionRedeemed || r.UsedBy != 1 || r.UsedTime != 1788319022 || r.CreatedBy != 1 {
		t.Fatalf("已核销映射错误: %+v", r)
	}
	if r.ExpiredTime != 1788866655 {
		t.Fatalf("未来过期时间应保留: %d", r.ExpiredTime)
	}
	// 未核销未过期 → hui 1。
	r2, err := TransformRedemption(legacyRedemption{ID: 2, Key: "k2", Status: 1, Quota: 50, ExpiredTime: i64Ptr(now + 100)}, now)
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if r2.Status != model.RedemptionUnused {
		t.Fatalf("未核销未过期应为未使用，实际 %d", r2.Status)
	}
	// 过期未核销 → hui 4。
	r3, err := TransformRedemption(legacyRedemption{ID: 3, Key: "k3", Status: 1, Quota: 50, ExpiredTime: i64Ptr(now - 100)}, now)
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if r3.Status != model.RedemptionExpired {
		t.Fatalf("过期未核销应为已过期，实际 %d", r3.Status)
	}
	// 禁用 → 作废(3)。
	r4, err := TransformRedemption(legacyRedemption{ID: 4, Key: "k4", Status: 2, Quota: 50}, now)
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if r4.Status != model.RedemptionVoided {
		t.Fatalf("禁用兑换码应映射为作废，实际 %d", r4.Status)
	}
	// expired_time=0 → -1。
	r5, err := TransformRedemption(legacyRedemption{ID: 5, Key: "k5", Status: 3, Quota: 6849315068, ExpiredTime: i64Ptr(0)}, now)
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if r5.ExpiredTime != model.EpochForever {
		t.Fatalf("expired_time=0 应为 EpochForever，实际 %d", r5.ExpiredTime)
	}
	// 未知状态硬失败。
	if _, err := TransformRedemption(legacyRedemption{ID: 9, Key: "k9", Status: 9, Quota: 1}, now); err == nil {
		t.Fatal("未知兑换码状态应硬失败")
	}
}

// ---- logs ----

func TestTransformConsumeLog(t *testing.T) {
	other := `{"model_ratio":0.32,"completion_ratio":2,"cache_tokens":0}`
	lg, outcome, err := TransformConsumeLog(legacyLog{
		ID: 1137, UserID: 3, CreatedAt: 1788343600, Type: 2, ModelName: strPtr("deepseek-v4-flash"),
		Quota: 85, PromptTokens: 84, CompletionTokens: 91, UseTime: 5, IsStream: 1,
		ChannelID: 7, TokenID: i64Ptr(3), Other: &other,
	})
	if err != nil {
		t.Fatalf("消费日志变换失败: %v", err)
	}
	if outcome != DetailKept || lg.Detail != other {
		t.Fatalf("合法 other 应保留为 detail: outcome=%d detail=%q", outcome, lg.Detail)
	}
	if lg.Protocol != "openai" || lg.ModelName != "deepseek-v4-flash" {
		t.Fatalf("protocol/model_name 错误: %+v", lg)
	}
	if lg.PromptTokens != 84 || lg.CompletionTokens != 91 || lg.Quota != 85 || lg.UseTime != 5 {
		t.Fatalf("计费列错误: %+v", lg)
	}
	if !lg.IsStream || lg.TokenID != 3 || lg.ChannelID != 7 || lg.UserID != 3 {
		t.Fatalf("关联列错误: %+v", lg)
	}
	if lg.CreatedTime != 1788343600 {
		t.Fatalf("created_at 应映射 created_time: %d", lg.CreatedTime)
	}

	// 超长 other → 置空。
	long := `{"k":"` + strings.Repeat("x", 3000) + `"}`
	lg2, outcome2, err := TransformConsumeLog(legacyLog{ID: 2, Type: 2, Other: &long})
	if err != nil {
		t.Fatalf("变换失败: %v", err)
	}
	if outcome2 != DetailDropped || lg2.Detail != "" {
		t.Fatalf("超长 other 应置空: outcome=%d detail_len=%d", outcome2, len(lg2.Detail))
	}
	// 非法 JSON → 置空。
	bad := `{not-json`
	lg3, outcome3, _ := TransformConsumeLog(legacyLog{ID: 3, Type: 2, Other: &bad})
	if outcome3 != DetailDropped || lg3.Detail != "" {
		t.Fatalf("非法 JSON other 应置空: outcome=%d", outcome3)
	}
	// 空 other → Absent。
	lg4, outcome4, _ := TransformConsumeLog(legacyLog{ID: 4, Type: 2})
	if outcome4 != DetailAbsent || lg4.Detail != "" {
		t.Fatalf("空 other 应为 Absent: outcome=%d", outcome4)
	}
	// token_id 缺省 → 0。
	if lg4.TokenID != 0 {
		t.Fatalf("token_id 缺省应为 0: %d", lg4.TokenID)
	}
}

func TestTransformTopupLog(t *testing.T) {
	quotaByID := map[int64]int64{1: 100000, 2: 100000, 3: 6849315068}
	content := "通过兑换码充值 $99999.999993 额度，兑换码ID 3"
	lg, linked, err := TransformTopupLog(legacyLog{
		ID: 913, UserID: 1, CreatedAt: 1788319073, Type: 1, Content: &content,
	}, quotaByID)
	if err != nil {
		t.Fatalf("topup 合成失败: %v", err)
	}
	if !linked {
		t.Fatal("兑换码ID 3 应成功关联")
	}
	if lg.Quota != 6849315068 {
		t.Fatalf("合成日志应带兑换码面值，实际 %d", lg.Quota)
	}
	if lg.Protocol != "topup" || lg.ModelName != "redemption" {
		t.Fatalf("topup 日志形状错误: %+v", lg)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(lg.Detail), &d); err != nil {
		t.Fatalf("detail 应为 JSON: %v", err)
	}
	if d["event"] != "topup" || d["ref_id"] != "legacy-redemption-3" || d["quota"] != float64(6849315068) {
		t.Fatalf("detail 内容错误: %v", d)
	}

	// 无法关联：quota=0，ref 回落日志 id。
	noRef := "无引用内容"
	lg2, linked2, _ := TransformTopupLog(legacyLog{ID: 910, Type: 1, Content: &noRef}, quotaByID)
	if linked2 || lg2.Quota != 0 {
		t.Fatalf("无法关联应 quota=0 linked=false: %+v linked=%v", lg2, linked2)
	}
	var d2 map[string]any
	_ = json.Unmarshal([]byte(lg2.Detail), &d2)
	if d2["ref_id"] != "legacy-log-910" {
		t.Fatalf("未关联 ref_id 应回落 legacy-log-<id>: %v", d2["ref_id"])
	}
	// content 引用不存在的兑换码 → 未关联。
	ghost := "兑换码ID 99"
	lg3, linked3, _ := TransformTopupLog(legacyLog{ID: 911, Type: 1, Content: &ghost}, quotaByID)
	if linked3 || lg3.Quota != 0 {
		t.Fatalf("引用不存在兑换码应未关联: %+v", lg3)
	}
}

// ---- ModelRatio ----

// TestTransformModelRatioConversion 验证 legacy×2 换算与最小集过滤。
func TestTransformModelRatioConversion(t *testing.T) {
	legacy := `{"deepseek-v4-flash": 0.32, "glm-5.3-flash": 0.09, "deepseek-v4-pro": 0.66, "unused-model": 1.0}`
	res, err := TransformModelRatio(legacy, []string{
		"deepseek-v4-pro", "glm-5.3-flash", "deepseek-v4-flash",
		"deepseek-v4-flash-vision-exp", "qwen3.8-flash", "glm-5.3-flash", // 重复项去重
	})
	if err != nil {
		t.Fatalf("换算失败: %v", err)
	}
	// ×2 换算且只含最小集模型（unused-model 被过滤）；键升序。
	want := `{"deepseek-v4-flash":0.64,"deepseek-v4-pro":1.32,"glm-5.3-flash":0.18}`
	if res.MigratedJSON != want {
		t.Fatalf("换算结果不符:\n got  %s\n want %s", res.MigratedJSON, want)
	}
	if len(res.Migrated) != 3 {
		t.Fatalf("Migrated 应 3 项: %v", res.Migrated)
	}
	wantMissing := []string{"deepseek-v4-flash-vision-exp", "qwen3.8-flash"}
	if strings.Join(res.Missing, ",") != strings.Join(wantMissing, ",") {
		t.Fatalf("缺口应 %v，实际 %v", wantMissing, res.Missing)
	}
	if _, err := TransformModelRatio(`{bad`, []string{"m"}); err == nil {
		t.Fatal("非法 JSON 应硬失败")
	}
	empty, err := TransformModelRatio("", []string{"m"})
	if err != nil {
		t.Fatalf("空输入不应失败: %v", err)
	}
	if empty.MigratedJSON != "{}" {
		t.Fatalf("空输入应得空集: %s", empty.MigratedJSON)
	}
	if len(empty.Missing) != 1 || empty.Missing[0] != "m" {
		t.Fatalf("空价单时最小集应全部进缺口: %v", empty.Missing)
	}
}

// mapSource 是内存配置源替身（满足 billing.Source）。
type mapSource map[string]string

func (m mapSource) Get(k string) (string, bool) { v, ok := m[k]; return v, ok }

// TestModelRatioGoldenAnchoring 是迁移正确性的黄金锚定：用换算后的 options
// 驱动真实 billing.Engine，对旧库真实账单的三个锚点（同 tokens）计费结果必须一致。
//
// 锚点（旧库 logs 实测，见 ADR-0008）：
//   - deepseek-v4-flash  (p=84,  c=91,  cr=0)     旧账 85  —— classic 模式锚定 ×2 换算方向；
//     若按 ÷2（mr=0.16）则得 21，锚定失败，防止方向性错账；
//   - glm-5.3-flash      (p=56761,c=750, cr=56384) 旧账 766 —— tiered_expr 逐字迁移锚定；
//   - qwen3.8-flash      (p=62,  c=36,  cr=0)     旧账 14  —— tiered_expr 锚定。
func TestModelRatioGoldenAnchoring(t *testing.T) {
	engine, err := billing.NewEngine(mapSource{
		billing.OptionKeyBillingMode:     `{"glm-5.3-flash":"tiered_expr","qwen3.8-flash":"tiered_expr"}`,
		billing.OptionKeyBillingExpr:     `{"glm-5.3-flash":"tier(\"base\", p * 0.18 + c * 0.6 + cr * 0.018)","qwen3.8-flash":"tier(\"base\", p * 0.15 + c * 0.5 + cr * 0.015)"}`,
		billing.OptionKeyGroupRatio:      `{"default":1,"vip":0.8,"trial":2}`,
		billing.OptionKeyCompletionRatio: `{"deepseek-v4-flash":2,"deepseek-v4-flash-vision-exp":2,"deepseek-v4-pro":3,"glm-5.3-flash":3.33}`,
		// 换算后 ModelRatio（legacy×2）。
		billing.OptionKeyModelRatio: `{"deepseek-v4-flash":0.64,"deepseek-v4-flash-vision-exp":0.64,"deepseek-v4-pro":1.32,"glm-5.3-flash":0.18}`,
	})
	if err != nil {
		t.Fatalf("构造计费引擎失败: %v", err)
	}

	cases := []struct {
		model string
		usage billing.Usage
		want  int64
	}{
		{"deepseek-v4-flash", billing.Usage{Input: 84, Completion: 91}, 85},
		{"glm-5.3-flash", billing.Usage{Input: 56761, Completion: 750, CacheRead: 56384}, 766},
		{"qwen3.8-flash", billing.Usage{Input: 62, Completion: 36}, 14},
	}
	for _, tc := range cases {
		price, err := engine.LookupPrice(tc.model)
		if err != nil {
			t.Fatalf("%s 应已配价: %v", tc.model, err)
		}
		got, err := engine.Charge(price, "default", tc.usage)
		if err != nil {
			t.Fatalf("%s 计费失败: %v", tc.model, err)
		}
		if got != tc.want {
			t.Fatalf("%s 黄金锚定失败：迁移后计费 %d ≠ 旧库账单 %d（用量 %+v）", tc.model, got, tc.want, tc.usage)
		}
	}

	// 方向性反证：若误用 ÷2（deepseek-v4-flash mr=0.16），同用量计费必然偏离旧账。
	wrongEngine, err := billing.NewEngine(mapSource{
		billing.OptionKeyModelRatio:      `{"deepseek-v4-flash":0.16}`,
		billing.OptionKeyCompletionRatio: `{"deepseek-v4-flash":2}`,
		billing.OptionKeyGroupRatio:      `{"default":1}`,
	})
	if err != nil {
		t.Fatalf("构造反证引擎失败: %v", err)
	}
	price, err := wrongEngine.LookupPrice("deepseek-v4-flash")
	if err != nil {
		t.Fatalf("反证引擎查价失败: %v", err)
	}
	got, err := wrongEngine.Charge(price, "default", billing.Usage{Input: 84, Completion: 91})
	if err != nil {
		t.Fatalf("反证计费失败: %v", err)
	}
	if got == 85 {
		t.Fatal("÷2 方向不可能得到旧账 85，反证失效")
	}
	t.Logf("方向反证：÷2 口径计费 %d ≠ 旧账 85（×2 口径 85 ✓）", got)
}

// ---- ActiveModels ----

func TestActiveModelsFromChannels(t *testing.T) {
	chs := []model.Channel{
		{Status: model.StatusEnabled, Models: "glm-5.3-flash,deepseek-v4-flash, qwen3.8-flash"},
		{Status: model.StatusDisabled, Models: "secret-model"},
		{Status: model.StatusEnabled, Models: "deepseek-v4-flash"},
	}
	got := ActiveModelsFromChannels(chs)
	want := []string{"deepseek-v4-flash", "glm-5.3-flash", "qwen3.8-flash"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("启用渠道最小集应 %v（去重升序、排除禁用），实际 %v", want, got)
	}
}
