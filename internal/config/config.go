// Package config 实现 hui-api 的配置双轨（docs/01 设计点 7）：
//
//   - 启动轨（Bootstrap）：环境变量 + YAML bootstrap 文件，仅进程启动时读取，
//     覆盖端口、数据库路径、会话密钥等部署面；优先级为 环境变量 > YAML > 代码默认值；
//   - 运行轨（Runtime）：options 表覆盖层，管理面修改后热加载生效，无需重启。
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// 代码内默认值（YAML 与环境变量都未提供时生效）。
const (
	DefaultAddr       = ":3100"
	DefaultSQLitePath = "hui-api.db"
)

// 环境变量名。
const (
	EnvPort          = "PORT"
	EnvSQLitePath    = "SQLITE_PATH"
	EnvSessionSecret = "SESSION_SECRET"
)

// Bootstrap 是启动轨配置快照。
type Bootstrap struct {
	Addr          string `yaml:"addr"`           // HTTP 监听地址（如 :3100）
	SQLitePath    string `yaml:"sqlite_path"`    // SQLite 数据库文件路径
	SessionSecret string `yaml:"session_secret"` // 管理台会话密钥；为空表示启动时随机生成
	Source        string `yaml:"-"`              // 配置来源描述（日志排查用）
}

// LoadBootstrap 加载启动轨配置。yamlPath 为空表示不读取文件，仅用默认值+环境变量。
// 优先级：环境变量 > YAML 文件 > 代码默认值。
func LoadBootstrap(yamlPath string) (*Bootstrap, error) {
	b := &Bootstrap{
		Addr:       DefaultAddr,
		SQLitePath: DefaultSQLitePath,
	}
	if yamlPath != "" {
		data, err := os.ReadFile(yamlPath)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件 %s: %w", yamlPath, err)
		}
		if err := yaml.Unmarshal(data, b); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s: %w", yamlPath, err)
		}
		b.Source = "yaml:" + yamlPath
	} else {
		b.Source = "defaults"
	}

	// 环境变量覆盖（12-factor 部署面）。
	if v := strings.TrimSpace(os.Getenv(EnvPort)); v != "" {
		b.Addr = NormalizeAddr(v)
	}
	if v := strings.TrimSpace(os.Getenv(EnvSQLitePath)); v != "" {
		b.SQLitePath = v
	}
	if v := os.Getenv(EnvSessionSecret); v != "" {
		b.SessionSecret = v
	}
	if b.Addr == "" {
		b.Addr = DefaultAddr
	}
	if b.SQLitePath == "" {
		b.SQLitePath = DefaultSQLitePath
	}
	return b, nil
}

// NormalizeAddr 归一化监听地址：纯数字端口补冒号前缀，含冒号的原样返回。
func NormalizeAddr(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultAddr
	}
	if strings.Contains(v, ":") {
		return v
	}
	return ":" + v
}
