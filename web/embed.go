// Package webui 把前端构建产物（web/dist）以 go:embed 嵌入单二进制，
// 并提供带 SPA 回退的静态资源 Handler。构建前必须保证 dist 存在：
// 本地由 scripts/build.ps1 兜底，CI 的 go job 会在 vet/test 前生成占位页。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler 返回前端静态资源 Handler：命中文件直接服务，未命中回退 index.html
// （React Router 之类的 SPA 前端路由依赖此回退）。
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// embed 根目录固定为 dist，正常不可达；防御性返回 404。
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "前端资源不可用", http.StatusNotFound)
		})
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(sub, p); err != nil {
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}
