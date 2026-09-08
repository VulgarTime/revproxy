// Package config 定义配置模型与持久化存储，支持运行时热重载。
package config

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const CurrentVersion = 1

// ---------- 模型 ----------

type Config struct {
	Version   int         `json:"version"`
	UpdatedAt time.Time   `json:"updatedAt"`
	Listen    Listen      `json:"listen"`
	Admin     Admin       `json:"admin"`
	Log       Log         `json:"log"`
	Global    Global      `json:"global"`
	Proxies   []Proxy     `json:"proxies"` // 命名的上游代理（SOCKS5 等）池
	Routes    []Route     `json:"routes"`
	TLS       TLSConfig   `json:"tls"`
	Snippets  []Snippet   `json:"snippets,omitempty"`
	Extra     interface{} `json:"extra,omitempty"`
}

type Listen struct {
	Addr         string `json:"addr"`         // 代理监听地址，如 :8080
	AdminAddr    string `json:"adminAddr"`    // 管理后台独立端口，为空则与代理共用 Addr
	AdminPath    string `json:"adminPath"`    // 共用端口时的管理路径前缀，默认 /__admin
	ReadTimeout  int    `json:"readTimeout"`  // 秒，0=不限制（WebSocket 必需）
	WriteTimeout int    `json:"writeTimeout"` // 秒，0=不限制
	IdleTimeout  int    `json:"idleTimeout"`  // 秒
	MaxHeaderKB  int    `json:"maxHeaderKB"`
}

type Admin struct {
	Username   string `json:"username"`
	PassHash   string `json:"passHash"`   // salt:hash
	Secret     string `json:"secret"`     // HMAC 签名密钥
	SessionTTL int    `json:"sessionTTL"` // 小时
	Enabled    bool   `json:"enabled"`    // 是否暴露管理后台
}

type Log struct {
	Level  string `json:"level"`  // debug|info|warn|error
	Access bool   `json:"access"` // 是否记录访问日志
	File   string `json:"file"`   // 访问日志文件，空=仅内存环形缓冲
	Buffer int    `json:"buffer"` // 内存保留条数
}

type Global struct {
	DefaultProxy    string `json:"defaultProxy"` // 默认上游代理 ID（所有未单独指定的路由）
	MaxConnsPerHost int    `json:"maxConnsPerHost"`
	MaxIdleConns    int    `json:"maxIdleConns"`
	DialTimeout     int    `json:"dialTimeout"`     // 秒
	ResponseTimeout int    `json:"responseTimeout"` // 秒，0=不限制
	KeepAlive       int    `json:"keepAlive"`       // 秒
	HideVersion     bool   `json:"hideVersion"`
	TrustProxy      bool   `json:"trustProxy"` // 是否信任 X-Forwarded-For（前面还有 CDN 时开启）
	EnableEcho      bool   `json:"enableEcho"` // 内置 /__echo 自测端点
	RateLimit       Rate   `json:"rateLimit"`  // 全局默认限流
}

type TLSConfig struct {
	Enabled  bool   `json:"enabled"`
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	Redirect bool   `json:"redirect"` // 80 → 443 跳转
	HTTPPort int    `json:"httpPort"`
}

type Proxy struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // socks5 | http | direct
	Addr     string `json:"addr"`
	Username string `json:"username"`
	Password string `json:"password"`
	Timeout  int    `json:"timeout"` // 秒
	Note     string `json:"note"`
}

type HeaderOp struct {
	Request  []KV     `json:"request,omitempty"`  // 转发给上游前设置
	Response []KV     `json:"response,omitempty"` // 回给客户端前设置
	Remove   []string `json:"remove,omitempty"`   // 从请求中删除
}

type KV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Rate struct {
	Enabled bool    `json:"enabled"`
	RPS     float64 `json:"rps"`
	Burst   int     `json:"burst"`
	PerIP   bool    `json:"perIp"`
}

type AccessControl struct {
	BasicAuth struct {
		Enabled  bool   `json:"enabled"`
		Username string `json:"username"`
		Password string `json:"password"`
		Realm    string `json:"realm"`
	} `json:"basicAuth"`
	IPAllow []string `json:"ipAllow"` // CIDR 或单 IP
	IPDeny  []string `json:"ipDeny"`
	Rate    Rate     `json:"rate"`
}

type HealthCheck struct {
	Enabled  bool   `json:"enabled"`
	Path     string `json:"path"`     // 空=纯 TCP 探测
	Interval int    `json:"interval"` // 秒
	Timeout  int    `json:"timeout"`  // 秒
	Fails    int    `json:"fails"`    // 连续失败多少次判定下线
	Passes   int    `json:"passes"`   // 连续成功多少次判定上线
	Codes    []int  `json:"codes"`    // 视为健康的状态码，默认 2xx/3xx
}

type Backend struct {
	URL     string `json:"url"`     // http://127.0.0.1:9000
	Weight  int    `json:"weight"`  // 权重，0/空视为 1
	ProxyID string `json:"proxyId"` // 覆盖路由级代理；"__direct__" 表示强制直连
	Backup  bool   `json:"backup"`  // 备用节点
}

// DomainRule 域名内嵌路径模式下，按域名指定走哪条代理
// 例如 a.com 直连、b.com 走 SOCKS5：用 match 通配，proxyId 引用上游代理池
type DomainRule struct {
	Match   string `json:"match"`   // 域名，支持 *.b.com 与 b.com
	ProxyID string `json:"proxyId"` // 该域名使用的代理；"__direct__" 表示直连；空=用路由默认
	Scheme  string `json:"scheme"`  // 该域名的协议覆盖：http | https；空=用路由级默认协议
}

type Route struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Enabled      bool          `json:"enabled"`
	Host         string        `json:"host"`                  // 空=任意；支持 *.example.com
	Path         string        `json:"path"`                  // /api
	Match        string        `json:"match"`                 // prefix | exact | regex
	Mode         string        `json:"mode"`                  // "" 普通模式 | "domain_in_path" 域名内嵌路径（万能反代）
	Scheme       string        `json:"scheme"`                // domain_in_path 模式下的目标协议：http | https，默认 http
	DomainRules  []DomainRule  `json:"domainRules,omitempty"` // 域名内嵌路径模式：不同域名走不同代理
	Priority     int           `json:"priority"`              // 越大越优先
	Rewrite      string        `json:"rewrite"`               // 把 Path 前缀替换为该值，null=不重写
	StripPath    bool          `json:"stripPath"`             // 移除 Path 前缀
	Backends     []Backend     `json:"backends"`
	LB           string        `json:"lb"`          // round_robin | weighted | least_conn | ip_hash | random
	ProxyID      string        `json:"proxyId"`     // 引用的代理 ID；空=用全局默认；__direct__=直连
	InsecureTLS  bool          `json:"insecureTLS"` // 跳过上游证书校验
	PreserveHost bool          `json:"preserveHost"`
	Retry        int           `json:"retry"`   // 网络错误重试次数
	Timeout      int           `json:"timeout"` // 单次请求总超时，秒，0=不限制
	Headers      HeaderOp      `json:"headers"`
	Health       HealthCheck   `json:"health"`
	Access       AccessControl `json:"access"`
	Note         string        `json:"note"`
	CreatedAt    time.Time     `json:"createdAt"`
	UpdatedAt    time.Time     `json:"updatedAt"`
}

type Snippet struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// ---------- 默认值 ----------

func Default(path string) *Config {
	secret, _ := randomHex(32)
	cfg := &Config{
		Version:   CurrentVersion,
		UpdatedAt: time.Now(),
		Listen: Listen{
			Addr:         ":8080",
			AdminAddr:    "",
			AdminPath:    "/__admin",
			ReadTimeout:  0,
			WriteTimeout: 0,
			IdleTimeout:  120,
			MaxHeaderKB:  64,
		},
		Admin: Admin{
			Username:   "admin",
			SessionTTL: 12,
			Enabled:    true,
			Secret:     secret,
		},
		Log: Log{Level: "info", Access: true, Buffer: 500},
		Global: Global{
			MaxConnsPerHost: 0,
			MaxIdleConns:    200,
			DialTimeout:     10,
			ResponseTimeout: 0,
			KeepAlive:       30,
			EnableEcho:      true,
		},
		Proxies: []Proxy{},
		Routes:  []Route{},
	}
	cfg.Admin.PassHash = HashPassword("admin123")
	return cfg
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashPassword 生成 salt:hash（PBKDF2 风格，纯标准库实现）
func HashPassword(pass string) string {
	salt, err := randomHex(16)
	if err != nil {
		salt = "revproxysalt"
	}
	return salt + ":" + derive(pass, salt)
}

func derive(pass, salt string) string {
	h := hmac.New(sha256.New, []byte(salt))
	h.Write([]byte(pass))
	buf := h.Sum(nil)
	for i := 0; i < 9999; i++ {
		h.Reset()
		h.Write(buf)
		buf = h.Sum(buf[:0])
	}
	return hex.EncodeToString(buf)
}

// VerifyPassword 校验密码
func VerifyPassword(pass, stored string) bool {
	parts := strings.SplitN(stored, ":", 2)
	if len(parts) != 2 {
		return false
	}
	return hmac.Equal([]byte(derive(pass, parts[0])), []byte(parts[1]))
}

func NewID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(b))
}

// ---------- Store ----------

type Store struct {
	mu     sync.RWMutex
	path   string
	cfg    *Config
	subs   []chan<- *Config
	closer chan struct{}
}

func Load(path string) (*Store, error) {
	if path == "" {
		path = "data/config.json"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	var cfg *Config
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		cfg = &Config{}
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
		}
	}
	if cfg == nil {
		cfg = Default(path)
		if err := writeFile(path, cfg); err != nil {
			return nil, err
		}
	}
	// 首次补齐密钥/默认密码后要落盘，否则每次重启都会让已登录会话失效
	needSave := cfg.Admin.Secret == "" || cfg.Admin.PassHash == ""
	cfg.normalize()
	if needSave {
		_ = writeFile(path, cfg)
	}
	s := &Store{path: path, cfg: cfg, closer: make(chan struct{})}
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update 原子替换配置并持久化，随后通知订阅者（热重载）
func (s *Store) Update(fn func(c *Config) error) error {
	s.mu.Lock()
	dup := s.cfg.clone()
	if err := fn(dup); err != nil {
		s.mu.Unlock()
		return err
	}
	dup.UpdatedAt = time.Now()
	dup.normalize()
	if err := writeFile(s.path, dup); err != nil {
		s.mu.Unlock()
		return err
	}
	s.cfg = dup
	subs := make([]chan<- *Config, len(s.subs))
	copy(subs, s.subs)
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- dup:
		case <-time.After(time.Second):
		}
	}
	return nil
}

// Subscribe 返回配置变更通道
func (s *Store) Subscribe() <-chan *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan *Config, 4)
	s.subs = append(s.subs, ch)
	return ch
}

// Reload 从磁盘重新加载（外部手改配置文件后调用）
func (s *Store) Reload() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	cfg := &Config{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return err
	}
	cfg.normalize()
	s.mu.Lock()
	s.cfg = cfg
	subs := make([]chan<- *Config, len(s.subs))
	copy(subs, s.subs)
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- cfg:
		case <-time.After(time.Second):
		}
	}
	return nil
}

func (c *Config) clone() *Config {
	raw, err := json.Marshal(c)
	if err != nil {
		return c
	}
	out := &Config{}
	if err := json.Unmarshal(raw, out); err != nil {
		return c
	}
	return out
}

// normalize 填默认值并修正非法输入
func (c *Config) normalize() {
	if c.Listen.Addr == "" {
		c.Listen.Addr = ":8080"
	}
	if c.Listen.AdminPath == "" {
		c.Listen.AdminPath = "/__admin"
	}
	c.Listen.AdminPath = "/" + strings.Trim(c.Listen.AdminPath, "/")
	if c.Admin.Username == "" {
		c.Admin.Username = "admin"
	}
	if c.Admin.PassHash == "" {
		c.Admin.PassHash = HashPassword("admin123")
	}
	if c.Admin.Secret == "" {
		c.Admin.Secret, _ = randomHex(32)
	}
	if c.Admin.SessionTTL <= 0 {
		c.Admin.SessionTTL = 12
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Buffer <= 0 {
		c.Log.Buffer = 500
	}
	if c.Global.MaxIdleConns <= 0 {
		c.Global.MaxIdleConns = 200
	}
	if c.Global.DialTimeout <= 0 {
		c.Global.DialTimeout = 10
	}
	if c.Global.KeepAlive <= 0 {
		c.Global.KeepAlive = 30
	}
	if c.TLS.HTTPPort == 0 {
		c.TLS.HTTPPort = 80
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.ID == "" {
			r.ID = NewID("rt")
		}
		if r.Match == "" {
			r.Match = "prefix"
		}
		if r.Mode == "domain_in_path" {
			if r.Scheme != "http" && r.Scheme != "https" {
				r.Scheme = "http"
			}
		}
		if r.LB == "" {
			r.LB = "round_robin"
		}
		if r.Path == "" {
			r.Path = "/"
		}
		if r.Health.Interval <= 0 {
			r.Health.Interval = 10
		}
		if r.Health.Timeout <= 0 {
			r.Health.Timeout = 3
		}
		if r.Health.Fails <= 0 {
			r.Health.Fails = 3
		}
		if r.Health.Passes <= 0 {
			r.Health.Passes = 2
		}
		for j := range r.Backends {
			if r.Backends[j].Weight <= 0 {
				r.Backends[j].Weight = 1
			}
		}
		if r.CreatedAt.IsZero() {
			r.CreatedAt = time.Now()
		}
		r.UpdatedAt = time.Now()
	}
	for i := range c.Proxies {
		if c.Proxies[i].ID == "" {
			c.Proxies[i].ID = NewID("px")
		}
		if c.Proxies[i].Type == "" {
			c.Proxies[i].Type = "socks5"
		}
		if c.Proxies[i].Timeout <= 0 {
			c.Proxies[i].Timeout = 10
		}
	}
	// 路由排序：优先级高的、路径长的优先匹配
	sort.SliceStable(c.Routes, func(i, j int) bool {
		if c.Routes[i].Priority != c.Routes[j].Priority {
			return c.Routes[i].Priority > c.Routes[j].Priority
		}
		if c.Routes[i].Host != "" && c.Routes[j].Host == "" {
			return true
		}
		return len(c.Routes[i].Path) > len(c.Routes[j].Path)
	})
}

func (c *Config) RouteByID(id string) *Route {
	for i := range c.Routes {
		if c.Routes[i].ID == id {
			return &c.Routes[i]
		}
	}
	return nil
}

func (c *Config) ProxyByID(id string) *Proxy {
	for i := range c.Proxies {
		if c.Proxies[i].ID == id {
			return &c.Proxies[i]
		}
	}
	return nil
}

func writeFile(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
