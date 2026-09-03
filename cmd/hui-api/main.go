// hui-api 程序入口：装配配置双轨、存储层、hook 旁路与路由，并实现优雅停机。
//
// 启动顺序（docs/01）：加载启动轨配置 → 打开存储层并迁移 → 加载运行轨配置 →
// 启动 hook 旁路 → 挂路由 → 监听信号。停机顺序：HTTP 优雅关闭（带超时）→
// hook 队列排空 → 关闭连接池。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/1923256780/hui-api/internal/api"
	"github.com/1923256780/hui-api/internal/billing"
	"github.com/1923256780/hui-api/internal/config"
	"github.com/1923256780/hui-api/internal/gateway"
	"github.com/1923256780/hui-api/internal/hook"
	"github.com/1923256780/hui-api/internal/relay/anthropic"
	"github.com/1923256780/hui-api/internal/relay/openai"
	"github.com/1923256780/hui-api/internal/store"
	webui "github.com/1923256780/hui-api/web"
)

// 构建信息，可经 -ldflags "-X main.version=..." 注入。
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// httpShutdownTimeout 优雅停机窗口：等待在途请求完成的上限。
const httpShutdownTimeout = 10 * time.Second

func main() {
	showVersion := flag.Bool("version", false, "打印版本信息后退出")
	addr := flag.String("addr", "", "HTTP 监听地址（覆盖配置文件与环境变量，如 :3100）")
	configPath := flag.String("config", "", "YAML bootstrap 配置文件路径")
	flag.Parse()

	if *showVersion {
		fmt.Printf("hui-api %s (commit=%s built=%s)\n", version, commit, date)
		return
	}
	if err := run(*addr, *configPath); err != nil {
		log.Fatalf("hui-api 异常退出: %v", err)
	}
}

// run 完成完整启动装配，阻塞直至收到停机信号并完成优雅关闭。
func run(addrOverride, configPath string) error {
	// 1. 启动轨配置：环境变量 + YAML > 代码默认值；-addr flag 最高优先。
	cfg, err := config.LoadBootstrap(configPath)
	if err != nil {
		return fmt.Errorf("加载启动配置: %w", err)
	}
	addr := cfg.Addr
	if addrOverride != "" {
		addr = config.NormalizeAddr(addrOverride)
	}
	if cfg.SessionSecret == "" {
		secret, err := randomSecret()
		if err != nil {
			return fmt.Errorf("生成会话密钥: %w", err)
		}
		cfg.SessionSecret = secret
		log.Printf("SESSION_SECRET 未设置，已随机生成（重启后已有会话失效）；生产部署建议在配置文件中固定")
	}
	log.Printf("配置来源: %s；监听 %s；SQLite: %s", cfg.Source, addr, cfg.SQLitePath)

	// 2. 存储层：打开（读池/写池分离）并执行迁移。
	st, err := store.Open(cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("打开存储层: %w", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("关闭存储层: %v", err)
		}
	}()
	schemaVersion, err := st.Migrate()
	if err != nil {
		return fmt.Errorf("执行迁移: %w", err)
	}
	log.Printf("存储层就绪，schema 版本 %d", schemaVersion)

	// 3. 运行轨配置：options 覆盖层；M2 起管理面写入后 Reload 即热生效。
	rt, err := config.NewRuntime(st)
	if err != nil {
		return fmt.Errorf("加载运行轨配置: %w", err)
	}

	// 4. hook 旁路：注册内置实现与 OTLP/webhook（M2-wave3），启动有界队列分发
	//（队列满丢弃并计数；endpoint/URL 未配置时 hook 自身静默跳过，热更即时生效）。
	registry := hook.NewRegistry()
	if err := registry.Register(hook.NewNoop()); err != nil {
		return fmt.Errorf("注册 noop hook: %w", err)
	}
	if err := registry.Register(hook.NewConsole()); err != nil {
		return fmt.Errorf("注册 console hook: %w", err)
	}
	if err := registry.Register(hook.NewOTLP(func() string {
		v, _ := rt.Get(hook.OptionKeyOTLPEndpoint)
		return v
	})); err != nil {
		return fmt.Errorf("注册 otlp hook: %w", err)
	}
	if err := registry.Register(hook.NewWebhook(func() string {
		v, _ := rt.Get(hook.OptionKeyWebhookURL)
		return v
	})); err != nil {
		return fmt.Errorf("注册 webhook hook: %w", err)
	}
	dispatcher := hook.NewDispatcher(registry, hook.DefaultQueueSize)
	dispatcher.Start(2)
	defer dispatcher.Stop(3 * time.Second)

	// 5. 路由 + 前端 SPA。
	engine, gw, err := newRouter(st, rt, schemaVersion, cfg.SessionSecret)
	if err != nil {
		return fmt.Errorf("组装路由: %w", err)
	}
	defer gw.Close() // 优雅停机排空异步请求日志（先行于存储层关闭）
	gw.SetHooks(dispatcher) // 观测旁路挂接（M2-wave3）
	engine.NoRoute(gin.WrapH(webui.Handler()))

	srv := &http.Server{Addr: addr, Handler: engine}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	log.Printf("hui-api %s（commit=%s）就绪", version, commit)

	select {
	case <-ctx.Done():
		log.Printf("收到停机信号，开始优雅停机…")
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP 优雅停机未在 %s 内完成: %v", httpShutdownTimeout, err)
	}
	log.Printf("hui-api 已停止")
	return nil
}

// newRouter 组装路由：/health 健康检查、/api/status 状态端点、转发面 /v1/* 与
// 管理面 /api（M2-wave1：root 引导 + 会话 + CRUD）。计费引擎在此构造（内置价单
// 启动校验，schema 非法时 fail-fast 拒绝启动），同时返回 Gateway 供调用方停机时
// 排空异步日志。sessionSecret 为会话 cookie 签名密钥（run() 已保证非空）。
func newRouter(st *store.Store, rt *config.Runtime, schemaVersion int64, sessionSecret string) (*gin.Engine, *gateway.Gateway, error) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// /health 契约（M0 固化）：200 + JSON，status=ok。
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "version": version})
	})
	// /api/status 契约（docs/05 管理面统一包裹结构）：服务状态、版本、schema 版本。
	r.GET("/api/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data": gin.H{
				"version":        version,
				"commit":         commit,
				"schema_version": schemaVersion,
				"config_version": rt.Version(),
			},
		})
	})

	// 计费引擎（docs/04）：价格来源 options 运行轨优先、内置 prices.json 兜底。
	pricer, err := billing.NewEngine(rt)
	if err != nil {
		return nil, nil, fmt.Errorf("构造计费引擎: %w", err)
	}

	// 转发面（docs/05 端点清单）：编排在 gateway，协议适配在 relay/<protocol>。
	gw := gateway.New(st, rt, pricer)
	v1 := r.Group("/v1")
	v1.POST("/chat/completions", func(c *gin.Context) { gw.Serve(c, openai.New()) })
	v1.POST("/messages", func(c *gin.Context) { gw.Serve(c, anthropic.New()) })
	v1.POST("/messages/count_tokens", func(c *gin.Context) { gw.Serve(c, anthropic.New()) })
	v1.GET("/models", gw.HandleModels)

	// 管理面（M2-wave1，docs/05）：先保证 root 存在（幂等），再挂会话与 /api CRUD。
	if _, err := api.EnsureRootUser(st); err != nil {
		return nil, nil, fmt.Errorf("引导 root 用户: %w", err)
	}
	sess := api.NewSessionManager([]byte(sessionSecret))
	api.New(st, rt, gw, sess).Register(r)
	return r, gw, nil
}

// randomSecret 生成 32 字节随机密钥的 hex 编码（SESSION_SECRET 缺省兜底）。
func randomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("读取随机熵: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
