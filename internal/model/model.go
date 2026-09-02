// Package model 定义 hui-api 的六张核心表模型。
//
// 设计来源：docs/03-数据模型与迁移.md。列名采用行业通用语义，quota 单位约定
// 500000 quota = $1，所有落库金额均为 quota 整数。时间列统一为 unix 秒（int64）。
// 本包只放模型与枚举常量，不包含任何业务逻辑。
package model

// 上游渠道协议类型（channels.type）。
const (
	ChannelTypeOpenAICompatible = 1 // OpenAI 兼容协议
	ChannelTypeAnthropic        = 2 // Anthropic Messages 协议
)

// 通用状态枚举（channels.status / tokens.status / users.status）。
const (
	StatusEnabled  = 1 // 启用
	StatusDisabled = 2 // 手动禁用
	StatusTripped  = 3 // 熔断中（仅 channels 使用）
)

// 兑换码状态（redemptions.status）。
const (
	RedemptionUnused   = 1 // 未使用
	RedemptionRedeemed = 2 // 已核销
	RedemptionVoided   = 3 // 已作废
)

// EpochForever 表示“永不过期”的时间哨兵值（expired_time 语义）。
const EpochForever = -1

// QuotaPerDollar 是 quota 与美元的换算基数：500000 quota = $1。
const QuotaPerDollar = 500000

// 用户角色值（users.role）。
const (
	RoleUser  = 1
	RoleAdmin = 100
)

// Channel 上游渠道配置：地址、密钥、模型清单、优先级/权重、状态。
type Channel struct {
	ID            int64  `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	Name          string `gorm:"column:name;size:128;not null;default:''" json:"name"`
	Type          int    `gorm:"column:type;not null;default:1" json:"type"`
	BaseURL       string `gorm:"column:base_url;size:512;not null;default:''" json:"base_url"`
	Key           string `gorm:"column:key;size:512;not null;default:''" json:"-"` // 上游密钥，禁止序列化输出
	Models        string `gorm:"column:models;size:2048;not null;default:''" json:"models"`
	Priority      int64  `gorm:"column:priority;not null;default:0" json:"priority"`
	Weight        int64  `gorm:"column:weight;not null;default:0" json:"weight"`
	Status        int    `gorm:"column:status;not null;default:1" json:"status"`
	ParamOverride string `gorm:"column:param_override;size:2048;not null;default:''" json:"param_override"` // 渠道级请求参数改写 JSON（M1-wave2），语义见 internal/override
	CreatedTime   int64  `gorm:"column:created_time;not null;default:0" json:"created_time"`
	UpdatedTime   int64  `gorm:"column:updated_time;not null;default:0" json:"updated_time"`
}

// TableName 显式指定表名，避免依赖 GORM 复数化推断。
func (Channel) TableName() string { return "channels" }

// Token 访问令牌：明文仅在创建时返回一次，库内鉴权唯一依据是 key_hash（SHA-256 hex）。
// ExpiredTime = -1 表示永不过期；BudgetDuration 支持 '', '24h', '7d', '30d', 'monthly'。
type Token struct {
	ID             int64  `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	UserID         int64  `gorm:"column:user_id;not null;default:0;index" json:"user_id"`
	Name           string `gorm:"column:name;size:128;not null;default:''" json:"name"`
	Key            string `gorm:"column:key;size:128;uniqueIndex" json:"-"`              // 明文兼容列，允许为空，禁止序列化输出
	KeyHash        string `gorm:"column:key_hash;size:64;not null;uniqueIndex" json:"-"` // SHA-256 hex，鉴权唯一依据
	Status         int    `gorm:"column:status;not null;default:1" json:"status"`
	Quota          int64  `gorm:"column:quota;not null;default:0" json:"quota"` // 预算周期内的总额度
	RemainQuota    int64  `gorm:"column:remain_quota;not null;default:0" json:"remain_quota"`
	UnlimitedQuota bool   `gorm:"column:unlimited_quota;not null;default:false" json:"unlimited_quota"`
	BudgetDuration string `gorm:"column:budget_duration;size:16;not null;default:''" json:"budget_duration"`
	BudgetResetAt  int64  `gorm:"column:budget_reset_at;not null;default:0" json:"budget_reset_at"` // 下次预算重置时间（unix 秒）
	TPMRPM         string `gorm:"column:tpm_rpm;size:128;not null;default:''" json:"tpm_rpm"`       // JSON: {"tpm":..,"rpm":..}
	Tags           string `gorm:"column:tags;size:512;not null;default:''" json:"tags"`             // JSON 数组，如 ["team-a"]
	ExpiredTime    int64  `gorm:"column:expired_time;not null;default:-1" json:"expired_time"`
	CreatedTime    int64  `gorm:"column:created_time;not null;default:0" json:"created_time"`
	AccessedTime   int64  `gorm:"column:accessed_time;not null;default:0" json:"accessed_time"`
}

// TableName 显式指定表名。
func (Token) TableName() string { return "tokens" }

// User 用户与余额。用户余额（quota）独立于令牌预算，支持按用户维度审计。
type User struct {
	ID            int64  `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	Username      string `gorm:"column:username;size:64;not null;uniqueIndex" json:"username"`
	PasswordHash  string `gorm:"column:password_hash;size:128;not null;default:''" json:"-"`
	DisplayName   string `gorm:"column:display_name;size:64;not null;default:''" json:"display_name"`
	Role          int    `gorm:"column:role;not null;default:1" json:"role"`
	Status        int    `gorm:"column:status;not null;default:1" json:"status"`
	Quota         int64  `gorm:"column:quota;not null;default:0" json:"quota"`
	UsedQuota     int64  `gorm:"column:used_quota;not null;default:0" json:"used_quota"`
	Email         string `gorm:"column:email;size:128;not null;default:'';index" json:"email"`
	CreatedTime   int64  `gorm:"column:created_time;not null;default:0" json:"created_time"`
	LastLoginTime int64  `gorm:"column:last_login_time;not null;default:0" json:"last_login_time"`
}

// TableName 显式指定表名。
func (User) TableName() string { return "users" }

// Redemption 兑换码：面值（quota 整数）、状态、核销记录。ExpiredTime = -1 表示永不过期。
type Redemption struct {
	ID          int64  `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	Key         string `gorm:"column:key;size:64;not null;uniqueIndex" json:"key"`
	Name        string `gorm:"column:name;size:128;not null;default:''" json:"name"`
	Status      int    `gorm:"column:status;not null;default:1" json:"status"`
	Quota       int64  `gorm:"column:quota;not null;default:0" json:"quota"`
	CreatedBy   int64  `gorm:"column:created_by;not null;default:0" json:"created_by"`
	UsedBy      int64  `gorm:"column:used_by;not null;default:0" json:"used_by"`
	UsedTime    int64  `gorm:"column:used_time;not null;default:0" json:"used_time"`
	ExpiredTime int64  `gorm:"column:expired_time;not null;default:-1" json:"expired_time"`
	CreatedTime int64  `gorm:"column:created_time;not null;default:0" json:"created_time"`
}

// TableName 显式指定表名。
func (Redemption) TableName() string { return "redemptions" }

// Option 运行时配置（key-value 扁平表）。只存非默认值：未落库的键由调用方提供代码内默认值。
// 同时承担迁移元数据（schema_version）的存取。运行轨热更数据源，变更经 config.Runtime 热加载生效。
type Option struct {
	Key   string `gorm:"column:key;primaryKey;size:128" json:"key"`
	Value string `gorm:"column:value;size:2048;not null;default:''" json:"value"`
}

// TableName 显式指定表名。
func (Option) TableName() string { return "options" }

// Log 请求与计费日志：对账的事实来源。Detail 存计费依据 JSON（可反向重算验证）。
type Log struct {
	ID               int64  `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	UserID           int64  `gorm:"column:user_id;not null;default:0;index" json:"user_id"`
	TokenID          int64  `gorm:"column:token_id;not null;default:0;index" json:"token_id"`
	ChannelID        int64  `gorm:"column:channel_id;not null;default:0;index" json:"channel_id"`
	Protocol         string `gorm:"column:protocol;size:16;not null;default:''" json:"protocol"`
	ModelName        string `gorm:"column:model_name;size:128;not null;default:'';index" json:"model_name"`
	PromptTokens     int    `gorm:"column:prompt_tokens;not null;default:0" json:"prompt_tokens"`
	CompletionTokens int    `gorm:"column:completion_tokens;not null;default:0" json:"completion_tokens"`
	Quota            int64  `gorm:"column:quota;not null;default:0" json:"quota"`
	UseTime          int64  `gorm:"column:use_time;not null;default:0" json:"use_time"` // 请求耗时（秒）
	IsStream         bool   `gorm:"column:is_stream;not null;default:false" json:"is_stream"`
	Detail           string `gorm:"column:detail;size:2048;not null;default:''" json:"detail"` // 计费依据 JSON
	CreatedTime      int64  `gorm:"column:created_time;not null;default:0;index" json:"created_time"`
}

// TableName 显式指定表名。
func (Log) TableName() string { return "logs" }

// AllModels 返回全部六表模型实例，供 AutoMigrate 与测试遍历使用。
func AllModels() []any {
	return []any{
		&Channel{},
		&Token{},
		&User{},
		&Redemption{},
		&Option{},
		&Log{},
	}
}
