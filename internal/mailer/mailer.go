// Package mailer 实现注册/找回密码等场景的邮件发送（M3-wave1，docs/05）。
//
// 传输层：465 端口隐式 TLS（tls.Dial 直连后走 SMTP）；AUTH LOGIN 机制手工实现
// （两段 base64 交互，由 net/smtp.Auth 接口承载）。配置经 Getter 闭包读取
// （生产装配传 config.Runtime.Get，测试传内存 map），每次 Send 重新读配置——
// 管理面改 SMTP 配置热生效，无需重建实例。
//
// 错误约定：发送失败返回 error（含业务上下文 %w 包装），不 panic、不重试
// （重试语义由调用方决定，验证码场景调用方直接映射 5xx）。
package mailer

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Getter 运行轨配置读取函数（键不存在返回 ("", false)）。
type Getter func(key string) (string, bool)

// Mailer 是邮件发送接口（SMTP 实现可被测试 mock 替换）。
type Mailer interface {
	Send(to, subject, body string) error
}

// 运行轨配置键（options 白名单 smtp.* 前缀，docs/05 键表）。
const (
	KeyEnabled  = "smtp.enabled"
	KeyHost     = "smtp.host"
	KeyPort     = "smtp.port"
	KeyUsername = "smtp.username"
	KeyPassword = "smtp.password"
	KeyFrom     = "smtp.from"
)

// 默认端口：465 隐式 TLS（本实现仅支持 465；587 STARTTLS 兜底为后续波次预留）。
const DefaultPort = 465

// ErrDisabled 表示 smtp.enabled 未开启（调用方映射 503 smtp_not_configured）。
var ErrDisabled = errors.New("mailer: smtp 未启用")

// dialTimeout / sendTimeout 网络超时：建连与整个会话各设上限，防止上游 SMTP
// 慢响应拖死管理面请求。
const (
	dialTimeout = 10 * time.Second
	sendTimeout = 15 * time.Second
)

// Config 是一次发送时的 SMTP 配置快照。
type Config struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

// readConfig 从 Getter 汇总 SMTP 配置（Port 缺省 465，From 缺省 Username）。
func readConfig(get Getter) Config {
	cfg := Config{}
	if v, ok := get(KeyEnabled); ok {
		cfg.Enabled = strings.EqualFold(strings.TrimSpace(v), "true")
	}
	cfg.Host, _ = get(KeyHost)
	cfg.Username, _ = get(KeyUsername)
	cfg.Password, _ = get(KeyPassword)
	cfg.From, _ = get(KeyFrom)
	if v, ok := get(KeyPort); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.Port = n
		}
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.From == "" {
		cfg.From = cfg.Username
	}
	return cfg
}

// SMTPMailer 是 Mailer 的 SMTP 实现（465 隐式 TLS）。
type SMTPMailer struct {
	get Getter
	// hostOverride 供测试注入本地 TLS 服务器地址（生产为空，直用 cfg.Host）。
	hostOverride string
	insecureTLS  bool // 测试自签证书跳过校验（生产恒 false）
}

// New 构造 SMTP 邮件发送器。get 允许为 nil（此时 Send 恒返回 ErrDisabled）。
func New(get Getter) *SMTPMailer {
	return &SMTPMailer{get: get}
}

// Send 发送一封纯文本邮件：465 隐式 TLS → EHLO → AUTH LOGIN → DATA。
// 任何一步失败以错误返回（%w 包装附上下文），不 panic。
func (m *SMTPMailer) Send(to, subject, body string) error {
	if m.get == nil {
		return ErrDisabled
	}
	cfg := readConfig(m.get)
	if !cfg.Enabled {
		return ErrDisabled
	}
	if cfg.Host == "" {
		return fmt.Errorf("mailer: smtp.host 未配置")
	}
	host := cfg.Host
	if m.hostOverride != "" {
		host = m.hostOverride
	}
	addr := net.JoinHostPort(host, strconv.Itoa(cfg.Port))
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	if m.insecureTLS {
		tlsCfg.InsecureSkipVerify = true // 仅测试（自签证书）；生产不启用
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: dialTimeout}, "tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("mailer: 连接 %s 失败: %w", addr, err)
	}
	if err := conn.SetDeadline(time.Now().Add(sendTimeout)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: 设置超时失败: %w", err)
	}
	cl, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: SMTP 握手失败: %w", err)
	}
	defer func() { _ = cl.Close() }()

	if err := cl.Hello(cfg.Host); err != nil {
		return fmt.Errorf("mailer: EHLO 失败: %w", err)
	}
	if ok, _ := cl.Extension("AUTH"); ok && cfg.Username != "" {
		if err := cl.Auth(&loginAuth{username: cfg.Username, password: cfg.Password}); err != nil {
			return fmt.Errorf("mailer: AUTH LOGIN 失败: %w", err)
		}
	}
	from := cfg.From
	if from == "" {
		from = "postmaster@localhost"
	}
	if err := cl.Mail(from); err != nil {
		return fmt.Errorf("mailer: MAIL FROM 失败: %w", err)
	}
	if err := cl.Rcpt(to); err != nil {
		return fmt.Errorf("mailer: RCPT TO 失败: %w", err)
	}
	w, err := cl.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA 失败: %w", err)
	}
	if _, err := w.Write(buildMessage(from, to, subject, body)); err != nil {
		_ = w.Close()
		return fmt.Errorf("mailer: 写入邮件体失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: 结束邮件体失败: %w", err)
	}
	if err := cl.Quit(); err != nil {
		return fmt.Errorf("mailer: QUIT 失败: %w", err)
	}
	return nil
}

// buildMessage 组装 RFC 5322 报文：UTF-8 主题 Q 编码、正文 base64（防行长溢出）。
func buildMessage(from, to, subject, body string) []byte {
	headers := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: <" + randomHex(12) + "@hui-api>",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: base64",
	}, "\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	// base64 按每行 76 字符折行（RFC 2045）。
	var lines []string
	for len(encoded) > 76 {
		lines = append(lines, encoded[:76])
		encoded = encoded[76:]
	}
	lines = append(lines, encoded)
	return []byte(headers + "\r\n\r\n" + strings.Join(lines, "\r\n") + "\r\n")
}

// loginAuth 手工实现 AUTH LOGIN 机制：两段 base64 交互（服务器依次提示
// Username:/Password:，客户端回明文，net/smtp 负责传输层 base64 编码）。
type loginAuth struct {
	username, password string
}

func (a *loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:", "username":
		return []byte(a.username), nil
	case "password:", "password":
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("mailer: AUTH LOGIN 未知提示: %q", string(fromServer))
	}
}

// randomHex 生成 n 字节随机数的 hex 字符串（Message-ID 用）。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return strings.ToLower(fmt.Sprintf("%x", buf))
}
