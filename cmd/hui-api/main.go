// hui-api 程序入口。M0 阶段提供版本信息输出与 /health 健康检查占位实现；
// M1 起逐步装配配置双轨、存储层与 relay 路由。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
)

// 构建信息，可经 -ldflags "-X main.version=..." 注入。
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本信息后退出")
	addr := flag.String("addr", ":3100", "HTTP 监听地址")
	flag.Parse()

	if *showVersion {
		fmt.Printf("hui-api %s (commit=%s built=%s)\n", version, commit, date)
		return
	}

	log.Printf("hui-api %s 启动，监听 %s", version, *addr)
	if err := http.ListenAndServe(*addr, newMux()); err != nil {
		log.Fatalf("HTTP 服务异常退出: %v", err)
	}
}

// newMux 组装路由表。M1 起将替换为 gin 路由与中间件链。
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	return mux
}

// handleHealth 返回服务健康状态，供负载均衡、systemd 探活与 M4 切换预案使用。
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version,
	})
}
