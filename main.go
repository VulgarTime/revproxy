package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"revproxy/internal/admin"
	"revproxy/internal/config"
	"revproxy/internal/proxy"
	"revproxy/internal/stats"
)

// Version 运行时版本，可通过 -ldflags "-X main.Version=x.y.z" 覆盖
var Version = "1.0.0"

func main() {
	var (
		cfgPath   = flag.String("config", "", "配置文件路径（默认 ./data/config.json，可用环境变量 REVPROXY_CONFIG）")
		addr      = flag.String("addr", "", "代理监听地址，如 :8080（覆盖配置文件）")
		adminAddr = flag.String("admin-addr", "", "管理后台独立监听地址，如 :8081（为空则与代理共用端口）")
		dataDir   = flag.String("data", "", "数据目录（默认 ./data）")
		debug     = flag.Bool("debug", false, "开启 debug 日志（打印每条访问日志）")
		ver       = flag.Bool("version", false, "打印版本并退出")
	)
	flag.Parse()

	if *ver {
		fmt.Printf("revproxy %s\n", Version)
		return
	}

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	// ---- 定位配置文件 ----
	path := *cfgPath
	if path == "" {
		path = os.Getenv("REVPROXY_CONFIG")
	}
	if path == "" {
		dir := *dataDir
		if dir == "" {
			dir = os.Getenv("REVPROXY_DATA")
		}
		if dir == "" {
			dir = "data"
		}
		path = filepath.Join(dir, "config.json")
	}

	store, err := config.Load(path)
	if err != nil {
		logger.Fatalf("加载配置失败: %v", err)
	}
	cfg := store.Get()

	// 命令行 / 环境变量覆盖监听地址（云环境通常注入 PORT）
	if *addr != "" {
		if err := store.Update(func(c *config.Config) error {
			c.Listen.Addr = *addr
			return nil
		}); err != nil {
			logger.Printf("[warn] 更新监听地址失败: %v", err)
		}
	} else if p := os.Getenv("PORT"); p != "" {
		a := p
		if !strings.Contains(a, ":") {
			a = ":" + a
		}
		_ = store.Update(func(c *config.Config) error {
			c.Listen.Addr = a
			return nil
		})
	}
	if *adminAddr != "" {
		_ = store.Update(func(c *config.Config) error {
			c.Listen.AdminAddr = *adminAddr
			return nil
		})
	}
	if *debug {
		_ = store.Update(func(c *config.Config) error {
			c.Log.Level = "debug"
			return nil
		})
	}
	cfg = store.Get()

	st := stats.New(cfg.Log.Buffer, cfg.Log.File)
	engine := proxy.NewEngine(store, st, logger)

	// ---- 前端资源 ----
	var webFS fs.FS
	if sub, err := fs.Sub(embeddedWeb, "web"); err == nil {
		webFS = sub
	}

	adminSrv := admin.New(admin.Deps{
		Store:   store,
		Engine:  engine,
		Stats:   st,
		Logger:  logger,
		WebFS:   webFS,
		Prefix:  cfg.Listen.AdminPath,
		Version: Version,
	})

	// ---- 配置热重载 ----
	go func() {
		ch := store.Subscribe()
		for c := range ch {
			if err := engine.Reload(c); err != nil {
				logger.Printf("[warn] 配置热重载：%v", err)
			} else {
				logger.Printf("[info] 配置已热重载：%d 条路由，%d 个上游代理", len(c.Routes), len(c.Proxies))
			}
		}
	}()

	// ---- 监听配置文件变化（外部直接编辑 JSON 也能生效）----
	go watchConfig(store, engine, logger)

	// ---- 组装 handler ----
	adminPath := cfg.Listen.AdminPath
	echo := proxy.EchoHandler("/__echo")
	mainHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == adminPath || strings.HasPrefix(p, adminPath+"/") {
			adminSrv.ServeHTTP(w, r)
			return
		}
		if cfg.Global.EnableEcho && (p == "/__echo" || strings.HasPrefix(p, "/__echo/")) {
			echo.ServeHTTP(w, r)
			return
		}
		engine.ServeHTTP(w, r)
	})

	mainSrv := &http.Server{
		Addr:         cfg.Listen.Addr,
		Handler:      mainHandler,
		ReadTimeout:  seconds(cfg.Listen.ReadTimeout),
		WriteTimeout: seconds(cfg.Listen.WriteTimeout),
		IdleTimeout:  seconds(cfg.Listen.IdleTimeout),
		MaxHeaderBytes: func() int {
			if cfg.Listen.MaxHeaderKB > 0 {
				return cfg.Listen.MaxHeaderKB * 1024
			}
			return 1 << 20
		}(),
		ErrorLog: logger,
	}
	if cfg.Listen.IdleTimeout > 0 {
		mainSrv.IdleTimeout = time.Duration(cfg.Listen.IdleTimeout) * time.Second
	}

	banner(logger, store, cfg, adminPath, cfg.Listen.AdminAddr)

	errCh := make(chan error, 2)

	go func() {
		if cfg.TLS.Enabled && cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
			mainSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			logger.Printf("[info] HTTPS 监听 %s (TLS 已启用)", mainSrv.Addr)
			errCh <- mainSrv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			logger.Printf("[info] HTTP 监听 %s", mainSrv.Addr)
			errCh <- mainSrv.ListenAndServe()
		}
	}()

	// 独立的 HTTP→HTTPS 重定向
	if cfg.TLS.Enabled && cfg.TLS.Redirect {
		go func() {
			port := cfg.TLS.HTTPPort
			if port == 0 {
				port = 80
			}
			rs := &http.Server{
				Addr: fmt.Sprintf(":%d", port),
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					target := "https://" + r.Host + r.URL.RequestURI()
					http.Redirect(w, r, target, http.StatusMovedPermanently)
				}),
				ErrorLog: logger,
			}
			logger.Printf("[info] HTTP→HTTPS 重定向监听 :%d", port)
			if err := rs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Printf("[warn] 重定向服务异常: %v", err)
			}
		}()
	}

	// ---- 管理后台独立端口 ----
	var adminSrvHTTP *http.Server
	if cfg.Listen.AdminAddr != "" {
		adminSrvHTTP = &http.Server{
			Addr:         cfg.Listen.AdminAddr,
			Handler:      adminSrv,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 120 * time.Second,
			IdleTimeout:  120 * time.Second,
			ErrorLog:     logger,
		}
		go func() {
			logger.Printf("[info] 管理后台独立监听 %s", adminSrvHTTP.Addr)
			if err := adminSrvHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	// ---- 优雅退出 ----
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("服务异常退出: %v", err)
		}
	case s := <-sig:
		logger.Printf("[info] 收到信号 %s，正在优雅退出…", s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = mainSrv.Shutdown(ctx)
	if adminSrvHTTP != nil {
		_ = adminSrvHTTP.Shutdown(ctx)
	}
	engine.Stop()
	logger.Printf("[info] 已退出")
}

func seconds(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

func watchConfig(store *config.Store, engine *proxy.Engine, logger *log.Logger) {
	var last time.Time
	if fi, err := os.Stat(store.Path()); err == nil {
		last = fi.ModTime()
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		fi, err := os.Stat(store.Path())
		if err != nil {
			continue
		}
		if fi.ModTime().After(last) {
			last = fi.ModTime()
			if err := store.Reload(); err != nil {
				logger.Printf("[warn] 配置文件变更后重载失败: %v", err)
				continue
			}
			if err := engine.Reload(store.Get()); err != nil {
				logger.Printf("[warn] 配置文件已加载，但部分路由有问题: %v", err)
			} else {
				logger.Printf("[info] 检测到配置文件变化，已自动热重载")
			}
		}
	}
}

func banner(logger *log.Logger, store *config.Store, cfg *config.Config, adminPath, adminAddr string) {
	base := cfg.Listen.Addr
	if cfg.TLS.Enabled {
		base = "https://" + base
	} else {
		base = "http://" + base
	}
	if strings.HasPrefix(base, "http://:") || strings.HasPrefix(base, "https://:") {
		base = strings.Replace(base, "://:", "://127.0.0.1:", 1)
	}
	adminURL := base + adminPath + "/"
	if adminAddr != "" {
		adminURL = "http://" + strings.Replace(adminAddr, ":", "127.0.0.1:", 1) + adminPath + "/"
		if strings.HasPrefix(adminAddr, ":") {
			adminURL = "http://127.0.0.1" + adminAddr + adminPath + "/"
		}
	}
	logger.Println("──────────────────────────────────────────────")
	logger.Printf("  revproxy %s  反代引擎已启动", Version)
	logger.Println("──────────────────────────────────────────────")
	logger.Printf("  配置文件    : %s", store.Path())
	logger.Printf("  代理端口    : %s", cfg.Listen.Addr)
	logger.Printf("  管理后台    : %s", adminURL)
	logger.Printf("  管理员账号  : %s", cfg.Admin.Username)
	if cfg.Admin.PassHash == config.HashPassword("admin123") {
		logger.Printf("  初始密码    : admin123  ← 请登录后立即修改！")
	}
	logger.Printf("  已加载路由  : %d 条，上游代理 %d 个", len(cfg.Routes), len(cfg.Proxies))
	logger.Println("──────────────────────────────────────────────")
}
