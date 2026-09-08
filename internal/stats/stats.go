// Package stats 采集代理层的运行时指标与访问日志（内存环形缓冲）。
package stats

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type AccessLog struct {
	Time     time.Time `json:"time"`
	RouteID  string    `json:"routeId"`
	Route    string    `json:"route"`
	Method   string    `json:"method"`
	Host     string    `json:"host"`
	Path     string    `json:"path"`
	Status   int       `json:"status"`
	Duration int64     `json:"duration"` // 毫秒
	BytesOut int64     `json:"bytesOut"`
	ClientIP string    `json:"clientIp"`
	Upstream string    `json:"upstream"`
	ViaProxy string    `json:"viaProxy"`
	Error    string    `json:"error,omitempty"`
	Retry    int       `json:"retry,omitempty"`
}

type RouteStat struct {
	Requests  uint64 `json:"requests"`
	Status2xx uint64 `json:"status2xx"`
	Status3xx uint64 `json:"status3xx"`
	Status4xx uint64 `json:"status4xx"`
	Status5xx uint64 `json:"status5xx"`
	BytesOut  uint64 `json:"bytesOut"`
	TotalDur  uint64 `json:"totalDur"` // 毫秒累计
	MaxDur    uint64 `json:"maxDur"`
	Active    int64  `json:"active"`
	LastError string `json:"lastError,omitempty"`
	LastErrAt string `json:"lastErrAt,omitempty"`
}

type Snapshot struct {
	Uptime     string               `json:"uptime"`
	UptimeSec  int64                `json:"uptimeSec"`
	TotalReq   uint64               `json:"totalReq"`
	QPS        float64              `json:"qps"`
	QPS1m      []int                `json:"qps1m"`
	Active     int64                `json:"active"`
	BytesOut   uint64               `json:"bytesOut"`
	AvgDur     float64              `json:"avgDur"`
	Routes     map[string]RouteStat `json:"routes"`
	GoRoutines int                  `json:"goroutines"`
	MemMB      float64              `json:"memMB"`
	LogCount   int                  `json:"logCount"`
}

type Collector struct {
	mu       sync.RWMutex
	start    time.Time
	total    uint64
	bytes    uint64
	active   int64
	routes   map[string]*RouteStat
	buckets  [60]uint64 // 近 60 秒请求分布
	curSec   int64
	logs     []AccessLog
	logPos   int
	logCap   int
	filePath string
	fileMu   sync.Mutex
}

func New(capacity int, filePath string) *Collector {
	if capacity <= 0 {
		capacity = 500
	}
	c := &Collector{
		start:    time.Now(),
		routes:   map[string]*RouteStat{},
		logs:     make([]AccessLog, 0, capacity),
		logCap:   capacity,
		filePath: filePath,
		curSec:   time.Now().Unix(),
	}
	if filePath != "" {
		go c.fileWriter()
	}
	return c
}

// Begin 请求开始
func (c *Collector) Begin() {
	atomic.AddInt64(&c.active, 1)
	atomic.AddUint64(&c.total, 1)
	sec := time.Now().Unix()
	c.mu.Lock()
	if sec != c.curSec {
		gap := sec - c.curSec
		if gap >= 60 {
			c.buckets = [60]uint64{}
		} else {
			for i := int64(0); i < gap; i++ {
				c.curSec++
				c.buckets[c.curSec%60] = 0
			}
		}
		c.curSec = sec
	}
	c.buckets[sec%60]++
	c.mu.Unlock()
}

// End 请求结束
func (c *Collector) End(l AccessLog) {
	atomic.AddInt64(&c.active, -1)
	atomic.AddUint64(&c.bytes, uint64(l.BytesOut))

	c.mu.Lock()
	rs := c.routes[l.RouteID]
	if rs == nil {
		rs = &RouteStat{}
		c.routes[l.RouteID] = rs
	}
	rs.Requests++
	switch {
	case l.Status >= 500:
		rs.Status5xx++
	case l.Status >= 400:
		rs.Status4xx++
	case l.Status >= 300:
		rs.Status3xx++
	default:
		rs.Status2xx++
	}
	rs.BytesOut += uint64(l.BytesOut)
	rs.TotalDur += uint64(l.Duration)
	if uint64(l.Duration) > rs.MaxDur {
		rs.MaxDur = uint64(l.Duration)
	}
	if l.Error != "" {
		rs.LastError = l.Error
		rs.LastErrAt = l.Time.Format("15:04:05")
	}
	rs.Active = atomic.LoadInt64(&c.active)

	// 环形日志
	if len(c.logs) < c.logCap {
		c.logs = append(c.logs, l)
	} else {
		c.logs[c.logPos] = l
		c.logPos = (c.logPos + 1) % c.logCap
	}
	c.mu.Unlock()

	if c.filePath != "" {
		select {
		case c.fileCh() <- l:
		default:
		}
	}
}

var logCh chan AccessLog
var logChOnce sync.Once

func (c *Collector) fileCh() chan AccessLog {
	logChOnce.Do(func() { logCh = make(chan AccessLog, 4096) })
	return logCh
}

func (c *Collector) fileWriter() {
	ch := c.fileCh()
	f, err := os.OpenFile(c.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for l := range ch {
		c.fileMu.Lock()
		_ = enc.Encode(l)
		c.fileMu.Unlock()
	}
}

// Logs 返回最近的访问日志（倒序，最新在前）
func (c *Collector) Logs(limit int) []AccessLog {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := len(c.logs)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]AccessLog, 0, limit)
	for i := 0; i < limit; i++ {
		idx := (c.logPos + n - 1 - i + n) % n
		out = append(out, c.logs[idx])
	}
	return out
}

// Clear 清空内存访问日志
func (c *Collector) Clear() {
	c.mu.Lock()
	c.logs = c.logs[:0]
	c.logPos = 0
	c.mu.Unlock()
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now().Unix()
	var qps float64
	series := make([]int, 60)
	for i := 0; i < 60; i++ {
		idx := (now - int64(59-i)) % 60
		if idx < 0 {
			idx += 60
		}
		v := c.buckets[idx]
		series[i] = int(v)
	}
	// 近 5 秒平均
	var sum uint64
	for i := 0; i < 5; i++ {
		idx := (now - int64(i)) % 60
		if idx < 0 {
			idx += 60
		}
		sum += c.buckets[idx]
	}
	qps = float64(sum) / 5.0

	routes := make(map[string]RouteStat, len(c.routes))
	var totalDur uint64
	for k, v := range c.routes {
		routes[k] = *v
		totalDur += v.TotalDur
	}
	total := atomic.LoadUint64(&c.total)
	avg := 0.0
	if total > 0 {
		avg = float64(totalDur) / float64(total)
	}
	return Snapshot{
		Uptime:    time.Since(c.start).Round(time.Second).String(),
		UptimeSec: int64(time.Since(c.start).Seconds()),
		TotalReq:  total,
		QPS:       qps,
		QPS1m:     series,
		Active:    atomic.LoadInt64(&c.active),
		BytesOut:  atomic.LoadUint64(&c.bytes),
		AvgDur:    avg,
		Routes:    routes,
		LogCount:  len(c.logs),
	}
}
