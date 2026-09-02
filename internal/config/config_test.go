package config

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/1923256780/hui-api/internal/store"
)

// TestNormalizeAddr 归一化边界。
func TestNormalizeAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"3100", ":3100"},
		{":3100", ":3100"},
		{"0.0.0.0:3100", "0.0.0.0:3100"},
		{" 3200 ", ":3200"},
		{"", DefaultAddr},
	}
	for _, c := range cases {
		if got := NormalizeAddr(c.in); got != c.want {
			t.Fatalf("NormalizeAddr(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestLoadBootstrapDefaults 无文件无环境变量时取代码默认值。
func TestLoadBootstrapDefaults(t *testing.T) {
	t.Setenv(EnvPort, "")
	t.Setenv(EnvSQLitePath, "")
	t.Setenv(EnvSessionSecret, "")
	b, err := LoadBootstrap("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if b.Addr != DefaultAddr || b.SQLitePath != DefaultSQLitePath {
		t.Fatalf("默认值不一致: %+v", b)
	}
}

// TestLoadBootstrapYamlThenEnvWins 验证 YAML 提供基线、环境变量覆盖。
func TestLoadBootstrapYamlThenEnvWins(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	content := "addr: \":3200\"\nsqlite_path: \"data/custom.db\"\nsession_secret: \"unit-secret\"\n"
	if err := os.WriteFile(yamlPath, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试 yaml 失败: %v", err)
	}

	b, err := LoadBootstrap(yamlPath)
	if err != nil {
		t.Fatalf("加载 yaml 失败: %v", err)
	}
	if b.Addr != ":3200" || b.SQLitePath != "data/custom.db" {
		t.Fatalf("yaml 值未生效: %+v", b)
	}
	if b.SessionSecret != "unit-secret" {
		t.Fatalf("session_secret 未生效: %+v", b)
	}

	// 环境变量覆盖 YAML。
	t.Setenv(EnvPort, "3300")
	t.Setenv(EnvSQLitePath, "env.db")
	t.Setenv(EnvSessionSecret, "env-secret")
	b, err = LoadBootstrap(yamlPath)
	if err != nil {
		t.Fatalf("加载 yaml+env 失败: %v", err)
	}
	if b.Addr != ":3300" {
		t.Fatalf("PORT 环境变量应覆盖为 :3300，实际 %q", b.Addr)
	}
	if b.SQLitePath != "env.db" || b.SessionSecret != "env-secret" {
		t.Fatalf("环境变量覆盖未生效: %+v", b)
	}

	// PORT 纯数字自动补冒号；显式含冒号原样保留。
	t.Setenv(EnvPort, "0.0.0.0:3400")
	b, _ = LoadBootstrap("")
	if b.Addr != "0.0.0.0:3400" {
		t.Fatalf("含冒号地址应原样保留，实际 %q", b.Addr)
	}
}

// TestLoadBootstrapMissingFile 显式指定的文件不存在必须报错（防静默丢配置）。
func TestLoadBootstrapMissingFile(t *testing.T) {
	if _, err := LoadBootstrap(filepath.Join(t.TempDir(), "no-such.yaml")); err == nil {
		t.Fatal("缺失配置文件应返回错误")
	}
}

// openTestStore 复用 store 包建临时库（与 config.Runtime 集成）。
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config-test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return st
}

// TestRuntimeHotReload 验证运行轨：写 options 后 Reload 热生效、版本号原子递增。
func TestRuntimeHotReload(t *testing.T) {
	st := openTestStore(t)

	rt, err := NewRuntime(st)
	if err != nil {
		t.Fatalf("构造 Runtime 失败: %v", err)
	}
	if rt.Version() != 1 {
		t.Fatalf("首次加载版本应为 1，实际 %d", rt.Version())
	}
	if _, ok := rt.Get("relay.timeout_seconds"); ok {
		t.Fatal("未写入的键不应存在")
	}
	if v := rt.GetInt64("relay.timeout_seconds", 30); v != 30 {
		t.Fatalf("未设键应返回默认值 30，实际 %d", v)
	}

	if err := st.SetOption("relay.timeout_seconds", "45"); err != nil {
		t.Fatalf("SetOption 失败: %v", err)
	}
	if v := rt.GetInt64("relay.timeout_seconds", 30); v != 30 {
		t.Fatal("Reload 前旧快照必须生效（热更前的读不应看到新值）")
	}
	if err := rt.Reload(); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if v := rt.GetInt64("relay.timeout_seconds", 30); v != 45 {
		t.Fatalf("Reload 后应读到 45，实际 %d", v)
	}
	if rt.Version() != 2 {
		t.Fatalf("第二次加载版本应为 2，实际 %d", rt.Version())
	}

	// 覆盖写 + 类型异常兜底。
	if err := st.SetOption("relay.timeout_seconds", "60"); err != nil {
		t.Fatalf("覆盖写失败: %v", err)
	}
	if err := st.SetOption("feature.beta", "true"); err != nil {
		t.Fatalf("SetOption 失败: %v", err)
	}
	if err := rt.Reload(); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if v := rt.GetInt64("relay.timeout_seconds", 30); v != 60 {
		t.Fatalf("覆盖后应读到 60，实际 %d", v)
	}
	if !rt.GetBool("feature.beta", false) {
		t.Fatal("GetBool 应读到 true")
	}
	if err := st.SetOption("feature.beta", "not-a-bool"); err != nil {
		t.Fatalf("写入非法布尔失败: %v", err)
	}
	if err := rt.Reload(); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if rt.GetBool("feature.beta", true) != true {
		t.Fatal("非法布尔值应回退默认值 true")
	}
}

// TestRuntimeConcurrentAccess 并发读 + 并发热更不撕裂（配合 -race 检查）。
func TestRuntimeConcurrentAccess(t *testing.T) {
	st := openTestStore(t)
	rt, err := NewRuntime(st)
	if err != nil {
		t.Fatalf("构造 Runtime 失败: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = rt.GetInt64("hot.key", 1)
					_ = rt.Version()
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if err := st.SetOption("hot.key", strconv.Itoa(i)); err != nil {
			t.Fatalf("SetOption 失败: %v", err)
		}
		if err := rt.Reload(); err != nil {
			t.Fatalf("Reload 失败: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	if v := rt.GetInt64("hot.key", 0); v != 19 {
		t.Fatalf("最终值应为 19，实际 %d", v)
	}
}
