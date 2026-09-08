// Package admin 提供反向代理的 Web 管理后台（REST API + 静态页面）。
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"revproxy/internal/config"
	"revproxy/internal/proxy"
	"revproxy/internal/socks"
	"revproxy/internal/stats"
)

type Deps struct {
	Store   *config.Store
	Engine  *proxy.Engine
	Stats   *stats.Collector
	Logger  *log.Logger
	WebFS   fs.FS
	Prefix  string
	Version string
}

type Server struct {
	store   *config.Store
	engine  *proxy.Engine
	stats   *stats.Collector
	logger  *log.Logger
	auth    *Auth
	web     fs.FS
	prefix  string
	version string
	start   time.Time
	reqs    int64
}

func New(d Deps) *Server {
	if d.Prefix == "" {
		d.Prefix = "/__admin"
	}
	return &Server{
		store:   d.Store,
		engine:  d.Engine,
		stats:   d.Stats,
		logger:  d.Logger,
		auth:    NewAuth(d.Store),
		web:     d.WebFS,
		prefix:  strings.TrimRight(d.Prefix, "/"),
		version: d.Version,
		start:   time.Now(),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.reqs, 1)
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	if path == "" {
		http.Redirect(w, r, s.prefix+"/", http.StatusFound)
		return
	}
	if path == "/" {
		path = "/index.html"
	}
	switch {
	case strings.HasPrefix(path, "/api/"):
		s.api(w, r, strings.TrimPrefix(path, "/api/"))
	case path == "/index.html":
		s.serveFile(w, r, "index.html", "text/html; charset=utf-8")
	case path == "/app.js":
		s.serveFile(w, r, "app.js", "application/javascript; charset=utf-8")
	case path == "/style.css":
		s.serveFile(w, r, "style.css", "text/css; charset=utf-8")
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name, ct string) {
	if s.web == nil {
		http.Error(w, "前端资源未打包", http.StatusNotFound)
		return
	}
	f, err := s.web.Open(name)
	if err != nil {
		http.Error(w, "资源不存在: "+name, http.StatusNotFound)
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil {
		if mt, ok := f.(io.ReadSeeker); ok {
			_ = mt
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
			_, _ = io.Copy(w, f)
			return
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "读取资源失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// ---------- JSON 工具 ----------

func (s *Server) writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeOK(w http.ResponseWriter, v interface{}) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "data": v})
}

func (s *Server) writeErr(w http.ResponseWriter, code int, msg string) {
	s.writeJSON(w, code, map[string]interface{}{"ok": false, "error": msg})
}

func decodeJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v)
}

// ---------- 鉴权 ----------

func (s *Server) currentUser(r *http.Request) (string, bool) {
	if c, err := r.Cookie("rp_sid"); err == nil && c.Value != "" {
		if u, ok := s.auth.Verify(c.Value); ok {
			return u, true
		}
	}
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		if u, ok := s.auth.Verify(strings.TrimPrefix(ah, "Bearer ")); ok {
			return u, true
		}
	}
	return "", false
}

// csrfOK 简单同源校验：写操作必须带自定义头或 JSON 内容类型
func csrfOK(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if r.Header.Get("X-Revproxy-Admin") != "" {
		return true
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		return true
	}
	return false
}

func (s *Server) setCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     "rp_sid",
		Value:    token,
		Path:     s.prefix + "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ---------- API 分发 ----------

func (s *Server) api(w http.ResponseWriter, r *http.Request, seg string) {
	seg = strings.Trim(seg, "/")

	// 登录接口不需要鉴权
	if seg == "login" {
		if r.Method != http.MethodPost {
			s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		var in struct {
			User string `json:"user"`
			Pass string `json:"pass"`
		}
		if err := decodeJSON(r, &in); err != nil {
			s.writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		token, err := s.auth.Login(in.User, in.Pass)
		if err != nil {
			s.writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		s.setCookie(w, token, int(s.auth.ttl().Seconds()))
		s.writeOK(w, map[string]interface{}{"token": token, "user": in.User})
		return
	}

	user, ok := s.currentUser(r)
	if !ok {
		s.writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
		return
	}
	if !csrfOK(r) {
		s.writeErr(w, http.StatusForbidden, "缺少同源请求头")
		return
	}

	switch {
	case seg == "me":
		s.writeOK(w, map[string]interface{}{"user": user, "version": s.version})

	case seg == "logout":
		s.setCookie(w, "", -1)
		s.writeOK(w, nil)

	case seg == "overview":
		s.handleOverview(w, r)

	case seg == "config":
		s.handleConfig(w, r)

	case seg == "routes":
		s.handleRoutes(w, r)

	case strings.HasPrefix(seg, "routes/"):
		s.handleRouteByID(w, r, strings.TrimPrefix(seg, "routes/"))

	case seg == "proxies":
		s.handleProxies(w, r)

	case strings.HasPrefix(seg, "proxies/"):
		s.handleProxyByID(w, r, strings.TrimPrefix(seg, "proxies/"))

	case seg == "proxy-test":
		s.handleProxyTest(w, r)

	case seg == "upstream-test":
		s.handleUpstreamTest(w, r)

	case seg == "logs":
		s.handleLogs(w, r)

	case seg == "logs/clear":
		if r.Method != http.MethodPost {
			s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		s.stats.Clear()
		s.writeOK(w, nil)

	case seg == "password":
		s.handlePassword(w, r)

	case seg == "settings":
		s.handleSettings(w, r)

	case seg == "reload":
		if r.Method != http.MethodPost {
			s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		if err := s.store.Reload(); err != nil {
			s.writeErr(w, http.StatusBadRequest, "重新加载配置文件失败: "+err.Error())
			return
		}
		if err := s.engine.Reload(s.store.Get()); err != nil {
			s.writeErr(w, http.StatusBadRequest, "配置已加载，但部分路由有问题: "+err.Error())
			return
		}
		s.writeOK(w, map[string]interface{}{"message": "已热重载"})

	case seg == "export":
		cfg := s.store.Get()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=revproxy-config.json")
		_ = json.NewEncoder(w).Encode(cfg)

	case seg == "import":
		if r.Method != http.MethodPost {
			s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		var body struct {
			Config map[string]interface{} `json:"config"`
			Raw    map[string]interface{} `json:"raw"`
		}
		if err := decodeJSON(r, &body); err != nil {
			s.writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		src := body.Config
		if src == nil {
			src = body.Raw
		}
		if src == nil {
			s.writeErr(w, http.StatusBadRequest, "没有可导入的内容")
			return
		}
		raw, _ := json.Marshal(src)
		newCfg := &config.Config{}
		if err := json.Unmarshal(raw, newCfg); err != nil {
			s.writeErr(w, http.StatusBadRequest, "配置文件格式错误: "+err.Error())
			return
		}
		if err := s.store.Update(func(c *config.Config) error {
			*c = *newCfg
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.engine.Reload(s.store.Get()); err != nil {
			s.writeErr(w, http.StatusBadRequest, "已导入，但部分路由有问题: "+err.Error())
			return
		}
		s.writeOK(w, map[string]interface{}{"message": "配置已导入"})

	case seg == "system":
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		s.writeOK(w, map[string]interface{}{
			"goVersion":  runtime.Version(),
			"goroutines": runtime.NumGoroutine(),
			"memMB":      float64(ms.Sys-ms.HeapReleased) / 1024 / 1024,
			"heapMB":     float64(ms.HeapAlloc) / 1024 / 1024,
			"numCPU":     runtime.NumCPU(),
			"configPath": s.store.Path(),
			"uptime":     time.Since(s.start).String(),
			"adminReqs":  atomic.LoadInt64(&s.reqs),
		})

	default:
		s.writeErr(w, http.StatusNotFound, "未知接口: "+seg)
	}
}

// ---------- 各接口实现 ----------

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	snap := s.stats.Snapshot()
	enabled := 0
	for _, rt := range cfg.Routes {
		if rt.Enabled {
			enabled++
		}
	}
	// 汇总各路由的实时指标
	details := make([]map[string]interface{}, 0, len(cfg.Routes))
	for _, rt := range cfg.Routes {
		st := snap.Routes[rt.ID]
		avg := 0.0
		if st.Requests > 0 {
			avg = float64(st.TotalDur) / float64(st.Requests)
		}
		details = append(details, map[string]interface{}{
			"id": rt.ID, "name": rt.Name, "enabled": rt.Enabled,
			"host": rt.Host, "path": rt.Path, "lb": rt.LB,
			"proxyId": rt.ProxyID, "backends": len(rt.Backends),
			"reqs": st.Requests, "err5xx": st.Status5xx, "err4xx": st.Status4xx,
			"avgDur": avg, "maxDur": st.MaxDur, "bytes": st.BytesOut,
			"lastError": st.LastError,
		})
	}
	s.writeOK(w, map[string]interface{}{
		"stats":     snap,
		"routes":    details,
		"upstreams": s.engine.States(),
		"meta": map[string]interface{}{
			"version":      s.version,
			"goVersion":    runtime.Version(),
			"listen":       cfg.Listen.Addr,
			"adminPath":    cfg.Listen.AdminPath,
			"routeTotal":   len(cfg.Routes),
			"routeOn":      enabled,
			"proxyTotal":   len(cfg.Proxies),
			"defaultProxy": cfg.Global.DefaultProxy,
			"configPath":   s.store.Path(),
			"uptime":       snap.Uptime,
		},
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.store.Get()
		// 不暴露 secret 原文之外的敏感项：这里只隐藏 secret
		out := map[string]interface{}{}
		raw, _ := json.Marshal(cfg)
		_ = json.Unmarshal(raw, &out)
		delete(out, "admin")
		out["admin"] = map[string]interface{}{"username": cfg.Admin.Username, "sessionTTL": cfg.Admin.SessionTTL}
		s.writeOK(w, out)
	case http.MethodPut:
		var newCfg config.Config
		if err := decodeJSON(r, &newCfg); err != nil {
			s.writeErr(w, http.StatusBadRequest, "配置解析失败: "+err.Error())
			return
		}
		old := s.store.Get()
		newCfg.Admin.Secret = old.Admin.Secret
		if newCfg.Admin.PassHash == "" {
			newCfg.Admin.PassHash = old.Admin.PassHash
		}
		if err := s.store.Update(func(c *config.Config) error {
			*c = newCfg
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.engine.Reload(s.store.Get()); err != nil {
			s.writeErr(w, http.StatusBadRequest, "已保存，但部分路由有问题: "+err.Error())
			return
		}
		s.writeOK(w, map[string]interface{}{"message": "配置已保存并热重载"})
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET/PUT")
	}
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeOK(w, s.store.Get().Routes)
	case http.MethodPost:
		var rt config.Route
		if err := decodeJSON(r, &rt); err != nil {
			s.writeErr(w, http.StatusBadRequest, "路由解析失败: "+err.Error())
			return
		}
		rt.ID = config.NewID("rt")
		rt.CreatedAt = time.Now()
		rt.UpdatedAt = time.Now()
		if err := s.store.Update(func(c *config.Config) error {
			c.Routes = append(c.Routes, rt)
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.engine.Reload(s.store.Get()); err != nil {
			s.writeErr(w, http.StatusBadRequest, "已添加，但该路由有问题: "+err.Error())
			return
		}
		s.writeOK(w, rt)
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

func (s *Server) handleRouteByID(w http.ResponseWriter, r *http.Request, tail string) {
	parts := strings.Split(tail, "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch action {
	case "toggle":
		if r.Method != http.MethodPost {
			s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		var found bool
		if err := s.store.Update(func(c *config.Config) error {
			if rt := c.RouteByID(id); rt != nil {
				rt.Enabled = !rt.Enabled
				rt.UpdatedAt = time.Now()
				found = true
			}
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			s.writeErr(w, http.StatusNotFound, "路由不存在")
			return
		}
		_ = s.engine.Reload(s.store.Get())
		s.writeOK(w, nil)
		return
	case "duplicate":
		if r.Method != http.MethodPost {
			s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		var cp config.Route
		if err := s.store.Update(func(c *config.Config) error {
			rt := c.RouteByID(id)
			if rt == nil {
				return fmt.Errorf("路由不存在")
			}
			cp = *rt
			cp.ID = config.NewID("rt")
			cp.Name = rt.Name + " 副本"
			cp.CreatedAt = time.Now()
			cp.UpdatedAt = time.Now()
			c.Routes = append(c.Routes, cp)
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		_ = s.engine.Reload(s.store.Get())
		s.writeOK(w, cp)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if rt := s.store.Get().RouteByID(id); rt != nil {
			s.writeOK(w, rt)
			return
		}
		s.writeErr(w, http.StatusNotFound, "路由不存在")
	case http.MethodPut:
		var rt config.Route
		if err := decodeJSON(r, &rt); err != nil {
			s.writeErr(w, http.StatusBadRequest, "路由解析失败: "+err.Error())
			return
		}
		rt.ID = id
		rt.UpdatedAt = time.Now()
		var found bool
		if err := s.store.Update(func(c *config.Config) error {
			for i := range c.Routes {
				if c.Routes[i].ID == id {
					if rt.CreatedAt.IsZero() {
						rt.CreatedAt = c.Routes[i].CreatedAt
					}
					c.Routes[i] = rt
					found = true
					return nil
				}
			}
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			s.writeErr(w, http.StatusNotFound, "路由不存在")
			return
		}
		if err := s.engine.Reload(s.store.Get()); err != nil {
			s.writeErr(w, http.StatusBadRequest, "已保存，但该路由有问题: "+err.Error())
			return
		}
		s.writeOK(w, rt)
	case http.MethodDelete:
		var found bool
		if err := s.store.Update(func(c *config.Config) error {
			out := c.Routes[:0]
			for _, rt := range c.Routes {
				if rt.ID == id {
					found = true
					continue
				}
				out = append(out, rt)
			}
			c.Routes = out
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			s.writeErr(w, http.StatusNotFound, "路由不存在")
			return
		}
		_ = s.engine.Reload(s.store.Get())
		s.writeOK(w, nil)
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

func (s *Server) handleProxies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeOK(w, s.store.Get().Proxies)
	case http.MethodPost:
		var p config.Proxy
		if err := decodeJSON(r, &p); err != nil {
			s.writeErr(w, http.StatusBadRequest, "解析失败: "+err.Error())
			return
		}
		p.ID = config.NewID("px")
		if err := s.store.Update(func(c *config.Config) error {
			c.Proxies = append(c.Proxies, p)
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.engine.Reload(s.store.Get())
		s.writeOK(w, p)
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

func (s *Server) handleProxyByID(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodPut:
		var p config.Proxy
		if err := decodeJSON(r, &p); err != nil {
			s.writeErr(w, http.StatusBadRequest, "解析失败: "+err.Error())
			return
		}
		p.ID = id
		var found bool
		if err := s.store.Update(func(c *config.Config) error {
			for i := range c.Proxies {
				if c.Proxies[i].ID == id {
					c.Proxies[i] = p
					found = true
					return nil
				}
			}
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			s.writeErr(w, http.StatusNotFound, "代理不存在")
			return
		}
		_ = s.engine.Reload(s.store.Get())
		s.writeOK(w, p)
	case http.MethodDelete:
		var found bool
		if err := s.store.Update(func(c *config.Config) error {
			out := c.Proxies[:0]
			for _, p := range c.Proxies {
				if p.ID == id {
					found = true
					continue
				}
				out = append(out, p)
			}
			c.Proxies = out
			if c.Global.DefaultProxy == id {
				c.Global.DefaultProxy = ""
			}
			return nil
		}); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			s.writeErr(w, http.StatusNotFound, "代理不存在")
			return
		}
		_ = s.engine.Reload(s.store.Get())
		s.writeOK(w, nil)
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// handleProxyTest 测试 SOCKS5/HTTP 代理是否可用
func (s *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var in struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Addr     string `json:"addr"`
		Username string `json:"username"`
		Password string `json:"password"`
		Timeout  int    `json:"timeout"`
		Target   string `json:"target"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.writeErr(w, http.StatusBadRequest, "解析失败")
		return
	}
	cfg := s.store.Get()
	if in.ID != "" && in.Addr == "" {
		if p := cfg.ProxyByID(in.ID); p != nil {
			in.Type, in.Addr, in.Username, in.Password, in.Timeout = p.Type, p.Addr, p.Username, p.Password, p.Timeout
		}
	}
	if in.Target == "" {
		in.Target = "www.cloudflare.com:443"
	}
	sc := &socks.Config{
		Type:     socks.ProxyType(in.Type),
		Addr:     in.Addr,
		Username: in.Username,
		Password: in.Password,
		Timeout:  in.Timeout,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	start := time.Now()
	elapsed, err := socks.Test(ctx, sc, in.Target)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok": false, "error": err.Error(), "target": in.Target, "elapsed": time.Since(start).String(),
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "target": in.Target, "elapsed": elapsed.Round(time.Millisecond).String(),
		"via": string(sc.Type) + "://" + sc.Addr,
	})
}

// handleUpstreamTest 测试某个后端（可指定走哪条代理）是否可达
func (s *Server) handleUpstreamTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var in struct {
		URL      string `json:"url"`
		ProxyID  string `json:"proxyId"`
		Path     string `json:"path"`
		Insecure bool   `json:"insecure"`
		Method   string `json:"method"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.writeErr(w, http.StatusBadRequest, "解析失败")
		return
	}
	res, err := s.engine.TestUpstream(in.URL, in.ProxyID, in.Path, in.Method, in.Insecure)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "data": res})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	s.writeOK(w, s.stats.Logs(limit))
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var in struct {
		Old     string `json:"old"`
		New     string `json:"new"`
		NewUser string `json:"newUser"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.writeErr(w, http.StatusBadRequest, "解析失败")
		return
	}
	token, err := s.auth.ChangePassword(in.Old, in.New)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.NewUser != "" {
		_ = s.store.Update(func(c *config.Config) error {
			c.Admin.Username = in.NewUser
			return nil
		})
		token = s.auth.Token(in.NewUser)
	}
	s.setCookie(w, token, int(s.auth.ttl().Seconds()))
	s.writeOK(w, map[string]interface{}{"token": token, "message": "密码已更新"})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 PUT")
		return
	}
	var in struct {
		Listen config.Listen    `json:"listen"`
		Log    config.Log       `json:"log"`
		Global config.Global    `json:"global"`
		TLS    config.TLSConfig `json:"tls"`
		Admin  *struct {
			SessionTTL int `json:"sessionTTL"`
		} `json:"admin"`
		DefaultProxy *string `json:"defaultProxy"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.writeErr(w, http.StatusBadRequest, "解析失败: "+err.Error())
		return
	}
	if err := s.store.Update(func(c *config.Config) error {
		c.Listen = in.Listen
		c.Log = in.Log
		c.Global = in.Global
		c.TLS = in.TLS
		if in.Admin != nil && in.Admin.SessionTTL > 0 {
			c.Admin.SessionTTL = in.Admin.SessionTTL
		}
		return nil
	}); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.engine.Reload(s.store.Get()); err != nil {
		s.writeErr(w, http.StatusBadRequest, "已保存，但存在问题: "+err.Error())
		return
	}
	s.writeOK(w, map[string]interface{}{
		"message":     "设置已保存（监听地址变更需重启进程生效）",
		"needRestart": s.needRestart(&in.Listen),
	})
}

func (s *Server) needRestart(l *config.Listen) bool {
	cur := s.store.Get().Listen
	return cur.Addr != l.Addr || cur.AdminAddr != l.AdminAddr
}

// Prefix 返回管理后台路径前缀
func (s *Server) Prefix() string { return s.prefix }
