package proxy

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
)

const baseCSS = `
:root{--bg:#0b0f17;--card:#141a24;--line:#232c3b;--fg:#e6edf7;--dim:#8b98ad;--acc:#4f8cff;--ok:#29c26e;--warn:#ffb020;--err:#ff5f56}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px;
font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;
background:radial-gradient(1200px 600px at 50% -10%,#16203100,#0b0f17 60%),#0b0f17;color:var(--fg)}
.card{max-width:620px;width:100%;background:var(--card);border:1px solid var(--line);border-radius:16px;padding:32px;
box-shadow:0 20px 60px rgba(0,0,0,.45)}
h1{margin:0 0 6px;font-size:22px;letter-spacing:.3px}
.badge{display:inline-block;padding:2px 10px;border-radius:999px;font-size:12px;border:1px solid var(--line);color:var(--dim);margin-bottom:14px}
p{margin:10px 0;line-height:1.7;color:#c3ccdb}
.dim{color:var(--dim);font-size:13px}
code{background:#0e141d;border:1px solid var(--line);border-radius:6px;padding:1px 6px;font-size:12px}
.actions{margin-top:22px;display:flex;gap:10px;flex-wrap:wrap}
a.btn{display:inline-block;padding:9px 16px;border-radius:10px;text-decoration:none;font-size:14px;
background:var(--acc);color:#fff;border:1px solid transparent}
a.btn.ghost{background:transparent;border-color:var(--line);color:var(--fg)}
hr{border:none;border-top:1px solid var(--line);margin:22px 0}
ul{margin:8px 0 0;padding-left:20px;color:var(--dim);font-size:13px;line-height:1.9}
`

func page(title, detail, extra string) string {
	var b strings.Builder
	b.WriteString("<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">")
	b.WriteString("<title>" + html.EscapeString(title) + "</title><style>" + baseCSS + "</style></head><body>")
	b.WriteString("<div class=\"card\">")
	b.WriteString("<span class=\"badge\">revproxy</span>")
	b.WriteString("<h1>" + html.EscapeString(title) + "</h1>")
	if detail != "" {
		b.WriteString("<p>" + html.EscapeString(detail) + "</p>")
	}
	if extra != "" {
		b.WriteString(extra)
	}
	b.WriteString("</div></body></html>")
	return b.String()
}

// writeDefaultPage 未匹配到任何路由时的兜底页
func (e *Engine) writeDefaultPage(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	cfg := e.currentCfg()
	adminPath := "/__admin"
	enabled, total := 0, 0
	if cfg != nil {
		adminPath = cfg.Listen.AdminPath
		total = len(cfg.Routes)
		for _, rt := range cfg.Routes {
			if rt.Enabled {
				enabled++
			}
		}
	}
	extra := "<hr><p class=\"dim\">当前共配置 <b>" + fmt.Sprint(total) + "</b> 条路由，其中 <b>" +
		fmt.Sprint(enabled) + "</b> 条已启用。</p>"
	extra += "<div class=\"actions\">"
	extra += "<a class=\"btn\" href=\"" + html.EscapeString(adminPath) + "/\">进入管理后台</a>"
	extra += "</div>"
	extra += "<hr><ul>"
	extra += "<li>路由按「Host + 路径前缀」匹配，优先级高、路径长的规则先命中</li>"
	extra += "<li>本请求：<code>" + html.EscapeString(r.Host) + html.EscapeString(r.URL.Path) + "</code></li>"
	extra += "</ul>"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(page(title, detail, extra)))
}

func (e *Engine) writeError(w http.ResponseWriter, code int, title, detail string) {
	extra := "<hr><p class=\"dim\">HTTP " + fmt.Sprint(code) + " · revproxy</p>"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(page(title, detail, extra)))
}

// Echo 内置回显服务，用于自测反代是否连通（/__echo）
func EchoHandler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		body := readBodySample(r)
		resp := fmt.Sprintf(`{
  "service": "revproxy-echo",
  "method": %q,
  "host": %q,
  "path": %q,
  "query": %q,
  "remoteAddr": %q,
  "headers": %s,
  "body": %q,
  "time": %q
}`, r.Method, r.Host, r.URL.Path, r.URL.RawQuery, r.RemoteAddr, headerJSON(r), trunc(body, 512), nowStamp())
		_, _ = w.Write([]byte(resp))
	})
}

func headerJSON(r *http.Request) string {
	var b strings.Builder
	b.WriteString("{")
	first := true
	for k, v := range r.Header {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(fmt.Sprintf("%q:%q", k, strings.Join(v, " | ")))
	}
	b.WriteString("}")
	return b.String()
}

func readBodySample(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf := make([]byte, 4096)
	n, _ := r.Body.Read(buf)
	return string(buf[:n])
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func nowStamp() string {
	return time.Now().Format(time.RFC3339)
}
