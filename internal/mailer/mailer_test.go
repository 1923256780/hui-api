// mailer_test.go 邮件发送测试（M3-wave1）：
// 配置读取缺省值、报文组装（Q 编码主题 + base64 正文折行）、AUTH LOGIN 机制、
// 未启用门控，以及本地自签 TLS 假 SMTP 服务器的 465 隐式 TLS 端到端发送。
package mailer

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// mapGetter 以内存 map 构造 Getter（模拟运行轨配置读取）。
func mapGetter(m map[string]string) Getter {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// TestReadConfigDefaults 配置缺省：端口回退 465、From 回退 Username、非法端口忽略。
func TestReadConfigDefaults(t *testing.T) {
	cfg := readConfig(mapGetter(nil))
	if cfg.Port != DefaultPort {
		t.Fatalf("缺省端口应 %d，实际 %d", DefaultPort, cfg.Port)
	}
	cfg = readConfig(mapGetter(map[string]string{
		KeyEnabled: "true", KeyHost: "smtp.test", KeyUsername: "u", KeyPassword: "p",
	}))
	if cfg.From != "u" {
		t.Fatalf("From 应回退 Username，实际 %q", cfg.From)
	}
	if !cfg.Enabled || cfg.Host != "smtp.test" {
		t.Fatalf("配置读取不符: %+v", cfg)
	}
	cfg = readConfig(mapGetter(map[string]string{KeyPort: "not-a-port"}))
	if cfg.Port != DefaultPort {
		t.Fatalf("非法端口应回退缺省，实际 %d", cfg.Port)
	}
}

// TestSendDisabled 未启用门控：nil Getter 与 enabled=false 均返回 ErrDisabled。
func TestSendDisabled(t *testing.T) {
	if err := New(nil).Send("a@b.c", "s", "body"); err == nil {
		t.Fatal("nil Getter 应返回错误")
	}
	if err := New(mapGetter(map[string]string{KeyEnabled: "false"})).Send("a@b.c", "s", "b"); err == nil {
		t.Fatal("enabled=false 应返回错误")
	}
	err := New(mapGetter(map[string]string{KeyEnabled: "true"})).Send("a@b.c", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "smtp.host") {
		t.Fatalf("enabled 但缺 host 应报错，实际 %v", err)
	}
}

// TestBuildMessage 报文组装：CRLF 行尾、Q 编码主题、base64 正文 76 字符折行。
func TestBuildMessage(t *testing.T) {
	msg := string(buildMessage("from@test", "to@test", "验证码 标题", strings.Repeat("正文", 100)))
	if !strings.Contains(msg, "From: from@test\r\n") || !strings.Contains(msg, "To: to@test\r\n") {
		t.Fatalf("报文应含 From/To 头: %q", msg)
	}
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("中文主题应 Q 编码: %q", msg)
	}
	if !strings.Contains(msg, "Content-Transfer-Encoding: base64") {
		t.Fatalf("正文应为 base64 传输编码: %q", msg)
	}
	head, body, ok := strings.Cut(msg, "\r\n\r\n")
	if !ok {
		t.Fatal("报文应含头体分隔空行")
	}
	for _, line := range strings.Split(body, "\r\n") {
		if line != "" && len(line) > 76 {
			t.Fatalf("base64 行超 76 字符: %q (head=%q)", line, head)
		}
	}
}

// TestLoginAuthMechanism AUTH LOGIN 两段交互：Start 返回 LOGIN，Next 按
// Username:/Password: 提示返回明文凭据，未知提示报错。
func TestLoginAuthMechanism(t *testing.T) {
	a := &loginAuth{username: "user", password: "pass"}
	proto, toServer, err := a.Start(nil)
	if err != nil || proto != "LOGIN" || toServer != nil {
		t.Fatalf("Start 应返回 LOGIN，实际 %q %v %v", proto, toServer, err)
	}
	v, err := a.Next([]byte("Username:"), true)
	if err != nil || string(v) != "user" {
		t.Fatalf("Username 提示应返回用户名，实际 %q %v", v, err)
	}
	v, err = a.Next([]byte("password"), true)
	if err != nil || string(v) != "pass" {
		t.Fatalf("Password 提示应返回口令，实际 %q %v", v, err)
	}
	if _, err := a.Next([]byte("bogus"), true); err == nil {
		t.Fatal("未知提示应报错")
	}
}

// fakeSMTPServer 最小 TLS SMTP 服务器：465 隐式 TLS + EHLO/AUTH LOGIN/MAIL/RCPT/
// DATA/QUIT 会话，记录凭据与信件（验证 AUTH LOGIN 与报文端到端）。
type fakeSMTPServer struct {
	addr     string
	username string // AUTH LOGIN 收到的用户名
	password string // AUTH LOGIN 收到的口令
	from     string
	to       string
	data     string
}

func newFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	// 自签证书：SAN 覆盖 smtp.test 与 127.0.0.1。
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "smtp.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"smtp.test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	srv := &fakeSMTPServer{}
	go func() {
		for {
			conn, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go srv.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = tlsLn.Close() })
	srv.addr = ln.Addr().String()
	return srv
}

// serve 处理一次 SMTP 会话（每次连接一封邮件；响应码见 RFC 5321）。
func (s *fakeSMTPServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	writeLine := func(line string) {
		_, _ = conn.Write([]byte(line + "\r\n"))
	}
	writeLine("220 smtp.test ESMTP")
	var inData bool
	var dataLines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				s.data = strings.Join(dataLines, "\r\n")
				inData = false
				writeLine("250 ok")
				continue
			}
			dataLines = append(dataLines, line)
			continue
		}
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// 多行响应：仅末行不带 "-"（RFC 5321）。不宣告 SMTPUTF8/8BITMIME——
			// net/smtp 会向 MAIL FROM 附加对应参数，简化假服务器的信封断言。
			writeLine("250-smtp.test")
			writeLine("250-AUTH LOGIN PLAIN")
			writeLine("250 HELP")
		case line == "AUTH LOGIN":
			writeLine("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
			resp, err := r.ReadString('\n')
			if err != nil {
				return
			}
			dec, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(resp))
			if decErr != nil {
				writeLine("535 bad")
				continue
			}
			s.username = string(dec)
			writeLine("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
			resp, err = r.ReadString('\n')
			if err != nil {
				return
			}
			dec, decErr = base64.StdEncoding.DecodeString(strings.TrimSpace(resp))
			if decErr != nil {
				writeLine("535 bad")
				continue
			}
			s.password = string(dec)
			writeLine("235 ok")
		case strings.HasPrefix(line, "MAIL FROM:"):
			s.from = strings.Trim(strings.TrimPrefix(line, "MAIL FROM:"), "<>")
			writeLine("250 ok")
		case strings.HasPrefix(line, "RCPT TO:"):
			s.to = strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), "<>")
			writeLine("250 ok")
		case line == "DATA":
			inData = true
			dataLines = nil
			writeLine("354 go")
		case line == "QUIT":
			writeLine("221 bye")
			return
		default:
			writeLine("500 unknown")
		}
	}
}

// TestSendOverImplicitTLS 端到端：465 隐式 TLS + AUTH LOGIN + DATA 全链路，
// 服务器应收到正确凭据与信件头体。
func TestSendOverImplicitTLS(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, portStr, _ := net.SplitHostPort(srv.addr)
	m := New(mapGetter(map[string]string{
		KeyEnabled: "true", KeyHost: "smtp.test", KeyPort: portStr,
		KeyUsername: "mailer-user", KeyPassword: "mailer-pass",
	}))
	m.hostOverride = host // 生产为空；测试指向本地假服务器（仅主机名，端口走配置）
	m.insecureTLS = true  // 自签证书（仅测试）
	if err := m.Send("rcpt@dest.test", "验证码邮件", "正文 hello"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if srv.username != "mailer-user" || srv.password != "mailer-pass" {
		t.Fatalf("AUTH LOGIN 凭据不符: user=%q pass=%q", srv.username, srv.password)
	}
	if !strings.HasPrefix(srv.from, "mailer-user") || srv.to != "rcpt@dest.test" {
		t.Fatalf("信封不符: from=%q to=%q", srv.from, srv.to)
	}
	if !strings.Contains(srv.data, "To: rcpt@dest.test") ||
		!strings.Contains(srv.data, "Subject: =?utf-8?") {
		t.Fatalf("信件头不符: %q", srv.data)
	}
	if !strings.Contains(srv.data, base64.StdEncoding.EncodeToString([]byte("正文 hello"))) {
		t.Fatalf("信件正文不符: %q", srv.data)
	}
}

// TestSendFailureReturnsError 发送失败返回错误且不 panic（连接拒绝场景）。
func TestSendFailureReturnsError(t *testing.T) {
	m := New(mapGetter(map[string]string{
		KeyEnabled: "true", KeyHost: "127.0.0.1", KeyPort: "1",
	}))
	if err := m.Send("a@b.c", "s", "b"); err == nil {
		t.Fatal("连接拒绝应返回错误")
	}
}

// TestKeyConstants 运行轨键与白名单前缀约定一致（防手滑改名，docs/05 键表）。
func TestKeyConstants(t *testing.T) {
	for _, k := range []string{KeyEnabled, KeyHost, KeyPort, KeyUsername, KeyPassword, KeyFrom} {
		if !strings.HasPrefix(k, "smtp.") {
			t.Fatalf("键 %q 应带 smtp. 前缀", k)
		}
	}
}
