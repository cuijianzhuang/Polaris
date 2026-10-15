# Polaris

多内核代理面板：一个主控管理 N 台服务器，每台运行一个 Agent，Agent 通过统一的内核适配层同时驱动 **Xray-core** 与 **sing-box**（并预留 Hysteria2 官方服务端、mihomo）。用户、入站、订阅在主控上只定义一次，由适配器翻译成各内核自己的配置。

```
┌──────────── 主控 polaris ────────────┐        ┌──────── 节点 polaris-agent ────────┐
│ React 后台 · REST API · 订阅 /sub/    │  mTLS  │ reconcile · 进程管理 · 流量上报     │
│ 期望状态 NodeSpec（带版本，可回滚）    │◀──────▶│  ├─ xray     (gRPC 热增删用户)      │
│ 流量结算 · 限额 · 证书 · 模板 · 链路   │ gRPC流 │  ├─ sing-box (SIGHUP 重载)          │
│ SQLite / PostgreSQL · 可选 Redis      │        │  ├─ hysteria (HTTP 认证回调)        │
└───────────────────────────────────────┘        │  └─ realm    (L4 中转转发)          │
                                                 └─────────────────────────────────────┘
```

代理流量由客户端直连节点内核，不经过主控；主控宕机时节点按最后一次成功应用的配置继续服务，流量在 Agent 本地落盘，恢复后补报。

## 功能

| 模块 | 内容 |
| --- | --- |
| 内核适配层 | `CoreAdapter` 接口：`Capabilities / Render / Validate / Start / Stop / Reload / AddUsers / RemoveUsers / CollectTraffic / OnlineIPs / Health`。新增内核只需新增一个适配器 |
| 协议 | VLESS（Vision / REALITY / XHTTP）、VMess、Trojan、Shadowsocks 2022、Hysteria2、TUIC v5、AnyTLS、ShadowTLS v3、Naive、SOCKS、HTTP；传输 TCP / WS / gRPC / HTTPUpgrade / XHTTP / mKCP |
| 能力矩阵 | 由适配器按内核版本计算（如 XHTTP 需 Xray ≥ 24.9.30、AnyTLS 需 sing-box ≥ 1.12），前端据此置灰不可选组合，主控创建时再校验；双内核都支持时按节点默认内核分配 |
| 期望状态同步 | 每次变更生成完整 `NodeSpec` 存入 `config_revisions`；Agent 只增删用户时走热更新，结构变化走 渲染 → 内核自检 → 原子替换 → 重载，失败自动回滚并上报；后台可一键回滚到任意已应用版本 |
| 用户 | 每个用户一个 UUID + 一个密码，SS 2022 按加密方式派生对应长度 PSK；配额、到期（固定日期或「首次使用后 N 天」）、IP 数限制（可选临时封禁）、授权（整台绑定节点 / 按分组 / 单独入站，三者取并集；节点页可查看谁有权使用、本节点流量与在线 IP，并直接绑定 / 解绑；批量绑定 / 解绑节点）、每月 / 周期重置、节点流量倍率；近 30 天每日流量图 |
| 订阅 | `/sub/{token}` 按 UA 或 `?format=` 输出 通用链接 / Clash(mihomo) / sing-box / Surge / Loon / Quantumult X；每种格式可维护多套规则模板。订阅设置：独立订阅域名、订阅标题、更新间隔、客服链接、一键停用；`subscription-userinfo` / `profile-title` / `support-url` / `announce`（公告）/ `routing`（Happ 路由规则，可开关）响应头；可在节点列表插入「剩余流量 / 到期 / 公告」信息节点或名称后缀 |
| 订阅信息页 | 浏览器打开订阅链接：公告、状态、用量与剩余、到期与重置周期、近 30 天流量柱状图、订阅二维码、12 个客户端一键导入（Clash Verge、sing-box、Shadowrocket、Stash、Hiddify、Happ、v2rayNG、Streisand、Surge、Loon、QX、Karing）、每个节点的链接与二维码、Telegram 一键绑定、客服入口；明暗主题自适应 |
| 订阅转换 | 参考 MiSub：订阅源（机场订阅链接 / 手动节点，自动识别 Base64、Clash、sing-box、SIP008，定时更新，记录流量与到期）+ 订阅组（面板用户节点 + 多个订阅源合并）；处理链：过滤（协议 / 地区 / 正则 / 去信息节点）、重命名（正则或 `{emoji} {regionZh} {index}` 模板）、排序（地区优先级）、去重；可切换第三方 subconverter 后端；SSRF 防护 |
| 规则与配置方案 | 参考 MiSub：内置 4 套方案（极简 / ACL4SSR 精简 / 流媒体与 AI / ACL4SSR 完整）+ ACL4SSR CM_Online 系列 7 套 + 自定义 ini URL；采用 subconverter ini 语法，内置引擎转换为各客户端原生写法（Clash rule-providers、sing-box rule-set、Surge / Loon / QX 远程规则），`[]@regions` 自动展开地区分组；第三方后端直接用方案作 config；全局或按订阅组设置，另有「跳过证书校验」「强制 UDP」开关 |
| 域名与 DDNS | DNS 服务商（Cloudflare / 自定义 Webhook）；域名绑定节点 IP（A / AAAA / 双栈，可选 CDN 代理）；Agent 定期探测公网 IP 随心跳上报，开启 DDNS 的节点 IP 变化后自动更新记录并告警 |
| 路由与负载均衡 | 出站负载均衡组（延迟最低 / 最稳定 / 故障转移 / 轮询 / 随机，探测地址、容差、兜底出站）；健康检查失败的成员自动摘除；入站可绑定固定出口；全局规则（所有节点）+ 节点规则；规则预设（拦截广告 / BT / 私有地址、国内直连、流媒体 / AI / Telegram 分流） |
| Telegram 机器人 | 超级管理员（全部节点与用户）/ 节点管理员（只管绑定的节点）/ 用户（订阅链接或信息页一键绑定）三种角色；查看节点、重启内核、强制同步、搜索用户、重置流量、续期；节点离线 / 下发失败 / IP 变更告警；用户流量与到期提醒；公告群发 |
| CDN | 入站可挂多个入口（直连 / CDN / 中转）；CDN 配置档含优选 IP（手填或定时拉取、可按 IP 展开）；Cloudflare 集成：自动建代理 DNS 记录、SSL 设为 Full (strict)、签发 Origin 证书并下发 |
| 链式代理 | L4 端口转发（realm，不解密）与协议中转（自动创建服务账号、出站与路由）；节点出站可经 WARP / 住宅 SOCKS / 面板内其他节点（`detour` 串链）；路由规则、urltest / selector 出站组；链路探测 |
| 入站模板 | 内置「REALITY 直连」「Hy2 + TUIC」「CDN WS 备用」「全协议」；选模板 → 选节点 → 预览（改派内核、端口冲突自动调整、跳过原因）→ 生成；改模板后差异同步，手动改过的字段保留；节点标签绑定模板，新节点上线自动套用 |
| 安全 | 主控 ↔ Agent 全程 mTLS（内置 CA，首次接入用一次性 token + CA 指纹钉扎，可吊销）；后台路径自定义，其余路径 404 或反代伪装站；bcrypt + TOTP + 5 次失败锁 15 分钟；REALITY 私钥 / 证书私钥 / SS 主密钥 / API Token 以 AES-GCM 加密存库；订阅 token 32 字节、接口限速；Agent 只执行结构化指令；内核下载校验官方 SHA256；全部写操作记审计日志 |

## 快速开始

### 主控

**一键安装（推荐）**：在一台 Linux（systemd，amd64 / arm64）服务器上以 root 执行。

```bash
# 绑定域名，自动安装 Caddy 申请 HTTPS（域名需已解析到本机）
bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) --domain panel.example.com

# 没有域名时用公网 IP + HTTP（仅建议测试）
bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh)
```

脚本从 GitHub Release 下载并校验 SHA-256，随机生成加密密钥和后台路径，写入 `/etc/polaris/polaris.env`，安装 systemd 服务并打印后台地址与初始密码；同时把 Agent 二进制放进数据目录，节点安装时直接从主控下载。
其他参数：`--version v0.1.0-beta`、`--port`、`--agent-port`、`--admin-path`、`--no-caddy`（自备反代）、`--mirror`（GitHub 下载加速前缀）。

安装后在服务器上输入 **`polaris`** 打开交互式管理菜单（`polaris-ctl` 为同一命令的别名）（安装 / 升级 / 卸载、启停与日志、查看后台地址、重置管理员密码、关闭二次验证、修改后台路径与端口、绑定域名切换 HTTPS、备份、开机自启），也可以直接带子命令：

```bash
polaris                      # 交互式菜单
polaris info                 # 后台地址、端口、运行状态
polaris update               # 升级（先备份数据库，配置不变）
polaris reset-password       # 忘记密码：生成新密码、解除锁定、注销所有会话
polaris reset-2fa            # 丢失验证器：关闭二次验证
polaris set-domain panel.example.com   # 由 IP + HTTP 切换到域名 + HTTPS
polaris set-path /my-admin | set-port 8443
polaris backup               # 配置 + 数据库一致性快照 → /root/polaris-backup-*.tar.gz
polaris node [-n 名称]       # 在主控本机安装节点（经 127.0.0.1 接入，无需放行端口）
polaris uninstall [--purge]  # 卸载；--purge 同时删除配置与数据
polaris help                 # 全部子命令
```

主控程序本身安装为 `/usr/local/bin/polaris-server`（由 systemd 运行）。维护操作也可直接调用：`polaris-server admin reset-password|reset-2fa|list`、`polaris-server backup FILE`（需与服务相同的环境变量；Docker 中为 `docker compose exec polaris polaris admin …`）。

**Docker**：

```bash
cp deploy/polaris.env.example deploy/polaris.env   # 至少修改 POLARIS_SECRET_KEY、POLARIS_ADMIN_PATH、POLARIS_PUBLIC_URL
docker compose up -d
docker compose logs polaris | grep password         # 未设置初始密码时会生成并打印
```

前置 Caddy / Nginx 处理 HTTPS（示例见 `deploy/Caddyfile`），访问 `https://panel.example.com/<admin_path>/`。
`9443` 是 Agent 的 mTLS 接入端口，需要直接开放，不能经反代。

也可以手动部署单二进制：`make all` 后把 `bin/polaris` 安装为 `/usr/local/bin/polaris-server`，配合 `deploy/polaris.service` 与 `/etc/polaris/polaris.env`。
主控启动后会把与自身同版本的 Agent（amd64 / arm64，校验 checksums.txt）缓存到 `<data_dir>/downloads/`，节点从主控下载 Agent，不依赖节点访问 GitHub；主控本身访问 GitHub 慢时可在设置项 `github_mirror` 填镜像前缀。也可以手动把 `bin/polaris-agent-linux-*` 放进该目录。

### 节点

后台「节点与内核 → 添加节点」会生成一行命令，在节点上以 root 执行：

```bash
curl -fsSL https://panel.example.com/install.sh | bash -s -- --token <一次性 token>
```

主控地址与 CA 指纹由主控写进 `install.sh`，命令里只需令牌（24 小时有效、一次性）。脚本会先检查与主控接入端口的连通性（不通时直接提示放行端口），再下载并校验 Agent、注册拿到客户端证书、写入 systemd 服务；弹窗会实时显示节点上线。Agent 数据在 `/var/lib/polaris-agent`，按期望状态下载内核到 `cores/{type}/{version}/` 并校验 SHA256，切换版本只改 `current` 软链。

- 国内机器可加 `--mirror https://<github 镜像前缀>`（Agent 与内核下载都会使用）
- 升级 Agent：面板升级后，「节点与内核」会标出 Agent 版本落后的节点，点「升级 Agent」或「升级全部 Agent」即可。Agent 经已有的 mTLS 通道从主控下载新版本，校验 SHA256 并试运行后替换自身、原地重启（内核随之重启，连接中断数秒）。也可在节点上执行 `… | bash -s -- --upgrade`
- 卸载：`… | bash -s -- --uninstall [--purge]`
- 主控服务器本机做节点：执行同一条命令即可（自动改用 127.0.0.1 连接），或在主控上运行 `polaris node`

> sing-box 官方构建不含 `with_v2ray_api`，此时 Agent 会跳过 sing-box 的按用户流量统计（Xray、Hysteria2 不受影响）。需要统计时请使用带该编译标签的构建：把二进制放到 `/var/lib/polaris-agent/cores/singbox/<版本>/sing-box`（已存在时 Agent 不会重新下载），或用 `--mirror` 指向托管该构建的镜像。

### 配置项

均可通过环境变量（`POLARIS_*`）或 `--config config.yaml` 设置：

| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `POLARIS_SECRET_KEY` | 列加密与会话签名密钥，**必填且需备份**；至少 16 位随机字符（`openssl rand -hex 32`），示例值会被拒绝 | — |
| `POLARIS_ADMIN_PATH` | 后台与 API 前缀 | `/` |
| `POLARIS_PUBLIC_URL` | 订阅链接中的外部地址 | — |
| `POLARIS_LISTEN` / `POLARIS_AGENT_LISTEN` | HTTP / Agent mTLS 监听 | `:8080` / `:9443` |
| `POLARIS_AGENT_HOSTS` | Agent 连接主控用的域名或 IP（写入服务端证书） | — |
| `POLARIS_DATA_DIR` | 数据目录（SQLite、CA、Agent 下载） | `./data` |
| `POLARIS_DB_DSN` | `postgres://…` 切换 PostgreSQL | SQLite |
| `POLARIS_REDIS_URL` | 流量计数缓冲 | 内存 |
| `POLARIS_DECOY` | 非后台路径反代到的伪装站 | 返回 404 |
| `POLARIS_SUB_RATE_LIMIT` | 订阅接口每 IP 每分钟请求数 | 30 |

ACME（Let's Encrypt）在后台「设置」填写邮箱即可启用：

- **HTTP 验证**（只需邮箱）：域名解析到节点，签发时主控让该节点的 Agent 在 80 端口临时应答 `/.well-known/acme-challenge/`（几秒后关闭；80 端口被占用时会提示是哪个程序）。节点按域名 / 地址自动匹配，也可手动指定。
- **DNS 验证**（再填 Cloudflare DNS Token）：支持泛域名 `*.example.com`，不需要 80 端口。
- **IP 证书**：域名处填节点 IP，按 Let's Encrypt 短期证书签发（约 6 天）。

证书在剩余不足 1/3 有效期时自动续期（90 天证书提前 30 天，IP 证书提前 2 天），并下发到使用它的节点。

**内核升级与回退**：每个内核的历史版本都保留在节点上（`current` 软链切换）。节点页会显示 GitHub 最新稳定版（API 受限时改读 releases/latest 跳转，可走 `github_mirror`），可一键升级、一键回退到上一版本，或「批量升级内核」把多个节点切到同一版本。新版本启动失败时 Agent 自动停掉新版本、切回旧版本并恢复原配置，面板随之把期望版本设回正在运行的版本（不会反复重试），并在内核卡片上显示失败原因。

**回落（一个端口多个协议）**：Xray 上的 VLESS / Trojan + TCP + TLS / REALITY 入站可配置回落，把不是本协议的连接按路径 / ALPN 转给同节点、监听 127.0.0.1、不开 TLS 的子入站（如 VLESS-WS、Trojan），实现 443 一个端口同时承载多种协议；子入站的订阅链接自动使用主入站的端口与 TLS。

## 开发

```
cmd/panel  cmd/agent           两个二进制
api/proto                      主控 ↔ Agent 的 gRPC 定义（connect-go，buf generate）
internal/model                 统一协议模型与能力矩阵
internal/core/{xray,singbox,hysteria,forwarder,mihomo,caddy}   内核适配器
internal/sub                   订阅解析与转换（处理链、地区识别）、渲染器 links / clash / singbox / surge / loon / quanx、信息页
internal/panel                 HTTP API、期望状态发布、Agent 服务、流量结算、定时任务
internal/agent                 reconcile、内核安装、进程管理、上报
internal/store                 GORM 仓储（SQLite / PostgreSQL）
web/                           React + TypeScript + Vite，构建产物 go:embed 进主控
```

```bash
make web build          # 构建前端与二进制
make test               # go vet + 单元测试
make test-cores XRAY=/path/xray SINGBOX=/path/sing-box
                        # 用真实内核校验渲染出的配置，并跑端到端测试：
                        # 主控 + 真实 Agent + Xray，覆盖注册、下发、热增删用户、回滚、订阅
cd web && npm run dev   # 前端开发服务器，/api 代理到 127.0.0.1:8080
```

接口文档：登录后访问 `<admin_path>/api/v1/openapi.json`（由路由自动生成）。实时节点数据走 WebSocket `<admin_path>/api/v1/ws`。

## 安全说明

发布前做过一轮安全复查，修复项都有回归测试（`internal/panel/security_test.go`）。部署时请注意：

- `POLARIS_SECRET_KEY` 至少 16 位随机字符，示例值会被拒绝；`POLARIS_ADMIN_PATH` 请设为难猜的路径（为 `/` 时启动会告警）。
- 主控前面放 Caddy / Nginx 终止 HTTPS；只信任来自本机或内网反代的 `X-Forwarded-For`（取最右一项）。
- Telegram 机器人只在私聊中响应；修改密码会让其他会话全部失效；重新绑定 TOTP 时旧验证器在新验证器确认前一直有效。
- Agent 安装脚本会校验二进制 SHA-256（主控提供的文件用主控下发的校验值，GitHub 回退用对应版本的 `checksums.txt`）。

已知并接受的风险：

- 登录失败锁定按账号计，知道用户名的人可以让管理员 15 分钟内无法登录（后台路径隐藏可缓解）。
- 管理员视为完全可信：订阅模板、Webhook 地址、CDN 优选 IP 来源等由管理员填写的内容不做沙箱限制。
- 后台会话 token 存在浏览器 localStorage 中，前端未启用 CSP；React 默认转义，代码中没有使用 `dangerouslySetInnerHTML`。

## 为什么内核以子进程运行，而不是引入 Xray / sing-box 的 Go 模块

Agent 托管的是**官方发布的二进制**，用户热增删、流量统计走官方 gRPC / HTTP API，配置用官方 `xray -test` / `sing-box check` 自检——实现本身就是官方的，只是没有链接进 Agent。这样选择是因为：

- **多内核共存与独立升级**：同一节点可同时跑 Xray、sing-box、mihomo、Hysteria，各自按节点切换版本、秒级回滚；嵌入库则每次升级内核都要重新发布 Agent。
- **依赖冲突**：Xray 与 sing-box 依赖不同的 quic-go / uTLS 分叉，同一个 Go 模块里很难同时满足；sing-box 的很多能力还取决于构建 tag（`with_quic`、`with_utls`、`with_v2ray_api`…），官方 release 已按推荐组合构建。
- **故障隔离**：内核 panic 或内存泄漏不会拖垮 Agent 和主控控制通道，Agent 可以继续上报并回滚。
- **许可证**：sing-box 为 GPL-3.0，直接链接会让 Agent 整体受 GPL 约束；子进程方式不受影响。
- **体积**：两个内核编进去后 Agent 会超过 60 MB，而大多数节点只用其中一个。

代价是需要进程管理和 API 客户端：Xray 的 HandlerService / StatsService 客户端是手写的 protobuf 编码（避免为几个消息引入整个 xray-core 依赖树），由 `make test-cores` 用真实二进制回归。

## 当前状态

已实现方案中的 M1–M6 主体功能，以及订阅转换、DDNS、负载均衡、Telegram 机器人、订阅设置与信息页。已知限制：

- Xray 的 Hysteria2 入站未开放（不同版本配置差异较大），Hy2 请使用 sing-box 或官方服务端
- sing-box 负载均衡组不支持「轮询 / 随机」（只有 urltest），在 sing-box 节点上会被拒绝
- 订阅信息页的 Happ / Hiddify / Streisand 等深链依赖客户端已安装；`routing` 头目前只有 Happ 识别
- 订阅模板使用 Go text/template
