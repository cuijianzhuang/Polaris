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
| 用户 | 每个用户一个 UUID + 一个密码，SS 2022 按加密方式派生对应长度 PSK；配额、到期、IP 数限制（可选临时封禁）、分组授权、每月 / 周期重置、节点流量倍率 |
| 订阅 | `/sub/{token}` 按 UA 或 `?format=` 输出 通用链接 / Clash(mihomo) / sing-box / Surge，浏览器打开显示信息页 + 二维码 + 一键导入；`subscription-userinfo` 响应头；规则模板后台可编辑 |
| CDN | 入站可挂多个入口（直连 / CDN / 中转）；CDN 配置档含优选 IP（手填或定时拉取、可按 IP 展开）；Cloudflare 集成：自动建代理 DNS 记录、SSL 设为 Full (strict)、签发 Origin 证书并下发 |
| 链式代理 | L4 端口转发（realm，不解密）与协议中转（自动创建服务账号、出站与路由）；节点出站可经 WARP / 住宅 SOCKS / 面板内其他节点（`detour` 串链）；路由规则、urltest / selector 出站组；链路探测 |
| 入站模板 | 内置「REALITY 直连」「Hy2 + TUIC」「CDN WS 备用」「全协议」；选模板 → 选节点 → 预览（改派内核、端口冲突自动调整、跳过原因）→ 生成；改模板后差异同步，手动改过的字段保留；节点标签绑定模板，新节点上线自动套用 |
| 安全 | 主控 ↔ Agent 全程 mTLS（内置 CA，首次接入用一次性 token + CA 指纹钉扎，可吊销）；后台路径自定义，其余路径 404 或反代伪装站；bcrypt + TOTP + 5 次失败锁 15 分钟；REALITY 私钥 / 证书私钥 / SS 主密钥 / API Token 以 AES-GCM 加密存库；订阅 token 32 字节、接口限速；Agent 只执行结构化指令；内核下载校验官方 SHA256；全部写操作记审计日志 |

## 快速开始

### 主控

```bash
cp deploy/polaris.env.example deploy/polaris.env   # 至少修改 POLARIS_SECRET_KEY、POLARIS_ADMIN_PATH、POLARIS_PUBLIC_URL
docker compose up -d
docker compose logs polaris | grep password         # 未设置初始密码时会生成并打印
```

前置 Caddy / Nginx 处理 HTTPS（示例见 `deploy/Caddyfile`），访问 `https://panel.example.com/<admin_path>/`。
`9443` 是 Agent 的 mTLS 接入端口，需要直接开放，不能经反代。

也可以单二进制运行：`make all` 后把 `bin/polaris` 放到 `/usr/local/bin`，配合 `deploy/polaris.service` 与 `/etc/polaris/polaris.env`。
把 `bin/polaris-agent-linux-*` 放进 `<data_dir>/downloads/`，安装脚本即可从主控直接下载 Agent。

### 节点

后台「节点与内核 → 添加节点」会生成一行命令，在节点上以 root 执行：

```bash
curl -fsSL https://panel.example.com/install.sh | bash -s -- \
  --panel https://panel.example.com:9443 --token <一次性 token> --ca-sha256 <CA 指纹>
```

脚本会安装 Agent、注册拿到客户端证书、写入 systemd 服务；Agent 按期望状态下载内核到 `/var/lib/polaris/cores/{type}/{version}/` 并校验 SHA256，切换版本只改 `current` 软链。
国内机器可加 `--mirror https://<github 镜像前缀>`。

> sing-box 官方构建不含 `with_v2ray_api`，此时 Agent 会跳过 sing-box 的按用户流量统计（Xray、Hysteria2 不受影响）。需要统计时请使用带该编译标签的构建：把二进制放到 `/var/lib/polaris/cores/singbox/<版本>/sing-box`（已存在时 Agent 不会重新下载），或用 `--mirror` 指向托管该构建的镜像。

### 配置项

均可通过环境变量（`POLARIS_*`）或 `--config config.yaml` 设置：

| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `POLARIS_SECRET_KEY` | 列加密与会话签名密钥，**必填且需备份** | — |
| `POLARIS_ADMIN_PATH` | 后台与 API 前缀 | `/` |
| `POLARIS_PUBLIC_URL` | 订阅链接中的外部地址 | — |
| `POLARIS_LISTEN` / `POLARIS_AGENT_LISTEN` | HTTP / Agent mTLS 监听 | `:8080` / `:9443` |
| `POLARIS_AGENT_HOSTS` | Agent 连接主控用的域名或 IP（写入服务端证书） | — |
| `POLARIS_DATA_DIR` | 数据目录（SQLite、CA、Agent 下载） | `./data` |
| `POLARIS_DB_DSN` | `postgres://…` 切换 PostgreSQL | SQLite |
| `POLARIS_REDIS_URL` | 流量计数缓冲 | 内存 |
| `POLARIS_DECOY` | 非后台路径反代到的伪装站 | 返回 404 |
| `POLARIS_SUB_RATE_LIMIT` | 订阅接口每 IP 每分钟请求数 | 30 |

ACME（DNS-01，Cloudflare）在后台「设置」中填写邮箱与 DNS Token 后启用，证书到期前 30 天自动续期并下发到使用它的节点。

## 开发

```
cmd/panel  cmd/agent           两个二进制
api/proto                      主控 ↔ Agent 的 gRPC 定义（connect-go，buf generate）
internal/model                 统一协议模型与能力矩阵
internal/core/{xray,singbox,hysteria,forwarder,mihomo}   内核适配器
internal/sub                   SubRenderer：links / clash / singbox / surge / 信息页
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

## 当前状态

已实现方案中的 M1–M6 主体功能。尚未完成 / 已知限制：

- mihomo 适配器仅占位（能力为空，不会被分配入站）；Hysteria2 官方服务端每节点只承载一个入站
- Xray 的 Hysteria2 入站未开放（不同版本配置差异较大），Hy2 请使用 sing-box 或官方服务端
- 「Agent 内置 Caddy 统一回源」开关已建模，回源逻辑尚未实现
- 链路健康检查为逐跳 TCP 探测 + 落地 HTTP 探测；落地故障转移依赖内核自身的 urltest / balancer
- Surge 之外的 Loon / Quantumult X 目前回落为通用链接
- 订阅模板使用 Go text/template，按格式各维护一份默认模板
