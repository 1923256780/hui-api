// Package override 实现 M1-wave2 的 param_override 管道：
// 对请求 JSON 按固定顺序应用渠道级参数改写操作（channels.param_override 列）。
//
// 配置为 JSON 对象，支持操作集（固定执行顺序：delete → set → append → replace → regex_replace）：
//
//	{
//	  "delete":        ["top_p", "messages.0.name"],              // 移除字段（不存在则忽略，幂等）
//	  "set":           {"temperature": 0.2, "meta.team": "a"},     // 写入/覆盖字段（中间层自动创建）
//	  "append":        {"system": "suffix", "stop": "END"},        // 追加：字符串拼接 / 数组追加；字段不存在时等价 set
//	  "replace":       {"model": {"old": "x", "new": "y"}},        // 字符串子串替换
//	  "regex_replace": {"prompt": {"pattern": "a+", "replacement": "b"}} // 正则替换
//	}
//
// 路径为点分式，数组用数字下标（如 messages.0.content）。delete 先于 set 执行，
// 同一路径同时出现在 delete 与 set 时最终取 set 值——这是固化语义，有测试保障。
package override

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ReplaceOp 是 replace 操作的参数：把路径处字符串中的 Old 子串全部替换为 New。
type ReplaceOp struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// RegexOp 是 regex_replace 操作的参数：对路径处字符串按 Go regexp 语义替换。
type RegexOp struct {
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
}

// Ops 是一份解析后的改写操作集。
type Ops struct {
	Delete       []string             `json:"delete"`
	Set          map[string]any       `json:"set"`
	Append       map[string]any       `json:"append"`
	Replace      map[string]ReplaceOp `json:"replace"`
	RegexReplace map[string]RegexOp   `json:"regex_replace"`
}

// Parse 解析 override 配置 JSON；空串视为空操作集（无任何改写）。
func Parse(overrideJSON string) (*Ops, error) {
	ops := &Ops{}
	if strings.TrimSpace(overrideJSON) == "" {
		return ops, nil
	}
	dec := json.NewDecoder(strings.NewReader(overrideJSON))
	dec.UseNumber()
	if err := dec.Decode(ops); err != nil {
		return nil, fmt.Errorf("解析 param_override 配置: %w", err)
	}
	return ops, nil
}

// Apply 对请求体 JSON 应用渠道级改写操作，返回改写后的请求体。
// body 必须是 JSON 对象（顶层数组等非对象请求体原样返回，不做改写）。
// overrideJSON 非法时报错（渠道配置错误应当显式失败而非静默忽略）。
func Apply(body []byte, overrideJSON string) ([]byte, error) {
	ops, err := Parse(overrideJSON)
	if err != nil {
		return nil, err
	}
	if ops.Empty() {
		return body, nil
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("解析请求体: %w", err)
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return body, nil
	}

	changed := false
	regexes, err := compileRegexes(ops) // 正则提前编译：配置错误在应用前显式失败
	if err != nil {
		return nil, err
	}
	for _, path := range ops.Delete { // 1. delete
		if deletePath(obj, splitPath(path)) {
			changed = true
		}
	}
	for _, path := range sortedKeys(ops.Set) { // 2. set
		if err := setPath(obj, splitPath(path), ops.Set[path]); err != nil {
			return nil, fmt.Errorf("set %s: %w", path, err)
		}
		changed = true
	}
	for _, path := range sortedKeys(ops.Append) { // 3. append
		if err := appendPath(obj, splitPath(path), ops.Append[path]); err != nil {
			return nil, fmt.Errorf("append %s: %w", path, err)
		}
		changed = true
	}
	for _, path := range sortedReplaceKeys(ops.Replace) { // 4. replace
		if err := replacePath(obj, splitPath(path), ops.Replace[path]); err != nil {
			return nil, fmt.Errorf("replace %s: %w", path, err)
		}
		changed = true
	}
	for _, path := range sortedRegexKeys(ops.RegexReplace) { // 5. regex_replace
		if err := regexReplacePath(obj, splitPath(path), ops.RegexReplace[path], regexes[path]); err != nil {
			return nil, fmt.Errorf("regex_replace %s: %w", path, err)
		}
		changed = true
	}

	if !changed {
		return body, nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("序列化改写后请求体: %w", err)
	}
	return out, nil
}

// Empty 判断操作集是否为空（空集时 Apply 原样返回请求体）。
func (o *Ops) Empty() bool {
	return len(o.Delete) == 0 && len(o.Set) == 0 && len(o.Append) == 0 &&
		len(o.Replace) == 0 && len(o.RegexReplace) == 0
}

// splitPath 拆分点分路径；空白路径视为非法。
func splitPath(path string) []string {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// child 取父节点下指定层级的子节点；不存在时返回 (nil, false)。
// node 为对象时 key 是字段名；node 为数组时 key 必须是数字下标。
func child(node any, key string) (any, bool) {
	switch cur := node.(type) {
	case map[string]any:
		v, ok := cur[key]
		return v, ok
	case []any:
		idx, ok := arrayIndex(key, len(cur))
		if !ok {
			return nil, false
		}
		return cur[idx], true
	default:
		return nil, false
	}
}

// isIndex 判断字符串是否为纯数字下标。
func isIndex(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// arrayIndex 解析数字下标；越界返回 false。
func arrayIndex(key string, length int) (int, bool) {
	idx := 0
	for _, r := range key {
		if r < '0' || r > '9' {
			return 0, false
		}
		idx = idx*10 + int(r-'0')
	}
	if idx >= length {
		return 0, false
	}
	return idx, true
}

// deletePath 按路径删除节点；路径不存在时忽略并返回 false（幂等）。
// parts 相对 node（对象）寻址；遇到数组时，下一段必须是数字下标。
func deletePath(node map[string]any, parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	if len(parts) == 1 {
		if _, ok := node[parts[0]]; !ok {
			return false
		}
		delete(node, parts[0])
		return true
	}
	head, rest := parts[0], parts[1:]
	next, ok := node[head]
	if !ok {
		return false
	}
	switch cur := next.(type) {
	case map[string]any:
		return deletePath(cur, rest)
	case []any:
		idx, ok := arrayIndex(rest[0], len(cur))
		if !ok {
			return false
		}
		sub, ok := cur[idx].(map[string]any)
		if !ok {
			return false
		}
		return deletePath(sub, rest[1:])
	default:
		return false
	}
}

// setPath 按路径写入值；中间层为对象且不存在时自动创建，
// 数组下标越界/数组不存在时报错（不支持自动创建数组）。
func setPath(node map[string]any, parts []string, value any) error {
	if len(parts) == 0 {
		return fmt.Errorf("路径为空")
	}
	if len(parts) == 1 {
		node[parts[0]] = value
		return nil
	}
	head, rest := parts[0], parts[1:]
	next, ok := node[head]
	if !ok {
		if isIndex(rest[0]) {
			return fmt.Errorf("数组 %q 不存在，无法按下标寻址", head)
		}
		// 中间层自动创建为对象。
		next = map[string]any{}
		node[head] = next
	}
	switch cur := next.(type) {
	case map[string]any:
		return setPath(cur, rest, value)
	case []any:
		idx, ok := arrayIndex(rest[0], len(cur))
		if !ok {
			return fmt.Errorf("数组 %q 下标 %q 不存在或越界", head, rest[0])
		}
		sub, ok := cur[idx].(map[string]any)
		if !ok {
			return fmt.Errorf("下标 %s 处不是对象，无法继续寻址", rest[0])
		}
		return setPath(sub, rest[1:], value)
	default:
		return fmt.Errorf("路径 %q 处是标量，无法寻址", head)
	}
}

// appendPath 追加语义：字符串拼接、数组追加；字段不存在时等价 set。
func appendPath(node map[string]any, parts []string, value any) error {
	existing, ok := lookup(node, parts)
	if !ok || existing == nil {
		return setPath(node, parts, value)
	}
	switch cur := existing.(type) {
	case string:
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("仅字符串可追加到字符串")
		}
		return setPath(node, parts, cur+s)
	case []any:
		return setPath(node, parts, append(cur, value))
	default:
		return fmt.Errorf("类型 %T 不支持 append（仅字符串与数组）", existing)
	}
}

// lookup 只读寻址。
func lookup(node map[string]any, parts []string) (any, bool) {
	if len(parts) == 0 {
		return nil, false
	}
	if len(parts) == 1 {
		v, ok := node[parts[0]]
		return v, ok
	}
	head, rest := parts[0], parts[1:]
	next, ok := node[head]
	if !ok {
		return nil, false
	}
	switch cur := next.(type) {
	case map[string]any:
		return lookup(cur, rest)
	case []any:
		idx, ok := arrayIndex(rest[0], len(cur))
		if !ok {
			return nil, false
		}
		sub, ok := cur[idx].(map[string]any)
		if !ok {
			return nil, false
		}
		return lookup(sub, rest[1:])
	default:
		return nil, false
	}
}

// replacePath 对路径处字符串做子串替换；非字符串报错，路径不存在忽略。
func replacePath(node map[string]any, parts []string, op ReplaceOp) error {
	existing, ok := lookup(node, parts)
	if !ok || existing == nil {
		return nil
	}
	s, isStr := existing.(string)
	if !isStr {
		return fmt.Errorf("replace 仅支持字符串值，实际 %T", existing)
	}
	return setPath(node, parts, strings.ReplaceAll(s, op.Old, op.New))
}

// regexReplacePath 对路径处字符串做正则替换；路径不存在忽略。
// 正则由 Apply 预编译（compileRegexes），此处只做应用。
func regexReplacePath(node map[string]any, parts []string, op RegexOp, re *regexp.Regexp) error {
	existing, ok := lookup(node, parts)
	if !ok || existing == nil {
		return nil
	}
	s, isStr := existing.(string)
	if !isStr {
		return fmt.Errorf("regex_replace 仅支持字符串值，实际 %T", existing)
	}
	if re == nil {
		var err error
		if re, err = regexp.Compile(op.Pattern); err != nil {
			return fmt.Errorf("正则 %q 编译失败: %w", op.Pattern, err)
		}
	}
	return setPath(node, parts, re.ReplaceAllString(s, op.Replacement))
}

// compileRegexes 预编译全部 regex_replace 模式，返回路径 → 编译结果。
// 任一模式非法即报错：配置错误必须在应用前显式失败，不能静默跳过。
func compileRegexes(ops *Ops) (map[string]*regexp.Regexp, error) {
	if len(ops.RegexReplace) == 0 {
		return nil, nil
	}
	out := make(map[string]*regexp.Regexp, len(ops.RegexReplace))
	for path, op := range ops.RegexReplace {
		re, err := regexp.Compile(op.Pattern)
		if err != nil {
			return nil, fmt.Errorf("regex_replace %s 正则 %q 编译失败: %w", path, op.Pattern, err)
		}
		out[path] = re
	}
	return out, nil
}

// sortedKeys 返回 map 键的稳定排序（保证改写顺序确定、测试可复现）。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

func sortedReplaceKeys(m map[string]ReplaceOp) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

func sortedRegexKeys(m map[string]RegexOp) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
