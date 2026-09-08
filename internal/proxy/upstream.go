package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"revproxy/internal/config"
	"revproxy/internal/socks"
)

// ---------- Transport 管理 ----------

type transportKey struct {
	proxy     string
	insecure  bool
	dialTO    int
	respTO    int
	keepAlive int
	idleConn  int
	maxHost   int
}

// TransportManager 按「代理 + TLS + 超时」维度复用 http.Transport，避免反复重建连接池
type TransportManager struct {
	mu    sync.Mutex
	cache map[transportKey]*http.Transport
}

func NewTransportManager() *TransportManager {
	return &TransportManager{cache: map[transportKey]*http.Transport{}}
}

type TransportOpts struct {
	Proxy      *socks.Config
	Insecure   bool
	DialTO     int // 秒
	RespTO     int // 秒，0=不限制
	KeepAlive  int // 秒
	MaxIdle    int
	MaxPerHost int
}

func (m *TransportManager) Get(o TransportOpts) *http.Transport {
	if o.DialTO <= 0 {
		o.DialTO = 10
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 30
	}
	if o.MaxIdle <= 0 {
		o.MaxIdle = 200
	}
	key := transportKey{
		proxy:     o.Proxy.Key(),
		insecure:  o.Insecure,
		dialTO:    o.DialTO,
		respTO:    o.RespTO,
		keepAlive: o.KeepAlive,
		idleConn:  o.MaxIdle,
		maxHost:   o.MaxPerHost,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if tr, ok := m.cache[key]; ok {
		return tr
	}
	px := o.Proxy
	tr := &http.Transport{
		Proxy: nil, // 不受环境变量干扰，全部由 DialContext 决定
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return socks.DialContext(ctx, px, network, addr)
		},
		MaxIdleConns:          o.MaxIdle,
		MaxIdleConnsPerHost:   max(2, o.MaxIdle/4),
		MaxConnsPerHost:       o.MaxPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   time.Duration(o.DialTO) * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     false, // 上游强制 HTTP/1.1，行为与 nginx 一致
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableCompression:    true,
	}
	if o.RespTO > 0 {
		tr.ResponseHeaderTimeout = time.Duration(o.RespTO) * time.Second
	}
	if o.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	m.cache[key] = tr
	return tr
}

// CloseIdle 配置变更后释放不再使用的空闲连接
func (m *TransportManager) CloseIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tr := range m.cache {
		tr.CloseIdleConnections()
	}
}

// ---------- 后端节点 ----------

type Backend struct {
	Raw       string
	URL       *url.URL
	Weight    int
	Proxy     *socks.Config
	ProxyName string
	BackendID string
	Backup    bool

	alive    int32 // 1=健康
	fails    int64
	okStreak int64
	conns    int64
	curW     int64 // 平滑加权轮询游标
	lastErr  string
	lastErrT time.Time
	mu       sync.Mutex
}

func newBackend(b config.Backend, resolve func(string) (*socks.Config, string)) (*Backend, error) {
	u, err := url.Parse(b.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("后端地址非法 %q", b.URL)
	}
	// internal://echo 是内置自测上游，不经过网络
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "internal" {
		return nil, fmt.Errorf("后端 %q 的协议必须是 http、https 或 internal", b.URL)
	}
	px, name := resolve(b.ProxyID)
	w := b.Weight
	if w <= 0 {
		w = 1
	}
	return &Backend{
		Raw:       b.URL,
		URL:       u,
		Weight:    w,
		Proxy:     px,
		ProxyName: name,
		Backup:    b.Backup,
		alive:     1,
	}, nil
}

func (b *Backend) Alive() bool { return atomic.LoadInt32(&b.alive) == 1 }

func (b *Backend) Conns() int64 { return atomic.LoadInt64(&b.conns) }

func (b *Backend) State() map[string]interface{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	errAt := ""
	if !b.lastErrT.IsZero() {
		errAt = b.lastErrT.Format("01-02 15:04:05")
	}
	return map[string]interface{}{
		"url":       b.Raw,
		"alive":     b.alive == 1,
		"conns":     atomic.LoadInt64(&b.conns),
		"fails":     b.fails,
		"weight":    b.Weight,
		"backup":    b.Backup,
		"proxy":     b.ProxyName,
		"lastErr":   b.lastErr,
		"lastErrAt": errAt,
	}
}

func (b *Backend) markFail(err error, threshold int) {
	b.mu.Lock()
	b.fails++
	b.okStreak = 0
	if err != nil {
		b.lastErr = err.Error()
		if len(b.lastErr) > 200 {
			b.lastErr = b.lastErr[:200]
		}
	}
	b.lastErrT = time.Now()
	if b.fails >= int64(max(1, threshold)) {
		atomic.StoreInt32(&b.alive, 0)
	}
	b.mu.Unlock()
}

func (b *Backend) markOK(passes int) {
	b.mu.Lock()
	b.okStreak++
	if b.okStreak >= int64(max(1, passes)) {
		b.fails = 0
		b.lastErr = ""
		atomic.StoreInt32(&b.alive, 1)
	}
	b.mu.Unlock()
}

// 被动健康检查：一次成功即恢复
func (b *Backend) markSuccess() {
	b.mu.Lock()
	b.fails = 0
	b.lastErr = ""
	atomic.StoreInt32(&b.alive, 1)
	b.mu.Unlock()
}

// ---------- 负载均衡池 ----------

type Pool struct {
	strategy string
	backends []*Backend
	mu       sync.Mutex
	rr       uint64
}

func NewPool(strategy string, bs []*Backend) *Pool {
	if strategy == "" {
		strategy = "round_robin"
	}
	return &Pool{strategy: strategy, backends: bs}
}

func (p *Pool) All() []*Backend { return p.backends }

// Next 按策略选一个健康后端；主节点全挂时才用 backup
func (p *Pool) Next(clientIP string, exclude map[*Backend]bool) *Backend {
	cands := make([]*Backend, 0, len(p.backends))
	for _, b := range p.backends {
		if exclude != nil && exclude[b] {
			continue
		}
		if b.Alive() && !b.Backup {
			cands = append(cands, b)
		}
	}
	if len(cands) == 0 {
		for _, b := range p.backends {
			if exclude != nil && exclude[b] {
				continue
			}
			if b.Alive() {
				cands = append(cands, b)
			}
		}
	}
	if len(cands) == 0 {
		return nil
	}
	switch p.strategy {
	case "random":
		return cands[int(time.Now().UnixNano())%len(cands)]
	case "least_conn":
		best := cands[0]
		bestScore := float64(best.Conns()) / float64(best.Weight)
		for _, b := range cands[1:] {
			if s := float64(b.Conns()) / float64(b.Weight); s < bestScore {
				best, bestScore = b, s
			}
		}
		return best
	case "ip_hash":
		return cands[int(fnv32(clientIP))%len(cands)]
	case "weighted":
		return p.smoothWeighted(cands)
	default: // round_robin
		return cands[int(atomic.AddUint64(&p.rr, 1))%len(cands)]
	}
}

// smoothWeighted nginx 同款平滑加权轮询
func (p *Pool) smoothWeighted(cands []*Backend) *Backend {
	p.mu.Lock()
	defer p.mu.Unlock()
	var total int64
	var best *Backend
	for _, b := range cands {
		b.curW += int64(b.Weight)
		total += int64(b.Weight)
		if best == nil || b.curW > best.curW {
			best = b
		}
	}
	if best == nil {
		return nil
	}
	best.curW -= total
	return best
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// ---------- 健康检查 ----------

type HealthTarget struct {
	RouteID  string
	Route    string
	Pool     *Pool
	Cfg      config.HealthCheck
	Insecure bool
}

type HealthChecker struct {
	mu   sync.Mutex
	stop chan struct{}
	wg   sync.WaitGroup
	tm   *TransportManager
}

func NewHealthChecker(tm *TransportManager) *HealthChecker {
	return &HealthChecker{tm: tm, stop: make(chan struct{})}
}

func (h *HealthChecker) Reset(targets []HealthTarget) {
	h.mu.Lock()
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
	h.wg.Wait()
	stop := make(chan struct{})
	h.stop = stop
	h.mu.Unlock()

	for _, t := range targets {
		if !t.Cfg.Enabled {
			continue
		}
		h.wg.Add(1)
		go func(t HealthTarget) {
			defer h.wg.Done()
			h.check(t)
			ticker := time.NewTicker(time.Duration(max(1, t.Cfg.Interval)) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					h.check(t)
				}
			}
		}(t)
	}
}

func (h *HealthChecker) check(t HealthTarget) {
	for _, b := range t.Pool.All() {
		if b.URL.Scheme == "internal" { // 内置上游永远健康
			b.markSuccess()
			continue
		}
		tr := h.tm.Get(TransportOpts{
			Proxy:    b.Proxy,
			Insecure: t.Insecure,
			DialTO:   max(1, t.Cfg.Timeout),
			RespTO:   max(1, t.Cfg.Timeout),
			MaxIdle:  4,
		})
		u := *b.URL
		if t.Cfg.Path != "" {
			u.Path = strings.TrimRight(b.URL.Path, "/") + "/" + strings.TrimLeft(t.Cfg.Path, "/")
		} else {
			u.Path = "/"
		}
		u.RawQuery = ""
		req, err := http.NewRequest("GET", u.String(), nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "revproxy-healthcheck")
		req.Close = true
		resp, err := tr.RoundTrip(req)
		if err != nil {
			b.markFail(err, t.Cfg.Fails)
			continue
		}
		resp.Body.Close()
		ok := resp.StatusCode < 400
		if len(t.Cfg.Codes) > 0 {
			ok = false
			for _, c := range t.Cfg.Codes {
				if c == resp.StatusCode {
					ok = true
					break
				}
			}
		}
		if ok {
			b.markOK(t.Cfg.Passes)
		} else {
			b.markFail(fmt.Errorf("健康检查返回状态码 %d", resp.StatusCode), t.Cfg.Fails)
		}
	}
}

func (h *HealthChecker) Stop() {
	h.mu.Lock()
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
	h.mu.Unlock()
	h.wg.Wait()
}
