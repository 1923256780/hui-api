// 旧库源行结构与只读读取（本文件所有查询只允许跑在 store.OpenReadOnly 返回的池上）。
package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

// legacyUser 是旧库 users 表的源行（仅声明迁移用到的列）。
type legacyUser struct {
	ID          int64 // 旧库主键（迁移后沿用，保证令牌 user_id 关联一致）
	Username    string
	Password    string // 期望 $2a$ 前缀 bcrypt
	DisplayName *string
	Role        int
	Status      int
	Email       *string
	Quota       int64
	UsedQuota   int64
	Group       *string `gorm:"column:group"`
	CreatedAt   *int64  `gorm:"column:created_at"`
	LastLoginAt *int64  `gorm:"column:last_login_at"`
}

// legacyToken 是旧库 tokens 表的源行。
type legacyToken struct {
	ID                 int64
	UserID             int64
	Key                string // 48 字符无前缀明文
	Status             int
	Name               *string
	CreatedTime        *int64
	AccessedTime       *int64
	ExpiredTime        *int64
	RemainQuota        *int64
	UnlimitedQuota     int64 // numeric 0/1
	ModelLimitsEnabled *int64
	ModelLimits        *string
	AllowIPs           *string `gorm:"column:allow_ips"`
	Group              *string `gorm:"column:group"`
}

// legacyChannel 是旧库 channels 表的源行。
type legacyChannel struct {
	ID            int64
	Type          int
	Key           string
	Status        int
	Name          *string
	Weight        int64
	CreatedTime   *int64
	BaseURL       *string
	Models        *string
	Group         *string `gorm:"column:group"`
	ModelMapping  *string
	Priority      int64
	ParamOverride *string
}

// legacyOption 是旧库 options 表的源行。
type legacyOption struct {
	Key   string
	Value string
}

// legacyRedemption 是旧库 redemptions 表的源行。
type legacyRedemption struct {
	ID           int64
	UserID       *int64
	Key          string
	Status       int
	Name         *string
	Quota        int64
	CreatedTime  *int64
	RedeemedTime *int64
	UsedUserID   *int64 `gorm:"column:used_user_id"`
	ExpiredTime  *int64
}

// legacyLog 是旧库 logs 表的源行。
type legacyLog struct {
	ID               int64
	UserID           int64
	CreatedAt        int64 `gorm:"column:created_at"`
	Type             int
	Content          *string
	TokenName        *string
	ModelName        *string
	Quota            int64
	PromptTokens     int64
	CompletionTokens int64
	UseTime          int64
	IsStream         int64
	ChannelID        int64
	TokenID          *int64
	Other            *string
}

// readLegacyUsers 读取全部用户（按 id 升序）。
func readLegacyUsers(db *gorm.DB) ([]legacyUser, error) {
	var rows []legacyUser
	err := db.Raw(`SELECT id, username, password, display_name, role, status, email,
		quota, used_quota, "group", created_at, last_login_at
		FROM users ORDER BY id`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("读取旧库 users: %w", err)
	}
	return rows, nil
}

// readLegacyTokens 读取全部令牌（按 id 升序）。
func readLegacyTokens(db *gorm.DB) ([]legacyToken, error) {
	var rows []legacyToken
	err := db.Raw(`SELECT id, user_id, key, status, name, created_time, accessed_time,
		expired_time, remain_quota, unlimited_quota, model_limits_enabled, model_limits,
		allow_ips, "group"
		FROM tokens ORDER BY id`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("读取旧库 tokens: %w", err)
	}
	return rows, nil
}

// readLegacyChannels 读取全部渠道（按 id 升序）。
func readLegacyChannels(db *gorm.DB) ([]legacyChannel, error) {
	var rows []legacyChannel
	err := db.Raw(`SELECT id, type, key, status, name, weight, created_time, base_url,
		models, "group", model_mapping, priority, param_override
		FROM channels ORDER BY id`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("读取旧库 channels: %w", err)
	}
	return rows, nil
}

// readLegacyOptions 读取全部 options（按 key 升序，保证报告顺序确定）。
func readLegacyOptions(db *gorm.DB) ([]legacyOption, error) {
	var rows []legacyOption
	err := db.Raw(`SELECT key, value FROM options ORDER BY key`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("读取旧库 options: %w", err)
	}
	return rows, nil
}

// readLegacyRedemptions 读取全部兑换码（按 id 升序）。
func readLegacyRedemptions(db *gorm.DB) ([]legacyRedemption, error) {
	var rows []legacyRedemption
	err := db.Raw(`SELECT id, user_id, key, status, name, quota, created_time,
		redeemed_time, used_user_id, expired_time
		FROM redemptions ORDER BY id`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("读取旧库 redemptions: %w", err)
	}
	return rows, nil
}

// readLegacyLogsByType 读取指定类型的日志行（按 id 升序）。type 取旧网关语义：
// 2=consume 计费日志，1=充值类日志。
func readLegacyLogsByType(db *gorm.DB, typ int) ([]legacyLog, error) {
	var rows []legacyLog
	err := db.Raw(`SELECT id, user_id, created_at, type, content, token_name, model_name,
		quota, prompt_tokens, completion_tokens, use_time, is_stream, channel_id, token_id, other
		FROM logs WHERE type = ? ORDER BY id`, typ).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("读取旧库 logs(type=%d): %w", typ, err)
	}
	return rows, nil
}

// readLegacyLogSkippedTypes 统计未迁移日志类型的行数（type 升序）。
func readLegacyLogSkippedTypes(db *gorm.DB, migratedTypes ...int) ([]TypeCount, error) {
	var rows []TypeCount
	err := db.Raw(`SELECT type, COUNT(*) AS count FROM logs
		WHERE type NOT IN ? GROUP BY type ORDER BY type`, migratedTypes).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("统计旧库 logs 未迁类型: %w", err)
	}
	return rows, nil
}

// readLegacyQuotaPerUnit 读取旧库 QuotaPerUnit 配置：未落库返回 (值=0, true)，
// 落库返回 (值, false)。
func readLegacyQuotaPerUnit(db *gorm.DB) (int64, bool, error) {
	var rows []legacyOption
	err := db.Raw(`SELECT key, value FROM options WHERE key = ?`, optionKeyQuotaPerUnit).Scan(&rows).Error
	if err != nil {
		return 0, false, fmt.Errorf("读取旧库 %s: %w", optionKeyQuotaPerUnit, err)
	}
	if len(rows) == 0 {
		return 0, true, nil
	}
	var v int64
	if _, err := fmt.Sscanf(rows[0].Value, "%d", &v); err != nil {
		return 0, false, fmt.Errorf("解析旧库 %s=%q: %w", optionKeyQuotaPerUnit, rows[0].Value, err)
	}
	return v, false, nil
}
