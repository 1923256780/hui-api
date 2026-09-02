package override

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustApply(t *testing.T, body, overrideJSON string) map[string]any {
	t.Helper()
	out, err := Apply([]byte(body), overrideJSON)
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("改写结果不是合法 JSON: %v", err)
	}
	return m
}

// TestApplySetOverride set 新增、覆盖与嵌套路径自动创建。
func TestApplySetOverride(t *testing.T) {
	body := `{"model":"m1","temperature":1,"meta":{"team":"a"}}`
	got := mustApply(t, body, `{"set":{"temperature":0.2,"max_tokens":4096,"meta.owner":"op"}}`)

	if got["temperature"] != float64(0.2) {
		t.Fatalf("temperature 应被覆盖为 0.2，实际 %v", got["temperature"])
	}
	if got["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens 应新增为 4096，实际 %v", got["max_tokens"])
	}
	meta := got["meta"].(map[string]any)
	if meta["team"] != "a" || meta["owner"] != "op" {
		t.Fatalf("嵌套 set 结果不符: %v", meta)
	}
}

// TestApplyDeleteOverride delete 移除字段；不存在路径幂等跳过。
func TestApplyDeleteOverride(t *testing.T) {
	body := `{"model":"m1","top_p":0.9,"messages":[{"role":"user","name":"x","content":"hi"}]}`
	got := mustApply(t, body, `{"delete":["top_p","messages.0.name","not.exist.path"]}`)

	if _, ok := got["top_p"]; ok {
		t.Fatal("top_p 应被删除")
	}
	msg := got["messages"].([]any)[0].(map[string]any)
	if _, ok := msg["name"]; ok {
		t.Fatal("messages.0.name 应被删除")
	}
	if msg["content"] != "hi" {
		t.Fatalf("其余字段不应受影响: %v", msg)
	}
}

// TestApplyDeleteThenSet 固化语义：同一路径 delete 先于 set，最终取 set 值。
func TestApplyDeleteThenSet(t *testing.T) {
	body := `{"temperature":1,"reasoning":{"effort":"high"}}`
	got := mustApply(t, body, `{"delete":["temperature","reasoning.effort"],"set":{"temperature":0.5,"reasoning.effort":"low"}}`)

	if got["temperature"] != float64(0.5) {
		t.Fatalf("delete+set 同路径应取 set 值 0.5，实际 %v", got["temperature"])
	}
	if got["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("嵌套 delete+set 应取 set 值 low: %v", got["reasoning"])
	}

	// 反向用例：只有 delete 无 set 时字段确实被移除。
	got2 := mustApply(t, body, `{"delete":["temperature"]}`)
	if _, ok := got2["temperature"]; ok {
		t.Fatal("仅 delete 时字段应被移除")
	}
}

// TestApplyAppend 追加语义：字符串拼接、数组追加、不存在字段等价 set。
func TestApplyAppendOverride(t *testing.T) {
	body := `{"system":"base","stop":["a"],"missing":""}`
	got := mustApply(t, body, `{"append":{"system":"-tail","stop":"b","missing":"x"}}`)

	if got["system"] != "base-tail" {
		t.Fatalf("字符串 append 应拼接，实际 %v", got["system"])
	}
	stop := got["stop"].([]any)
	if len(stop) != 2 || stop[1] != "b" {
		t.Fatalf("数组 append 应追加到末尾，实际 %v", stop)
	}
	if got["missing"] != "x" {
		t.Fatalf("不存在字段 append 应等价 set，实际 %v", got["missing"])
	}
}

// TestApplyReplaceAndRegex 字符串替换与正则替换。
func TestApplyReplaceAndRegex(t *testing.T) {
	body := `{"model":"prefix-alpha-suffix","note":"aaa-123-aaa"}`
	got := mustApply(t, body, `{
		"replace": {"model": {"old": "alpha", "new": "beta"}},
		"regex_replace": {"note": {"pattern": "\\d+", "replacement": "N"}}
	}`)

	if got["model"] != "prefix-beta-suffix" {
		t.Fatalf("replace 子串替换不符: %v", got["model"])
	}
	if got["note"] != "aaa-N-aaa" {
		t.Fatalf("regex_replace 结果不符: %v", got["note"])
	}
}

// TestApplyTopLevelArrayPath 顶层数组下标寻址（messages.0.content）。
func TestApplyArrayIndexPath(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hello"}]}`
	got := mustApply(t, body, `{"set":{"messages.0.content":"hi"},"replace":{"messages.0.role":{"old":"user","new":"human"}}}`)

	msg := got["messages"].([]any)[0].(map[string]any)
	if msg["content"] != "hi" || msg["role"] != "human" {
		t.Fatalf("数组下标路径改写不符: %v", msg)
	}
}

// TestApplyNonObjectBodyPreserved 顶层非对象请求体原样返回。
func TestApplyNonObjectBodyPreserved(t *testing.T) {
	body := `[1,2,3]`
	out, err := Apply([]byte(body), `{"set":{"a":1}}`)
	if err != nil {
		t.Fatalf("Apply 不应报错: %v", err)
	}
	if string(out) != body {
		t.Fatalf("非对象请求体应原样返回，实际 %s", out)
	}
}

// TestApplyEmptyAndInvalidOverride 空配置原样返回；非法配置显式报错。
func TestApplyEmptyAndInvalidOverride(t *testing.T) {
	body := []byte(`{"model":"m1"}`)

	for _, cfg := range []string{"", "  ", "{}"} {
		out, err := Apply(body, cfg)
		if err != nil {
			t.Fatalf("空配置 %q 不应报错: %v", cfg, err)
		}
		if !reflect.DeepEqual(out, body) {
			t.Fatalf("空配置应原样返回，实际 %s", out)
		}
	}
	if _, err := Apply(body, `{"set":`); err == nil {
		t.Fatal("非法 override JSON 应报错")
	}
	if _, err := Apply(body, `{"regex_replace":{"a":{"pattern":"(","replacement":"x"}}}`); err == nil {
		t.Fatal("非法正则应报错")
	}
	if _, err := Apply([]byte(`not-json`), `{"set":{"a":1}}`); err == nil {
		t.Fatal("非法请求体 JSON 应报错")
	}
}

// TestApplySetArrayIndexOutOfBounds 数组下标越界显式报错。
func TestApplySetArrayIndexOutOfBounds(t *testing.T) {
	if _, err := Apply([]byte(`{"a":[1]}`), `{"set":{"a.5.b":1}}`); err == nil {
		t.Fatal("数组下标越界应报错")
	}
}

// TestApplyNumberPrecision set 的数字字面量保留原样（json.Number 透传）。
func TestApplyNumberPrecision(t *testing.T) {
	out, err := Apply([]byte(`{}`), `{"set":{"n":12345678901234567890,"f":0.1}}`)
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if string(out) != `{"f":0.1,"n":12345678901234567890}` {
		t.Fatalf("数字精度应保留，实际 %s", out)
	}
}

// TestApplyCombinedOps 多操作组合顺序：delete → set → append → replace → regex_replace。
func TestApplyCombinedOps(t *testing.T) {
	body := `{"model":"m1","tag":"A"}`
	got := mustApply(t, body, `{
		"delete": ["model"],
		"set": {"model": "m2", "tag": "B"},
		"append": {"tag": "-C"},
		"replace": {"tag": {"old": "B-C", "new": "X"}}
	}`)
	if got["model"] != "m2" {
		t.Fatalf("组合后 model 应为 m2，实际 %v", got["model"])
	}
	if got["tag"] != "X" {
		t.Fatalf("组合后 tag 应为 X（set→append→replace 链），实际 %v", got["tag"])
	}
}

// TestParseOps 结构解析回读。
func TestParseOps(t *testing.T) {
	ops, err := Parse(`{"delete":["a"],"set":{"b":1},"append":{"c":"x"},
		"replace":{"d":{"old":"o","new":"n"}},
		"regex_replace":{"e":{"pattern":"p","replacement":"r"}}}`)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if !reflect.DeepEqual(ops.Delete, []string{"a"}) {
		t.Fatalf("delete 解析不符: %v", ops.Delete)
	}
	if ops.Replace["d"] != (ReplaceOp{Old: "o", New: "n"}) {
		t.Fatalf("replace 解析不符: %v", ops.Replace["d"])
	}
	if ops.RegexReplace["e"] != (RegexOp{Pattern: "p", Replacement: "r"}) {
		t.Fatalf("regex_replace 解析不符: %v", ops.RegexReplace["e"])
	}
	empty, err := Parse("")
	if err != nil {
		t.Fatalf("空配置 Parse 失败: %v", err)
	}
	if !empty.Empty() {
		t.Fatal("空配置应为空操作集")
	}
}
