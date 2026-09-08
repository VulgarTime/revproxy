# revproxy —— 带 Web 后台的反向代理

用 Go 写的单文件反向代理，**零第三方依赖**，自带可视化管理后台，配置热重载。

它的存在理由很单纯：**nginx 的 `proxy_pass` 不支持让上游走 SOCKS5**。当你的上游是「本地直连不通、必须先过一层 socks5」的地址时（内网服务、远端 API、家里的 NAS…），nginx 只能再叠一层代理软件绕路，而 revproxy 直接在路由里选一条 socks5 就行。

---

## 目录

- [特性](#特性)
- [30 秒上手](#30-秒上手)
- [和 nginx 的配置对照](#和-nginx-的配置对照)
- [SOCKS5 用法（核心场景）](#socks5-用法核心场景)
- [管理后台](#管理后台)
- [配置文件字段参考](#配置文件字段参考)
- [REST API](#rest-api)
- [部署方式](#部署方式)
- [性能与调优](#性能与调优)
- [二次开发指南](#二次开发指南)
- [已知限制](#已知限制)

---

## 特性

**代理核心**

- 任意 HTTP/HTTPS 上游，`http://` / `https://` 通吃
- **上游可走 SOCKS5 / SOCKS5 用户名密码认证 / HTTP CONNECT 代理**，粒度到「全局 → 路由 → 单个节点」
- 路径重写（`rewrite`、去前缀）、Host 通配（`*.example.com`）、前缀 / 精确 / 正则三种匹配
- WebSocket、SSE、gRPC、大文件流式上传下载（自动 `FlushInterval`，不做缓冲）
- 负载均衡：轮询 / 加权轮询（nginx 同款平滑算法）/ 最少连接 / IP 哈希 / 随机
- 主动健康检查（HTTP 或纯 TCP）+ 被动熔断，故障节点自动摘除与恢复，备用节点（backup）自动顶上
- 失败自动换节点重试（对无请求体的请求安全重放）

**治理能力**

- 每路由独立：Basic 认证、IP 白/黑名单（CIDR）、令牌桶限流（按 IP 或全局）
- 自定义请求头 / 响应头 / 删除请求头，自动补齐 `X-Forwarded-For / Host / Proto`
- 上游自签证书跳过校验、保留客户端 Host

**可观测**

- 实时 QPS、延迟、状态码分布、出流量、活跃连接
- 访问日志（内存环形缓冲 + 可选落盘 JSON Lines）
- 后台一键测试「代理连通性」和「上游可达性」，不用再手动 curl

**运维友好**

- **配置热重载**：后台改完立即生效，不用 `nginx -s reload`，不中断现有连接
- 单二进制、零依赖、无 CGO，交叉编译出什么平台都能跑
- 配置文件就是一份 JSON，可直接 git 管版本；外部改动 3 秒内自动生效

---

## 30 秒上手

```bash
# 方式一：直接跑（需要 Go 1.21+）
git clone <你的仓库> revproxy && cd revproxy
make run            # 编译并前台启动，监听 :8080

# 方式二：Docker
docker compose up -d --build

# 方式三：安装为 systemd 服务
sudo make install
```

打开 <http://127.0.0.1:8080/__admin/> ，用 `admin` / `admin123` 登录（**首次登录后请立刻改密码**）。

命令行参数：

| 参数 | 说明 | 默认 |
|---|---|---|
| `-config` | 配置文件路径 | `./data/config.json` |
| `-addr` | 代理监听地址 | `:8080` |
| `-admin-addr` | 管理后台独立端口，空则共用代理端口 | 空 |
| `-data` | 数据目录（与 `-config` 二选一） | `./data` |
| `-debug` | 打印每条访问日志 | 关 |
| `-version` | 打印版本 | — |

环境变量：`REVPROXY_CONFIG`、`REVPROXY_DATA`、`PORT`（云平台常用，存在时自动覆盖监听端口）。

---

## 和 nginx 的配置对照

迁移的时候对着改就行：

| nginx 指令 | revproxy 字段 |
|---|---|
| `server_name a.com;` | `route.host` = `a.com`（支持 `*.a.com`，空=任意） |
| `location /api { ... }` | `route.path` = `/api`，`route.match` = `prefix` |
| `location = /exact` | `route.match` = `exact` |
| `location ~ ^/re/\d+` | `route.match` = `regex` |
| `proxy_pass http://127.0.0.1:9000/;` | `route.rewrite` = `/`（去前缀），`backends[].url` = `http://127.0.0.1:9000` |
| `proxy_pass http://upstream;` | 不填 `rewrite`，原样转发 |
| `upstream` 多server + `weight` | `backends[]` + `weight` + `lb=weighted` |
| `proxy_set_header X-K V;` | `headers.request: [{name, value}]` |
| `proxy_set_header X-K "";` | `headers.remove: ["X-K"]` |
| `add_header X-K V;` | `headers.response: [{name, value}]` |
| `proxy_next_upstream error timeout;` | `route.retry` = 1~3 |
| `proxy_connect_timeout 10s;` | `global.dialTimeout` |
| `proxy_read_timeout 60s;` | `route.timeout`（0=不限，WebSocket 自动跳过） |
| `max_fails=3 fail_timeout=10s` | `health.fails` / `health.interval` |
| `proxy_set_header Host $host;` | `route.preserveHost` = true |
| `proxy_ssl_verify off;` | `route.insecureTLS` = true |
| `allow 10.0.0.0/8; deny all;` | `access.ipAllow` / `access.ipDeny` |
| `auth_basic` | `access.basicAuth` |
| `limit_req zone=one rate=10r/s;` | `access.rate` |
| `backup` | `backends[].backup` = true |
| **无对应能力** | **`proxyId`：指定上游走 SOCKS5** ✅ |

---

## SOCKS5 用法（核心场景）

### 三个层级，就近覆盖

```
全局默认代理 (global.defaultProxy)
        ↓ 未指定时继承
路由级代理   (route.proxyId)
        ↓ 节点未指定时继承
节点级代理   (backends[].proxyId)
```

特殊取值：`__direct__` = 强制直连；空 = 继承上一层。

### 万能反代（域名写在路径里）：一个入口反代任意站点

这就是"像 nginx 一样随便反代"的用法：**把目标域名写进访问路径的第一层**，不用为每个站点单独建规则。

```
你访问   http://191.168.1.1/a.com/sd.txt
实际请求 http://a.com/sd.txt
```

**后台配置：**

1. **上游代理（添加代理）→ 添加代理**：先把 SOCKS5 代理加进来（`127.0.0.1:1080`，有账号密码就填）
2. **路由 → 添加路由**：

| 字段 | 值 | 说明 |
|---|---|---|
| 名称 | `万能反代` | 随便起 |
| 路由模式 | **万能反代（域名写在路径里）** | 关键选项 |
| 路径前缀 | `/` | 域名直接跟在根后面；想挂子路径填 `/proxy`，则访问 `/proxy/a.com/x` |
| 目标协议 | `http` | 要反代 https 站点选 `https` |
| 路由级默认代理 | **直连** | 没配域名规则的站点走这里（本地能直连的站） |
| 域名规则 | `b.com` → 你的 socks5 | 本地连不通的站单独指定出口 |

保存后效果：

| 你访问 | 实际请求 | 出口 |
|---|---|---|
| `http://IP/a.com/sd.txt` | `http://a.com/sd.txt` | 直连（默认） |
| `http://IP/b.com/api` | `http://b.com/api` | SOCKS5（命中域名规则） |
| `http://IP/b.com:8080/x` | `http://b.com:8080/x` | SOCKS5（端口会保留） |

- 域名规则支持通配符：`*.telegram.org` 一次覆盖所有子域。
- 域名规则为空时，所有站点都走「路由级默认代理」。
- 只访问 `http://IP/` 不带域名会返回 400，并提示正确格式。

### 多个 SOCKS5 代理：按域名分配（代理不是固定的情况）

如果你的出口代理不固定——`a.com` 走 `1.1.1.1:11`，`b.com` 走 `2.2.2.2:22`，其余站点直连——用**域名规则**一条路由就搞定，**不需要为每个域名建路由**。

**第一步**：上游代理（添加代理）→ 把每个 SOCKS5 代理都加进来，起好认的名字

| 名称 | 类型 | 地址 |
|---|---|---|
| 代理A | socks5 | `1.1.1.1:11` |
| 代理B | socks5 | `2.2.2.2:22` |
| 代理C | socks5 | `3.3.3.3:33` |

**第二步**：编辑万能反代路由，「域名规则」逐条添加映射

| 域名 | 走哪个代理 |
|---|---|
| `a.com` | 代理A |
| `b.com` | 代理B |
| `*.telegram.org` | 代理C（通配符，一次覆盖所有子域） |

- 域名规则**数量不限**，想加多少条加多少条。
- 匹配不到任何规则的域名，走该路由的「路由级默认代理」（可设为直连）。
- 域名带端口也能匹配：访问 `/b.com:8080/x` 照样命中 `b.com` 的规则。

实测（两个本地 SOCKS5 实例分别记录日志）：访问 `/a.com/` 只在代理A 的日志里出现 `CONNECT a.com:9011`，访问 `/b.com/` 只在代理B 出现 `CONNECT b.com:9022`，互不干扰；未配置的域名两个代理都没有记录（走默认直连）。

### 实战：代理 GitHub 的 m3u（HTTPS + 必须走 SOCKS5）

> 需求：本地网络直接访问不了 `raw.githubusercontent.com`，必须经由一台 SOCKS5 才能拉到。
> 目标：本地访问 `http://192.168.1.111/raw.githubusercontent.com/Kimentanm/aptv/refs/heads/master/m3u/ahyd.m3u`，
> 就等价于访问 `https://raw.githubusercontent.com/Kimentanm/aptv/refs/heads/master/m3u/ahyd.m3u`。

**第一步**：上游代理（添加代理）→ 添加你的 SOCKS5（假设 `61.218.31.99:24080`，有账号密码就填）。记下它的代理名，下面用 `我的SOCKS5`。

**第二步**：路由 → 添加路由（一条搞定，路径里直接写完整域名）：

| 字段 | 值 | 说明 |
|---|---|---|
| 名称 | `github万能反代` | 随便起 |
| 路由模式 | **万能反代（域名写在路径里）** | 关键选项 |
| 路径前缀 | `/` | 域名直接跟在根后面 |
| 目标协议 | **`https`** | GitHub 是 https，必须选 https，否则 80 端口拉不到 |
| 路由级默认代理 | 直连 | 万一以后反代别的 http 站点用 |
| 域名规则 | `raw.githubusercontent.com` → `我的SOCKS5` | 命中就走 SOCKS5，否则按默认出口 |

保存后实际效果：

| 你访问 | 实际请求 | 出口 |
|---|---|---|
| `http://192.168.1.111/raw.githubusercontent.com/Kimentanm/aptv/.../ahyd.m3u` | `https://raw.githubusercontent.com/Kimentanm/aptv/.../ahyd.m3u` | **SOCKS5**（命中域名规则） |

要点：

- **目标协议选 `https`** —— 这是最容易踩的坑。路径里带的是域名，协议得由这条路由的「目标协议」决定；选 http 会去连 GitHub 的 80 端口而拉空。
- 域名规则只配 `raw.githubusercontent.com` 就够了，`gh.aptv.app`、`github.com` 等同源不同域并不会被这条规则覆盖（需要就再加一条）。
- 如果某次失败，错误页会明确写出**真实目标域名**和**实际出口代理名**（例如 `"upstream":"raw.githubusercontent.com","viaProxy":"我的SOCKS5"`），方便排查到底是没走代理还是代理连不通。

### 兜底设 http、个别域名（如 GitHub）要 https

万能反代路由只有**一个**「目标协议」。如果你把兜底设成 `http`（因为大部分站是 http），会遇到 `raw.githubusercontent.com` 这类只认 https 的站被 301 跳回 https。

解决办法：**在域名规则里给该域名单独指定协议**（每条域名规则多了个「协议」下拉，留空=继承路由默认，可选 `http` / `https`）。

- 兜底路由目标协议：`http`
- 域名规则：`raw.githubusercontent.com` → 走你的 SOCKS5，**协议选 `https`**

这样 `/raw.githubusercontent.com/...` 强制走 https（不再 301），其它没配协议的域名仍按兜底 `http` 处理。这同时满足「有些站 http、有些站 https」的混合需求，不用为 https 站单独建路由。

### 路径别名：把域名换成你自己的名字（nginx 式 location 重写）

> 不想把真实域名暴露在上游路径里？比如 `http://IP/raw.githubusercontent.com/...` 想写成 `http://IP/gh/...`，
> 或 `http://IP/39.13.69.2/abc.com/xx/index.txt` 想写成 `http://IP/myip/xx/index.txt`。
> 这正是 nginx 的 `location /gh/ { proxy_pass https://raw.githubusercontent.com/; }` —— revproxy 用**普通路由 + 重写为 `/`** 实现，不用万能反代。
>
> 具体路由（如 `/gh`、`/myip`）会自动压过兜底 `/`（最长前缀优先，和 nginx 一致），不用手动调顺序。

**核心原理**：`重写为 /` = 去掉路由匹配到的路径前缀，剩下的拼到上游地址后面；上游地址里可以直接带基础路径。

| 你的访问 | 路由配置（普通模式） | 实际请求 |
|---|---|---|
| `http://IP/gh/Kimentanm/aptv/.../ahyd.m3u` | 路径前缀 `/gh`，重写为 `/`，上游 `https://raw.githubusercontent.com`（走 SOCKS5） | `https://raw.githubusercontent.com/Kimentanm/.../ahyd.m3u` |
| `http://IP/myip/xx/index.txt` | 路径前缀 `/myip`，重写为 `/`，上游 `http://39.13.69.2/abc.com` | `http://39.13.69.2/abc.com/xx/index.txt` |

- 想反代「任意域名」才用万能反代；想给「固定上游」起个别名，用普通路由 + 重写即可。
- 上游地址带基础路径（如 `http://39.13.69.2/abc.com`）是直接写进上游 URL 的，重写时前缀被剥掉、基础路径被保留。

### 场景一：反代 Telegram Bot API（本地直连不通）

后台操作：**上游代理（添加代理） → 添加代理**（类型 socks5，地址 `127.0.0.1:1080`，有密码就填）→ **路由 → 新建**：

| 字段 | 值 |
|---|---|
| 名称 | `telegram-api` |
| 路径 | `/tg` |
| 重写为 | `/` |
| 上游节点 | `https://api.telegram.org` |
| 走哪条代理 | 刚建的 socks5 |

保存后 `curl http://127.0.0.1:8080/tg/bot<TOKEN>/getMe` 即可。

等价的 JSON（写入配置文件同样生效）：

```json
{
  "name": "telegram-api",
  "enabled": true,
  "path": "/tg",
  "match": "prefix",
  "rewrite": "/",
  "backends": [{ "url": "https://api.telegram.org", "weight": 1 }],
  "lb": "round_robin",
  "proxyId": "px_home_socks5",
  "retry": 2,
  "health": { "enabled": true, "path": "/", "interval": 30, "timeout": 5 }
}
```

代理池里对应的条目：

```json
{
  "id": "px_home_socks5",
  "name": "家里 socks5",
  "type": "socks5",
  "addr": "1.2.3.4:1080",
  "username": "user",
  "password": "pass",
  "timeout": 10
}
```

### 场景二：同一个入口，不同上游走不同代理

```
/ai     → https://api.openai.com      走 代理A（socks5，境外）
/home   → http://192.168.1.10:8080    走 代理B（socks5，家里内网）
/local  → http://127.0.0.1:9000       直连
```

三条路由各选各的代理，互不干扰。节点级覆盖更细：

```json
"backends": [
  { "url": "http://10.0.0.1:80",   "weight": 3, "proxyId": "px_a" },
  { "url": "http://10.0.0.2:80",   "weight": 1, "proxyId": "px_b" },
  { "url": "http://127.0.0.1:80",  "weight": 1, "proxyId": "__direct__", "backup": true }
]
```

### 场景三：代理挂了自动降级

主节点走 socks5、备用节点直连（本项目自带的演示路由就是这个组合）：主节点连续失败被摘除后，流量自动落到备用节点，socks5 恢复后自动切回。

### 内置自测上游：`internal://echo`

想验证反代但手边没有后端？把节点地址填 `internal://echo`，revproxy 会直接构造回显响应（不经过网络），返回它实际收到的路径、查询串和请求头。5 秒内就能确认「路由匹配 + 路径重写 + 自定义头」是否符合预期。

### 排错顺序

1. 后台 → 上游代理 → **测试**（验证 socks5 本身通不通，会去连 `www.cloudflare.com:443`）
2. 路由编辑 → 节点右侧 **测试**（验证「这个上游 + 这条代理」的组合）
3. 访问日志里看 `viaProxy` 列，确认确实走了预期的代理
4. 上游健康状态页看节点是否 `alive`

---

## 管理后台

路径默认 `/__admin/`（可在设置里改），五个页面：

- **概览**：总请求、QPS（近 60 秒曲线）、活跃连接、平均延迟、出流量、运行时长；路由实时指标；节点健康状态
- **路由**：增删改查、启停、复制；快速添加向导；每个节点可单独测连通性
- **上游代理**：SOCKS5 / HTTP CONNECT 代理池，支持用户名密码，一键测连通性
- **访问日志**：实时滚动（3 秒刷新），含状态、耗时、命中的上游、走的哪条代理、客户端 IP
- **设置**：监听端口、日志、全局超时、默认代理、信任 XFF、HTTPS 证书、改密码、导入导出配置

移动端自适应，手机上也能改配置。

---

## 配置文件字段参考

文件默认在 `data/config.json`，改完 3 秒内自动热重载，不需要重启。

```jsonc
{
  "listen": {
    "addr": ":8080",          // 代理监听
    "adminAddr": "",          // 管理后台独立端口，空=共用
    "adminPath": "/__admin",  // 共用端口时的路径前缀
    "readTimeout": 0,         // 秒，0=不限（WebSocket 必须为 0）
    "writeTimeout": 0,
    "idleTimeout": 120
  },
  "admin": { "username": "admin", "sessionTTL": 12 },
  "log": { "level": "info", "access": true, "file": "", "buffer": 500 },
  "global": {
    "defaultProxy": "",       // 全局默认代理 ID
    "dialTimeout": 10,        // 建连超时（含 socks5 握手）
    "responseTimeout": 0,     // 等上游响应头超时，0=不限
    "keepAlive": 30,
    "maxIdleConns": 200,
    "maxConnsPerHost": 0,     // 0=不限
    "trustProxy": false,      // 前面还有 CDN 时才开，否则 IP 白名单可被伪造绕过
    "enableEcho": true        // 内置 /__echo 自测端点
  },
  "proxies": [
    { "id": "px1", "name": "本地", "type": "socks5", "addr": "127.0.0.1:1080",
      "username": "", "password": "", "timeout": 10 }
  ],
  "routes": [
    {
      "id": "rt1", "name": "示例", "enabled": true,
      "host": "",              // 空=任意；支持 *.example.com
      "path": "/api", "match": "prefix", "priority": 0,
      "rewrite": "/",          // 把 path 前缀替换掉；空=原样转发
      "backends": [
        { "url": "http://127.0.0.1:9000", "weight": 1, "proxyId": "", "backup": false }
        // url 也可以是 internal://echo —— 内置自测上游，不经网络
      ],
      "lb": "round_robin",     // round_robin|weighted|least_conn|ip_hash|random
      "proxyId": "px1",        // __direct__=直连；空=继承全局
      "insecureTLS": false,
      "preserveHost": false,
      "retry": 1,              // 网络错误换节点重试次数
      "timeout": 0,            // 单次请求总超时，秒
      "headers": {
        "request":  [{ "name": "X-Token", "value": "abc" }],
        "response": [{ "name": "X-Powered", "value": "revproxy" }],
        "remove":   ["Cookie"]
      },
      "health": { "enabled": true, "path": "/healthz", "interval": 10,
                  "timeout": 3, "fails": 3, "passes": 2, "codes": [] },
      "access": {
        "basicAuth": { "enabled": false, "username": "", "password": "", "realm": "Restricted" },
        "ipAllow": [], "ipDeny": [],
        "rate": { "enabled": false, "rps": 10, "burst": 20, "perIp": true }
      }
    }
  ],
  "tls": { "enabled": false, "certFile": "", "keyFile": "", "redirect": false, "httpPort": 80 }
}
```

路由匹配顺序：**priority 大的优先 → 有 Host 的优先 → 路径长的优先**（和 nginx 的 `location` 直觉一致）。

---

## REST API

所有接口在 `{adminPath}/api/` 下，需先登录拿到 Cookie（或带 `Authorization: Bearer <token>`），写操作需带 `X-Revproxy-Admin: 1` 头或 JSON 内容类型。

```bash
# 登录
curl -c cj -X POST http://127.0.0.1:8080/__admin/api/login \
  -H 'Content-Type: application/json' -d '{"user":"admin","pass":"admin123"}'

# 新建 SOCKS5 代理
curl -b cj -X POST http://127.0.0.1:8080/__admin/api/proxies \
  -H 'Content-Type: application/json' \
  -d '{"name":"home","type":"socks5","addr":"1.2.3.4:1080","username":"u","password":"p"}'

# 新建路由
curl -b cj -X POST http://127.0.0.1:8080/__admin/api/routes -H 'Content-Type: application/json' -d '{
  "name":"tg","enabled":true,"path":"/tg","match":"prefix","rewrite":"/",
  "backends":[{"url":"https://api.telegram.org","weight":1}],
  "lb":"round_robin","proxyId":"px_xxx","retry":2}'

# 测试代理 / 上游连通性
curl -b cj -X POST http://127.0.0.1:8080/__admin/api/proxy-test   -H 'Content-Type: application/json' -d '{"id":"px_xxx"}'
curl -b cj -X POST http://127.0.0.1:8080/__admin/api/upstream-test -H 'Content-Type: application/json' \
  -d '{"url":"https://api.telegram.org","proxyId":"px_xxx","path":"/"}'
```

其余接口：`GET/PUT /config`、`GET/POST /routes`、`GET/PUT/DELETE /routes/{id}`、`POST /routes/{id}/toggle`、`POST /routes/{id}/duplicate`、`GET/POST/PUT/DELETE /proxies`、`GET /logs?limit=200`、`POST /logs/clear`、`POST /password`、`PUT /settings`、`POST /reload`、`GET /export`、`POST /import`、`GET /overview`、`GET /system`。

---

## 部署方式

### 二进制（推荐，最省事）

```bash
make cross                      # 交叉编译到 dist/
ls dist/
#   revproxy-linux-amd64   revproxy-linux-arm64   revproxy-darwin-arm64
#   revproxy-windows-amd64.exe   ...
scp dist/revproxy-linux-amd64 server:/usr/local/bin/revproxy
```

### systemd

```bash
make build && sudo make install    # 装二进制 + /etc/revproxy/config.json + 服务
journalctl -u revproxy -f          # 看日志
sudo systemctl restart revproxy
sudo make uninstall                # 卸载
```

或手动：把 `deploy/revproxy.service` 放到 `/etc/systemd/system/`，改里面的路径即可。

### Docker

```bash
docker compose up -d --build
docker compose logs -f
```

> 容器里要访问**宿主机上的 socks5**：地址填 `host.docker.internal:1080`（compose 里已加解析），或用 `network_mode: host`。

### Kubernetes

镜像是无状态的，配置挂 ConfigMap、数据挂 PVC 即可；探活用 `/__echo`（记得在设置里开启）。

---

## 性能与调优

- 反代是 IO 密集型，goroutine 模型天然适配；实测单核轻松数千并发，瓶颈通常在上游或 socks5 代理本身
- `global.maxIdleConns` 调大可减少到上游的重复建连（走 socks5 时建连更贵，建议 200+）
- 走 socks5 时每跳都会多一次握手，`global.dialTimeout` 别设太短（建议 ≥10s）
- 大文件传输不受内存影响（全程流式，只在管理日志里记字节数）
- 想看每个请求明细：`-debug` 或设置里日志级别调 `debug`（高 QPS 下会有开销）

---

## 二次开发指南

代码约 4000 行，纯标准库，结构很直白：

```
main.go                     启动、端口组装、热重载监听、优雅退出
internal/config/config.go   配置模型 + JSON 持久化 + 热重载 + 密码哈希
internal/socks/socks5.go    SOCKS5(RFC1928/1929) + HTTP CONNECT 拨号，零依赖实现
internal/proxy/engine.go    路由匹配、重写、访问控制、统计、连通性测试
internal/proxy/upstream.go  Transport 复用、负载均衡、健康检查、熔断
internal/proxy/middleware.go IP/CIDR 匹配、令牌桶限流、Basic Auth
internal/admin/server.go    管理后台 REST API
internal/admin/auth.go      登录与 HMAC 会话令牌
internal/stats/stats.go     指标采集与访问日志环形缓冲
web/                        前端（原生 JS，无构建步骤，通过 embed 打进二进制）
```

改代码的常见入口：

| 想改什么 | 改哪里 |
|---|---|
| 加一种新的路由匹配方式 | `engine.go` 的 `RouteHandler.match` |
| 加一种负载均衡算法 | `upstream.go` 的 `Pool.Next` |
| 加一种代理协议（如 Shadowsocks） | `socks/socks5.go` 里加一个 `DialContext` 分支 + `config.ProxyType` |
| 加个中间件（CORS、WAF、JWT…） | `middleware.go`，在 `engine.go` 的 `ServeHTTP` 链上插一步 |
| 加统计指标 | `stats/stats.go` 的 `RouteStat` + 后台 `overview` 接口 |
| 改前端 | `web/` 三个文件，改完 `make build` 即可生效（embed 进二进制） |

本地开发：

```bash
make dev          # go run，改完 Ctrl-C 重跑
go vet ./...      # 静态检查
make test         # 单元测试
```

改前端不用重新编译？不行——前端是 embed 进二进制的，改完必须 `make build`。想热改前端的话，可以临时把 `web/` 挂到 nginx 上反代 `/__admin/`。

---

## 已知限制

- 暂不内置 ACME 自动证书（不想引入外部依赖）。要 HTTPS 自动续期就前置 Caddy / traefik，或用 acme.sh 签好后在设置里填证书路径
- HTTP/2 到上游默认关闭（强制 HTTP/1.1，行为与 nginx 默认一致），到客户端的 HTTP/2 需前置支持 TLS 的网关
- 有请求体（POST/PUT）的请求失败不重试——重放 body 需要缓冲，会在大文件上传时爆内存，所以宁可不重试
- 管理后台是单管理员账号，没做多用户/角色
