// Package socks 实现了 SOCKS5 (RFC 1928 / RFC 1929) 与 HTTP CONNECT 代理拨号。
// 不依赖任何第三方库，输出一个标准的 DialContext，可直接塞进 http.Transport。
package socks

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// ProxyType 代理协议类型
type ProxyType string

const (
	TypeDirect ProxyType = "direct" // 直连
	TypeSOCKS5 ProxyType = "socks5" // SOCKS5（客户端本地解析或交由代理解析）
	TypeHTTP   ProxyType = "http"   // HTTP CONNECT 隧道
)

// Config 一条上游代理配置
type Config struct {
	Type     ProxyType `json:"type"`
	Addr     string    `json:"addr"`     // host:port
	Username string    `json:"username"` // 可选
	Password string    `json:"password"` // 可选
	Timeout  int       `json:"timeout"`  // 秒，0 表示默认 10s
}

// Empty 表示无需经过代理
func (c *Config) Empty() bool {
	return c == nil || c.Type == "" || c.Type == TypeDirect || c.Addr == ""
}

func (c *Config) timeout() time.Duration {
	if c == nil || c.Timeout <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.Timeout) * time.Second
}

// Key 用于 transport 缓存
func (c *Config) Key() string {
	if c.Empty() {
		return "direct"
	}
	return fmt.Sprintf("%s|%s|%s|%s", c.Type, c.Addr, c.Username, c.Password)
}

// DialContext 经过（或不经过）代理建立 TCP 连接
func DialContext(ctx context.Context, cfg *Config, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("socks: 不支持的网络类型 %q", network)
	}
	if cfg.Empty() {
		d := &net.Dialer{Timeout: cfg.timeout()}
		return d.DialContext(ctx, network, addr)
	}
	switch cfg.Type {
	case TypeSOCKS5:
		return dialSOCKS5(ctx, cfg, addr)
	case TypeHTTP:
		return dialHTTPConnect(ctx, cfg, addr)
	default:
		return nil, fmt.Errorf("socks: 未知代理类型 %q", cfg.Type)
	}
}

func dialSOCKS5(ctx context.Context, cfg *Config, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: cfg.timeout()}
	conn, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("socks5: 连接代理服务器 %s 失败: %w", cfg.Addr, err)
	}
	// 握手阶段限时，之后交还给调用方
	deadline := time.Now().Add(cfg.timeout())
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	// 清除 deadline 后返回，避免长连接被握手超时砍断
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		} else {
			_ = conn.SetDeadline(time.Time{})
		}
	}()

	// ---- 协商认证方式 ----
	methods := []byte{0x00} // NO AUTH
	if cfg.Username != "" {
		methods = append(methods, 0x02) // USERNAME/PASSWORD
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return nil, fmt.Errorf("socks5: 发送协商请求失败: %w", err)
	}
	sel := make([]byte, 2)
	if _, err := io.ReadFull(conn, sel); err != nil {
		return nil, fmt.Errorf("socks5: 读取协商响应失败: %w", err)
	}
	if sel[0] != 0x05 {
		return nil, fmt.Errorf("socks5: 代理服务器版本不匹配 (0x%02x)", sel[0])
	}
	switch sel[1] {
	case 0x00:
		// 无需认证
	case 0x02:
		if err := socks5Auth(conn, cfg); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("socks5: 代理服务器拒绝了所有认证方式（0xff）")
	}

	// ---- 发送 CONNECT 请求 ----
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("socks5: 目标地址非法 %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("socks5: 目标端口非法 %q", portStr)
	}
	var atyp byte
	var dest []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			atyp, dest = 0x01, ip4
		} else {
			atyp, dest = 0x04, ip.To16()
		}
	} else {
		if len(host) > 255 {
			return nil, errors.New("socks5: 目标域名过长")
		}
		atyp = 0x03
		dest = append([]byte{byte(len(host))}, []byte(host)...)
	}
	req := []byte{0x05, 0x01, 0x00, atyp}
	req = append(req, dest...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("socks5: 发送 CONNECT 失败: %w", err)
	}

	// ---- 读取响应 ----
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, fmt.Errorf("socks5: 读取 CONNECT 响应失败: %w", err)
	}
	if hdr[0] != 0x05 {
		return nil, fmt.Errorf("socks5: 响应版本不匹配 (0x%02x)", hdr[0])
	}
	if hdr[1] != 0x00 {
		return nil, fmt.Errorf("socks5: 代理拒绝连接，错误码 0x%02x (%s)", hdr[1], socks5Err(hdr[1]))
	}
	// 丢弃 BND.ADDR / BND.PORT
	var skip int64
	switch hdr[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		b := make([]byte, 1)
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, err
		}
		skip = int64(b[0])
	default:
		return nil, fmt.Errorf("socks5: 未知地址类型 0x%02x", hdr[3])
	}
	if _, err := io.CopyN(io.Discard, conn, skip+2); err != nil {
		return nil, err
	}
	ok = true
	return conn, nil
}

func socks5Auth(conn net.Conn, cfg *Config) error {
	u := []byte(cfg.Username)
	p := []byte(cfg.Password)
	if len(u) > 255 || len(p) > 255 {
		return errors.New("socks5: 代理用户名或密码过长")
	}
	req := []byte{0x01, byte(len(u))}
	req = append(req, u...)
	req = append(req, byte(len(p)))
	req = append(req, p...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: 发送认证失败: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5: 读取认证响应失败: %w", err)
	}
	if resp[1] != 0x00 {
		return errors.New("socks5: 代理认证失败（用户名或密码错误）")
	}
	return nil
}

func socks5Err(code byte) string {
	switch code {
	case 0x01:
		return "常规失败"
	case 0x02:
		return "规则集不允许"
	case 0x03:
		return "网络不可达"
	case 0x04:
		return "主机不可达"
	case 0x05:
		return "连接被拒"
	case 0x06:
		return "TTL 过期"
	case 0x07:
		return "命令不支持"
	case 0x08:
		return "地址类型不支持"
	default:
		return "未知错误"
	}
}

func dialHTTPConnect(ctx context.Context, cfg *Config, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: cfg.timeout()}
	conn, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("http 代理: 连接 %s 失败: %w", cfg.Addr, err)
	}
	deadline := time.Now().Add(cfg.timeout())
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		} else {
			_ = conn.SetDeadline(time.Time{})
		}
	}()

	var buf []byte
	buf = append(buf, "CONNECT "+addr+" HTTP/1.1\r\nHost: "+addr+"\r\n"...)
	if cfg.Username != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(cfg.Username + ":" + cfg.Password))
		buf = append(buf, "Proxy-Authorization: Basic "+cred+"\r\n"...)
	}
	buf = append(buf, "\r\n"...)
	if _, err := conn.Write(buf); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("http 代理: 读取响应失败: %w", err)
	}
	// HTTP/1.1 200 Connection established
	if len(line) < 12 || line[9] != '2' {
		return nil, fmt.Errorf("http 代理: 隧道建立失败: %s", trimCRLF(line))
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	if br.Buffered() > 0 {
		return nil, errors.New("http 代理: 隧道建立后存在多余数据")
	}
	ok = true
	return conn, nil
}

func trimCRLF(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

// Test 用于后台「测试连通性」：通过代理去连一个目标地址
func Test(ctx context.Context, cfg *Config, target string) (time.Duration, error) {
	if target == "" {
		target = "www.google.com:80"
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "80")
	}
	start := time.Now()
	conn, err := DialContext(ctx, cfg, "tcp", target)
	if err != nil {
		return 0, err
	}
	conn.Close()
	return time.Since(start), nil
}
