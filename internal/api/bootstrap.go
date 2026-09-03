// bootstrap.go root 用户引导（M2-wave1）：首次启动保证存在管理员账号，
// 口令经 HUI_API_ROOT_PASSWORD 注入（缺省 123456，docs/05 部署注记）。
package api

import (
	"log"
	"os"
	"time"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// DefaultRootPassword 是未设置 HUI_API_ROOT_PASSWORD 时的缺省口令
// （本地自托管场景；生产部署应显式注入强口令）。
const DefaultRootPassword = "123456"

// RootUsername 是引导创建的管理员用户名。
const RootUsername = "root"

// EnsureRootUser 保证存在 root（role=100）用户：不存在时以 username=root 创建。
// 幂等：已存在任何 root 用户时不做任何变更。返回是否新建。
func EnsureRootUser(st *store.Store) (bool, error) {
	var n int64
	if err := st.Read.Model(&model.User{}).Where("role = ?", model.RoleRoot).Count(&n).Error; err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	plain := os.Getenv("HUI_API_ROOT_PASSWORD")
	if plain == "" {
		plain = DefaultRootPassword
	}
	hash, err := HashPassword(plain)
	if err != nil {
		return false, err
	}
	u := model.User{
		Username:     RootUsername,
		PasswordHash: hash,
		Role:         model.RoleRoot,
		Status:       model.StatusEnabled,
		DisplayName:  "Administrator",
		AuthVersion:  1,
		CreatedTime:  time.Now().Unix(),
	}
	if err := st.Write.Create(&u).Error; err != nil {
		return false, err
	}
	log.Printf("[api] 已创建初始 root 用户（username=%s，口令来自 HUI_API_ROOT_PASSWORD 或缺省值）", RootUsername)
	return true, nil
}
