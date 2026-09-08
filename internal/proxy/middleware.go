package proxy

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"revproxy/internal/config"
)

// ---------- 客户端 IP ----------

// ClientIP 提取真实客户端 IP。trustProxy 为真时优先信任 X-Forwarded-For / X-Real-IP，
// 否则只用 TCP 对端地址（避免伪造绕过 IP 白名单）。
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[0]); ip != "" {
				return ip
			}
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- IP 白/黑名单 ----------

type IPMatcher struct {
	nets    []*net.IPNet
	singles []string
}

func NewIPMatcher(list []string) *IPMatcher {
	m := &IPMatcher{}
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			if _, n, err := net.ParseCIDR(item); err == nil {
				m.nets = append(m.nets, n)
				continue
			}
		}
		if ip := net.ParseIP(item); ip != nil {
			m.singles = append(m.singles, ip.String())
		}
	}
	return m
}

func (m *IPMatcher) Empty() bool { return len(m.nets) == 0 && len(m.singles) == 0 }

func (m *IPMatcher) Match(ip string) bool {
	if m == nil || m.Empty() {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, s := range m.singles {
		if s == parsed.String() {
			return true
		}
	}
	for _, n := range m.nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// ---------- 令牌桶限流 ----------

type bucket struct {
	tokens float64
	last   time.Time
	mu     sync.Mutex
}

func (b *bucket) take(rps float64, burst int, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.last = now
		b.tokens = float64(burst)
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rps
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type Limiter struct {
	rps    float64
	burst  int
	perIP  bool
	global *bucket
	mu     sync.Mutex
	ips    map[string]*bucket
	stop   chan struct{}
	once   sync.Once
}

func NewLimiter(r config.Rate) *Limiter {
	if r.RPS <= 0 {
		r.RPS = 10
	}
	if r.Burst <= 0 {
		r.Burst = int(r.RPS * 2)
		if r.Burst < 1 {
			r.Burst = 1
		}
	}
	l := &Limiter{
		rps:    r.RPS,
		burst:  r.Burst,
		perIP:  r.PerIP,
		global: &bucket{},
		ips:    map[string]*bucket{},
		stop:   make(chan struct{}),
	}
	go l.gc()
	return l
}

func (l *Limiter) Allow(key string) bool {
	now := time.Now()
	if !l.perIP {
		return l.global.take(l.rps, l.burst, now)
	}
	if key == "" {
		key = "_"
	}
	l.mu.Lock()
	b := l.ips[key]
	if b == nil {
		b = &bucket{}
		l.ips[key] = b
	}
	l.mu.Unlock()
	return b.take(l.rps, l.burst, now)
}

func (l *Limiter) gc() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			cut := time.Now().Add(-10 * time.Minute)
			l.mu.Lock()
			for k, b := range l.ips {
				b.mu.Lock()
				idle := b.last.Before(cut)
				b.mu.Unlock()
				if idle {
					delete(l.ips, k)
				}
			}
			l.mu.Unlock()
		}
	}
}

func (l *Limiter) Stop() {
	l.once.Do(func() { close(l.stop) })
}

// ---------- Basic Auth ----------

func BasicAuthOK(r *http.Request, user, pass string) bool {
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 &&
		subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
}

// ---------- Host 匹配 ----------

// hostMatch 支持精确匹配与 *.example.com 通配
func hostMatch(pattern, host string) bool {
	if pattern == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	pattern = strings.ToLower(pattern)
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return host == pattern
}

// ---------- 常用判定 ----------

func IsUpgrade(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return true
	}
	for _, v := range r.Header.Values("Connection") {
		if strings.Contains(strings.ToLower(v), "upgrade") {
			return true
		}
	}
	return false
}
