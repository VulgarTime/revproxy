package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"revproxy/internal/config"
	"revproxy/internal/socks"
	"revproxy/internal/stats"
)

type ctxKey int

const (
	ctxKeyBackend ctxKey = iota
	ctxKeyRoute
	ctxKeyIP
	ctxKeyStart
)

func backendOf(r *http.Request) *Backend {
	if b, ok := r.Context().Value(ctxKeyBackend).(*Backend); ok {
		return b
	}
	return nil
}

func setBackend(r *http.Request, b *Backend) {
	*r = *r.WithContext(context.WithValue(r.Context(), ctxKeyBackend, b))
}

// ---------- 路由处理器 ----------

type RouteHandler struct {
	cfg       config.Route
	pool      *Pool
	rp        *httputil.ReverseProxy
	allow     *IPMatcher
	deny      *IPMatcher
	limiter   *Limiter
	pathRe    *regexp.Regexp
	engine    *Engine
	passFail  int
	dynDomain bool   // 域名内嵌路径（万能反代）模式
	prefix    string // 路由路径前缀（domain_in_path 模式下需扣除）
}

func (h *RouteHandler) match(r *http.Request) bool {
	if !h.cfg.Enabled {
		return false
	}
	if !hostMatch(h.cfg.Host, r.Host) {
		return false
	}
	p := r.URL.Path
	switch h.cfg.Match {
	case "exact":
		return p == h.cfg.Path
	case "regex":
		return h.pathRe != nil && h.pathRe.MatchString(p)
	default: // prefix
		if h.cfg.Path == "/" {
			return true
		}
		return strings.HasPrefix(p, h.cfg.Path)
	}
}

// applyOutURL 计算并发往上游的最终 URL
func (h *RouteHandler) applyOutURL(out *http.Request, b *Backend) {
	orig := out.URL.Path
	path := orig
	switch {
	case h.cfg.Rewrite != "":
		rw := strings.TrimRight(h.cfg.Rewrite, "/")
		rest := strings.TrimPrefix(orig, h.cfg.Path)
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
		path = rw + rest
		if path == "" {
			path = "/"
		}
	case h.cfg.StripPath:
		path = strings.TrimPrefix(orig, h.cfg.Path)
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		if path == "" {
			path = "/"
		}
	}
	base := strings.TrimRight(b.URL.Path, "/")
	out.URL.Scheme = b.URL.Scheme
	out.URL.Host = b.URL.Host
	out.URL.Path = base + path
	out.URL.RawPath = ""
}

// dynDomainOf 从路径中解析目标域名（域名内嵌路径模式）。
// 例如 /b.com/sd.txt → "b.com"，/a.com:8080/x → "a.com:8080"。无有效域名返回 ""。
// routePathInner 去除路由的路径前缀，返回前缀之后的真实子路径。
// 例：prefix="/p"、path="/p/a.com/x" → "/a.com/x"；prefix="/" 或 "" → 原样。
func routePathInner(fullPath, prefix string) string {
	if prefix == "" || prefix == "/" {
		return fullPath
	}
	rest := strings.TrimPrefix(fullPath, prefix)
	if rest == "" {
		return "/"
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return rest
}

func (h *RouteHandler) dynDomainOf(r *http.Request) string {
	p := routePathInner(r.URL.Path, h.prefix)
	raw := strings.TrimPrefix(p, "/")
	if raw == "" {
		return ""
	}
	domain := raw
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		domain = raw[:i]
	}
	if domain == "" || domain[0] == '.' || strings.ContainsAny(domain, " \t\r\n") {
		return ""
	}
	return domain
}

// resolveDynProxy 按域名决定走哪条代理：先匹配 DomainRules，未命中则用路由级默认代理
func (h *RouteHandler) resolveDynProxy(dom string) (*socks.Config, string) {
	resolve := resolveProxyFn(h.engine.currentCfg())
	for _, rule := range h.cfg.DomainRules {
		if hostMatch(rule.Match, dom) {
			return resolve(rule.ProxyID)
		}
	}
	return resolve(h.cfg.ProxyID)
}

// dynSchemeOf 万能反代模式下按域名解析目标协议：命中域名规则且该规则显式指定了 scheme 时用规则值，
// 否则回退到路由级默认协议（h.cfg.Scheme，非 https 一律当 http）。这样兜底设 http 时，
// 仍可单独让 raw.githubusercontent.com 这类站点走 https，解决「有些 http 有些 https」的冲突。
func (h *RouteHandler) dynSchemeOf(dom string) string {
	for _, rule := range h.cfg.DomainRules {
		if rule.Scheme != "" && hostMatch(rule.Match, dom) {
			return rule.Scheme
		}
	}
	if h.cfg.Scheme == "https" {
		return "https"
	}
	return "http"
}

// rewriteDynamic 域名内嵌路径模式：把 /<域名>/<路径> 还原为 <协议>://<域名>/<路径>
func (h *RouteHandler) rewriteDynamic(pr *httputil.ProxyRequest) {
	in := pr.In
	dom := h.dynDomainOf(in)
	scheme := h.dynSchemeOf(dom)
	inner := routePathInner(in.URL.Path, h.prefix) // 例：/a.com/sd.txt
	rest := strings.TrimPrefix(inner, "/")         // a.com/sd.txt
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[i:] // /sd.txt
	} else {
		rest = ""
	}
	out := pr.Out
	out.URL.Scheme = scheme
	out.URL.Host = dom
	out.URL.Path = rest
	if out.URL.Path == "" {
		out.URL.Path = "/"
	}
	out.URL.RawPath = ""
	out.URL.RawQuery = in.URL.RawQuery
	out.Host = dom // 让上游看到正确的 Host
	pr.SetXForwarded()
	for _, kv := range h.cfg.Headers.Request {
		if kv.Name != "" {
			out.Header.Set(kv.Name, kv.Value)
		}
	}
	for _, name := range h.cfg.Headers.Remove {
		out.Header.Del(name)
	}
}

// roundTripDynamic 域名内嵌路径模式的实际转发：按目标域名解析出口代理后发送。
// 注意：此时 req 已被 rewriteDynamic 改写，域名已填入 req.URL.Host（如 b.com:9022），
// 因此直接用 req.URL.Host 匹配域名规则，切勿再调 dynDomainOf（会误解析剩余路径）。
func (h *RouteHandler) roundTripDynamic(req *http.Request) (*http.Response, error) {
	e := h.engine
	dom := req.URL.Host
	if dom == "" {
		dom = req.Host
	}
	px, viaName := h.resolveDynProxy(dom)
	respTO := h.cfg.Timeout
	if respTO <= 0 {
		respTO = e.timeout()
	}
	tr := e.tm.Get(TransportOpts{
		Proxy:      px,
		Insecure:   h.cfg.InsecureTLS,
		DialTO:     e.dialTimeout(),
		RespTO:     respTO,
		KeepAlive:  e.currentCfg().Global.KeepAlive,
		MaxIdle:    e.currentCfg().Global.MaxIdleConns,
		MaxPerHost: e.currentCfg().Global.MaxConnsPerHost,
	})
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("访问 %s 失败（经由 %s）: %w", dom, viaName, err)
	}
	resp.Body = &countBody{rc: resp.Body, done: func() {}}
	return resp, nil
}

func (h *RouteHandler) rewrite(pr *httputil.ProxyRequest) {
	if h.dynDomain {
		h.rewriteDynamic(pr)
		return
	}
	b := backendOf(pr.Out)
	if b == nil {
		return
	}
	pr.SetXForwarded()
	out := pr.Out
	h.applyOutURL(out, b)
	if h.cfg.PreserveHost {
		out.Host = pr.In.Host
	} else {
		out.Host = ""
	}
	for _, kv := range h.cfg.Headers.Request {
		if kv.Name != "" {
			out.Header.Set(kv.Name, kv.Value)
		}
	}
	for _, name := range h.cfg.Headers.Remove {
		out.Header.Del(name)
	}
}

func (h *RouteHandler) modifyResponse(resp *http.Response) error {
	for _, kv := range h.cfg.Headers.Response {
		if kv.Name != "" {
			resp.Header.Set(kv.Name, kv.Value)
		}
	}
	if resp.Header.Get("X-Revproxy-Route") == "" {
		resp.Header.Set("X-Revproxy-Route", h.cfg.ID)
	}
	return nil
}

// errorPage 上游不可用时的响应
func (h *RouteHandler) errorPage(w http.ResponseWriter, r *http.Request, err error) {
	up := ""
	via := ""
	if h.dynDomain {
		// 万能反代模式：反向代理已把真实目标域名写入 r.URL.Host，按域名解析到的出口代理名展示，
		// 避免误导用户看到 __dynamic__ / 直连。
		dom := r.URL.Host
		if dom == "" {
			dom = r.Host
		}
		up = dom
		if _, viaName := h.resolveDynProxy(dom); viaName != "" {
			via = viaName
		} else {
			via = "直连"
		}
	} else if b := backendOf(r); b != nil {
		up = b.Raw
		via = b.ProxyName
	}
	status := http.StatusBadGateway
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "timeout"):
		status = http.StatusGatewayTimeout
	case strings.Contains(msg, "no such host"):
		status = http.StatusBadGateway
	case strings.Contains(msg, "connection refused"):
		status = http.StatusBadGateway
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Revproxy-Error", truncate(msg, 200))
	w.WriteHeader(status)
	body := fmt.Sprintf(`{"error":"bad gateway","route":%q,"upstream":%q,"viaProxy":%q,"message":%q,"time":%q}`,
		h.cfg.Name, up, via, msg, time.Now().Format(time.RFC3339))
	_, _ = w.Write([]byte(body))
}

// ---------- 带重试的 RoundTripper ----------

type routeRT struct {
	h *RouteHandler
}

func (rt *routeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	h := rt.h
	e := h.engine
	if h.dynDomain {
		return h.roundTripDynamic(req)
	}
	ip, _ := req.Context().Value(ctxKeyIP).(string)
	b := backendOf(req)
	if b == nil {
		return nil, errNoBackend
	}
	// 有请求体时无法安全重放，禁用重试
	attempts := 1 + max(0, h.cfg.Retry)
	if req.Body != nil && req.ContentLength != 0 {
		attempts = 1
	}
	// 内置自测上游：不经过网络，直接回显请求，用于验证反代链路本身
	if b.URL.Scheme == "internal" {
		b.markSuccess()
		return internalEcho(req), nil
	}
	exclude := map[*Backend]bool{}
	var lastErr error
	threshold := 3
	if h.cfg.Health.Enabled && h.cfg.Health.Fails > 0 {
		threshold = h.cfg.Health.Fails
	}
	respTO := h.cfg.Timeout
	if respTO <= 0 {
		respTO = e.timeout()
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			nb := h.pool.Next(ip, exclude)
			if nb == nil {
				break
			}
			b = nb
			setBackend(req, b)
			h.applyOutURL(req, b)
		}
		tr := e.tm.Get(TransportOpts{
			Proxy:      b.Proxy,
			Insecure:   h.cfg.InsecureTLS,
			DialTO:     e.dialTimeout(),
			RespTO:     respTO,
			KeepAlive:  e.currentCfg().Global.KeepAlive,
			MaxIdle:    e.currentCfg().Global.MaxIdleConns,
			MaxPerHost: e.currentCfg().Global.MaxConnsPerHost,
		})
		atomic.AddInt64(&b.conns, 1)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			atomic.AddInt64(&b.conns, -1)
			b.markFail(err, threshold)
			lastErr = err
			exclude[b] = true
			continue
		}
		b.markSuccess()
		resp.Body = &countBody{rc: resp.Body, done: func() {
			atomic.AddInt64(&b.conns, -1)
		}}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errNoBackend
	}
	return nil, lastErr
}

var errNoBackend = fmt.Errorf("没有可用的上游节点")

// countBody 统计在途连接数。
// 必须实现 io.ReadWriteCloser：WebSocket 等 101 升级响应要求 resp.Body 可写，
// 否则 ReverseProxy 无法 hijack 双向拷贝。
type countBody struct {
	rc   io.ReadCloser
	once sync.Once
	done func()
}

func (c *countBody) Read(p []byte) (int, error) { return c.rc.Read(p) }

func (c *countBody) Write(p []byte) (int, error) {
	if w, ok := c.rc.(io.Writer); ok {
		return w.Write(p)
	}
	return 0, fmt.Errorf("上游响应体不可写，无法完成协议升级")
}

func (c *countBody) Close() error {
	err := c.rc.Close()
	c.once.Do(c.done)
	return err
}

// ---------- 响应记录器 ----------

type statusRecorder struct {
	http.ResponseWriter
	status   int
	written  int64
	hijacked bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := s.ResponseWriter.(http.Hijacker); ok {
		s.hijacked = true
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("当前连接不支持 hijack")
}

// ---------- Engine ----------

type Engine struct {
	store  *config.Store
	stats  *stats.Collector
	tm     *TransportManager
	hc     *HealthChecker
	logger *log.Logger
	mu     sync.RWMutex
	routes []*RouteHandler
	cfg    *config.Config
	start  time.Time
}

func NewEngine(store *config.Store, st *stats.Collector, logger *log.Logger) *Engine {
	e := &Engine{
		store:  store,
		stats:  st,
		tm:     NewTransportManager(),
		logger: logger,
		start:  time.Now(),
	}
	e.hc = NewHealthChecker(e.tm)
	if err := e.Reload(store.Get()); err != nil {
		logger.Printf("[warn] 初始加载路由失败: %v", err)
	}
	return e
}

func (e *Engine) currentCfg() *config.Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

func (e *Engine) dialTimeout() int {
	c := e.currentCfg()
	if c != nil && c.Global.DialTimeout > 0 {
		return c.Global.DialTimeout
	}
	return 10
}

func (e *Engine) timeout() int {
	c := e.currentCfg()
	if c != nil {
		return c.Global.ResponseTimeout
	}
	return 0
}

// Reload 热重载路由表（不中断现有连接）
func (e *Engine) Reload(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("配置为空")
	}
	resolve := resolveProxyFn(cfg)
	handlers := make([]*RouteHandler, 0, len(cfg.Routes))
	targets := make([]HealthTarget, 0, len(cfg.Routes))
	var errs []string

	for _, rc := range cfg.Routes {
		if len(rc.Backends) == 0 && rc.Mode != "domain_in_path" {
			errs = append(errs, fmt.Sprintf("路由 %q 没有配置后端", rc.Name))
			continue
		}
		pool := make([]*Backend, 0, len(rc.Backends))
		var berrs []string
		for _, b := range rc.Backends {
			// 节点未单独指定代理时，继承路由级代理；路由级也为空则走全局默认
			pid := strings.TrimSpace(b.ProxyID)
			if pid == "" {
				pid = rc.ProxyID
			}
			bb, err := newBackend(b, func(string) (*socks.Config, string) {
				return resolve(pid)
			})
			if err != nil {
				berrs = append(berrs, err.Error())
				continue
			}
			pool = append(pool, bb)
		}
		// 域名内嵌路径（万能反代）模式：出口代理按域名动态选择，无需固定后端
		if len(pool) == 0 && rc.Mode == "domain_in_path" {
			bb, err := newBackend(config.Backend{URL: "http://__dynamic__/"}, func(string) (*socks.Config, string) {
				return resolve(rc.ProxyID)
			})
			if err != nil {
				berrs = append(berrs, err.Error())
			} else {
				pool = append(pool, bb)
			}
		}
		if len(pool) == 0 {
			errs = append(errs, strings.Join(berrs, "; "))
			continue
		}
		p := NewPool(rc.LB, pool)
		h := &RouteHandler{
			cfg:       rc,
			pool:      p,
			allow:     NewIPMatcher(rc.Access.IPAllow),
			deny:      NewIPMatcher(rc.Access.IPDeny),
			engine:    e,
			passFail:  rc.Health.Fails,
			dynDomain: rc.Mode == "domain_in_path",
			prefix:    rc.Path,
		}
		if rc.Match == "regex" {
			if re, err := regexp.Compile(rc.Path); err == nil {
				h.pathRe = re
			} else {
				errs = append(errs, fmt.Sprintf("路由 %q 正则非法: %v", rc.Name, err))
			}
		}
		if rc.Access.Rate.Enabled {
			h.limiter = NewLimiter(rc.Access.Rate)
		}
		h.rp = &httputil.ReverseProxy{
			Transport:      &routeRT{h: h},
			Rewrite:        h.rewrite,
			ModifyResponse: h.modifyResponse,
			FlushInterval:  100 * time.Millisecond, // 支持 SSE / 流式响应
			ErrorLog:       e.logger,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				h.errorPage(w, r, err)
			},
		}
		handlers = append(handlers, h)
		if rc.Health.Enabled && !h.dynDomain {
			targets = append(targets, HealthTarget{
				RouteID:  rc.ID,
				Route:    rc.Name,
				Pool:     p,
				Cfg:      rc.Health,
				Insecure: rc.InsecureTLS,
			})
		}
	}

	e.mu.Lock()
	old := e.routes
	e.routes = handlers
	e.cfg = cfg
	e.mu.Unlock()

	// 回收旧资源
	for _, h := range old {
		if h.limiter != nil {
			h.limiter.Stop()
		}
	}
	e.hc.Reset(targets)
	e.tm.CloseIdle()

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// States 返回所有后端节点的实时状态，供管理后台展示
func (e *Engine) States() []map[string]interface{} {
	e.mu.RLock()
	routes := e.routes
	e.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(routes))
	for _, h := range routes {
		bs := make([]map[string]interface{}, 0, len(h.pool.All()))
		for _, b := range h.pool.All() {
			bs = append(bs, b.State())
		}
		out = append(out, map[string]interface{}{
			"id":       h.cfg.ID,
			"name":     h.cfg.Name,
			"enabled":  h.cfg.Enabled,
			"host":     h.cfg.Host,
			"path":     h.cfg.Path,
			"backends": bs,
		})
	}
	return out
}

func (e *Engine) Routes() []config.Route {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]config.Route, 0, len(e.routes))
	for _, h := range e.routes {
		out = append(out, h.cfg)
	}
	return out
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.stats.Begin()
	start := time.Now()
	cfg := e.currentCfg()
	ip := ClientIP(r, cfg != nil && cfg.Global.TrustProxy)

	h := e.match(r)
	var (
		status  int
		written int64
		errMsg  string
		up      string
		via     string
		routeID = "_nomatch"
		name    = "-"
	)

	if h == nil {
		status = http.StatusNotFound
		name = "-"
		e.writeDefaultPage(w, r, status, "404 — 没有匹配到任何路由", "请求没有命中任何已启用的路由规则。")
	} else {
		routeID = h.cfg.ID
		name = h.cfg.Name
		if h.dynDomain {
			dom := h.dynDomainOf(r)
			if dom == "" {
				status = http.StatusBadRequest
				e.writeError(w, status, "400 Bad Request",
					"「域名内嵌路径」模式要求路径形如 /目标域名/路径，例如 /example.com/index.html")
				return
			}
			_, via = h.resolveDynProxy(dom)
			up = h.cfg.Scheme
			if up != "https" {
				up = "http"
			}
			up += "://" + dom
		}
		// ---- 访问控制 ----
		if h.deny.Match(ip) || (h.allow != nil && !h.allow.Empty() && !h.allow.Match(ip)) {
			status = http.StatusForbidden
			e.writeError(w, status, "403 Forbidden", "你的 IP ("+ip+") 不在允许范围内。")
		} else if h.cfg.Access.BasicAuth.Enabled && !BasicAuthOK(r, h.cfg.Access.BasicAuth.Username, h.cfg.Access.BasicAuth.Password) {
			realm := h.cfg.Access.BasicAuth.Realm
			if realm == "" {
				realm = "Restricted"
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
			status = http.StatusUnauthorized
			e.writeError(w, status, "401 Unauthorized", "该路由启用了 Basic 认证。")
		} else if h.limiter != nil && !h.limiter.Allow(ip) {
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", "1")
			e.writeError(w, status, "429 Too Many Requests", "请求过于频繁，请稍后再试。")
		} else {
			b := h.pool.Next(ip, nil)
			if b == nil {
				status = http.StatusBadGateway
				e.writeError(w, status, "502 Bad Gateway", "路由「"+h.cfg.Name+"」的所有上游节点都不可用。")
				errMsg = "所有上游节点不可用"
			} else {
				// 万能反代模式下 up/via 已按真实目标域名算好，别被合成占位节点覆盖
				if !h.dynDomain {
					up = b.Raw
					via = b.ProxyName
				}
				ctx := context.WithValue(r.Context(), ctxKeyBackend, b)
				ctx = context.WithValue(ctx, ctxKeyIP, ip)
				ctx = context.WithValue(ctx, ctxKeyStart, start)
				r = r.WithContext(ctx)

				rec := &statusRecorder{ResponseWriter: w, status: 0}
				timeout := h.cfg.Timeout
				if timeout <= 0 {
					timeout = e.timeout()
				}
				if timeout > 0 && !IsUpgrade(r) {
					cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
					defer cancel()
					r = r.WithContext(cctx)
				}
				h.rp.ServeHTTP(rec, r)
				status = rec.status
				written = rec.written
				if status == 0 {
					status = http.StatusOK
				}
				if status >= 500 {
					errMsg = http.StatusText(status)
				}
			}
		}
	}

	if cfg == nil || cfg.Log.Access {
		logEntry := stats.AccessLog{
			Time:     start,
			RouteID:  routeID,
			Route:    name,
			Method:   r.Method,
			Host:     r.Host,
			Path:     r.URL.RequestURI(),
			Status:   status,
			Duration: time.Since(start).Milliseconds(),
			BytesOut: written,
			ClientIP: ip,
			Upstream: up,
			ViaProxy: via,
			Error:    errMsg,
		}
		e.stats.End(logEntry)
		if cfg == nil || strings.EqualFold(cfg.Log.Level, "debug") {
			e.logger.Printf("[access] %s %s%s -> %d %dms via=%s up=%s ip=%s",
				r.Method, r.Host, r.URL.RequestURI(), status, logEntry.Duration, via, up, ip)
		}
	}
}

// Stop 停止健康检查并释放连接
func (e *Engine) Stop() {
	e.hc.Stop()
	e.tm.CloseIdle()
}

// TestUpstream 探测某个后端是否可达（可指定走哪条代理），供管理后台「连通性测试」使用
func (e *Engine) TestUpstream(backendURL, proxyID, path, method string, insecure bool) (map[string]interface{}, error) {
	u, err := url.Parse(backendURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "internal") {
		return nil, fmt.Errorf("后端地址非法，应为 http(s)://host:port 或 internal://echo，当前为 %q", backendURL)
	}
	if u.Scheme == "internal" {
		return map[string]interface{}{
			"status": 200, "elapsed": "0s", "via": "内置上游", "url": "internal://echo",
			"server": "revproxy-internal",
		}, nil
	}
	cfg := e.currentCfg()
	if cfg == nil {
		return nil, fmt.Errorf("配置尚未加载")
	}
	px, name := resolveProxyFn(cfg)(proxyID)
	if path == "" {
		path = "/"
	}
	if method == "" {
		method = "GET"
	}
	full := *u
	full.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(path, "/")
	full.RawQuery = ""
	tr := e.tm.Get(TransportOpts{Proxy: px, Insecure: insecure, DialTO: 10, RespTO: 15, MaxIdle: 2})
	req, err := http.NewRequest(method, full.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "revproxy-probe")
	req.Close = true
	start := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return map[string]interface{}{
		"status":  resp.StatusCode,
		"elapsed": time.Since(start).Round(time.Millisecond).String(),
		"via":     name,
		"url":     full.String(),
		"server":  resp.Header.Get("Server"),
	}, nil
}

func (e *Engine) match(r *http.Request) *RouteHandler {
	e.mu.RLock()
	routes := e.routes
	e.mu.RUnlock()
	// 与 nginx 一致：多个路由都能匹配时，选「最具体」的那条。
	// 优先级：精确匹配 > 更长的前缀 > 正则(权重最低) > 同权重取靠前的。
	// 这样具体的别名路由（如 /gh）会正确压过通用的兜底路由（如 /）。
	var best *RouteHandler
	bestScore := -1
	for _, h := range routes {
		if !h.match(r) {
			continue
		}
		score := 0
		switch h.cfg.Match {
		case "exact":
			if r.URL.Path == h.cfg.Path {
				score = 1 << 30
			} else {
				continue
			}
		case "regex":
			score = 0
		default: // prefix
			if h.cfg.Path == "/" {
				score = 1
			} else {
				score = len(h.cfg.Path)
			}
		}
		if score > bestScore {
			best = h
			bestScore = score
		}
	}
	return best
}

// ---------- 工具 ----------

func resolveProxyFn(cfg *config.Config) func(id string) (*socks.Config, string) {
	return func(id string) (*socks.Config, string) {
		if id == "__direct__" {
			return nil, "直连"
		}
		if id == "" {
			id = cfg.Global.DefaultProxy
		}
		if id == "" || id == "__direct__" {
			return nil, "直连"
		}
		if p := cfg.ProxyByID(id); p != nil {
			sc := &socks.Config{
				Type:     socks.ProxyType(p.Type),
				Addr:     p.Addr,
				Username: p.Username,
				Password: p.Password,
				Timeout:  p.Timeout,
			}
			if sc.Empty() {
				return nil, "直连"
			}
			return sc, p.Name
		}
		return nil, "直连(代理ID不存在)"
	}
}

func truncate(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// internalEcho 内置自测上游的响应构造
func internalEcho(req *http.Request) *http.Response {
	host := req.Header.Get("X-Forwarded-Host") // 反代改写后的 Host 存在这里
	if host == "" {
		host = req.Host
	}
	body := fmt.Sprintf(`{
  "service": "revproxy-internal-echo",
  "message": "反代链路正常：请求已到达 revproxy",
  "method": %q,
  "host": %q,
  "path": %q,
  "query": %q,
  "headers": %s,
  "time": %q
}`, req.Method, host, req.URL.Path, req.URL.RawQuery, headerJSON(req), time.Now().Format(time.RFC3339))
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json; charset=utf-8")
	hdr.Set("X-Revproxy-Upstream", "internal://echo")
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        hdr,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
