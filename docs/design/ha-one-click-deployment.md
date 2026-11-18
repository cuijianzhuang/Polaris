# 面板高可用一键部署

> 状态：设计稿，待定。目标是把《高可用部署文档》（`~/workspace/your_files/polaris-ha-deployment.md`）里的手工步骤，变成主节点一条命令 + 备节点一条命令。
>
> 一句话方案：**`install-panel.sh ha-init`（dmit 跑一次）+ 面板“高可用”页生成加入命令 + `install-panel.sh ha-join <token>`（HK 跑一次）**。故障切换保持半人工（DNS 自动切，数据库提主人工做），见 §1。

## 1. 目标与非目标

### 目标

- 一条命令初始化主节点：WireGuard 隧道、PG 流复制、Redis 主从、面板走隧道连库、生成一次性加入 token。
- 一条命令加入备节点：建 PG 从库、Redis 从库、装同版本面板、同步密钥、装反代 + 证书、装 DNS 看门狗、自检汇报。
- 面板里有“高可用”页：看得到主备角色、双实例在线、复制延迟；能生成加入命令；故障切换有向导（检查 + 命令，不代劳执行）。
- 全程可重跑、可 dry-run、可回滚到单机。

### 非目标（v1 刻意不做）

- PG 自动提主、面板里点按钮提主。双机无仲裁，自动提主有脑裂风险；提主永远是人确认“主真的死了”之后再做。
- DNS 自动回切。回切人工做，防抖动。
- 3 个节点。v1 只支持 1 主 1 备；数据表留扩展位。
- 同步复制默认开启。默认异步；向导里给开关，见 §9。
- SQLite 支持。HA 要求 PG；SQLite 的库先用 `polaris migrate` 迁到 PG。
- 托管 PG/Redis。只覆盖自建的两台机器。

### 与“多机管理”（Panels 页）的关系

那是另一功能：管理多个**独立**面板（跨站）。HA 是**同一个**面板集群的两个实例。HA 页单独开路由 `/ha`，不复用 Panels 页，只复用它的表格/抽屉写法。

## 2. 现状与复用点

| 现有能力 | 位置 | 本设计怎么用 |
|---|---|---|
| 多实例：leader 租约、cluster bus、成员表 | `internal/panel/cluster.go`、`clustermembers.go` | 不用改。HA 页直接读 `cluster_members` / `cluster_leases` 显示双实例和 leader |
| CA 存在库里，JWT 无状态 | `server.go:loadOrCreateCA`、`auth.go` | 两实例共用 CA、登录态不用粘滞会话；已满足 |
| 节点安装 token：hash 存库 + 1 小时过期 + 一次性 | `admincli.go:LocalNodeToken`、`api_nodes.go:newInstallToken` | 加入 token 照抄这个模式：`pki.Token(24)`、`token_hash`、`token_expires` |
| `polaris` CLI：admin/backup/restore/migrate/node-token | `cmd/panel/main.go` | 加 `polaris ha-token`（发加入 token）和 `polaris ha sync-wg`（对齐 WG 对端） |
| 安装脚本的整套现成函数 | `scripts/install-panel.sh`（1950 行） | `preflight`、`env_get`/`env_set`、`pg_su`、`setup_local_pg`、`setup_local_redis`、`wait_healthy`、`dl`、`check_pg_target` 全部复用；ha-init / ha-join 只新增流程函数 |
| 敏感值密封进库 | `ClusterSecret` + `Box.Seal`（CA 密钥、`RefundJob.BotToken` 同款） | CF token、复制账号密码、Redis 密码密封存 `cluster_secrets`，不进明文 settings |
| 面板写意图、root 侧执行 | `cluster_operations.go` 的 `writeUpdateRequest` | WG 对端同步走同一思路：面板只写 `ha_peers` 行，root 定时器执行 `wg set`（见 §5.4） |
| 集群更新 | `topicUpdate` + `polaris update-request` | 版本升级沿用现有机制；HA 不另起一套 |

### 一条硬边界

面板进程内**不直接执行系统命令**（现代码无 `os/exec` 引用）。所有需要 root 的系统操作只出现在 shell 脚本与 CLI 里；Go 代码只读写自己的库和配置。这条边界本设计不打破。

## 3. 总体方案

```
dmit（主）                                        HK（备）
┌─────────────────────────────────┐               ┌─────────────────────────────────┐
│ install-panel.sh ha-init        │  token + 配置包 │ install-panel.sh ha-join <token>│
│  - WG 密钥对、起隧道            │ ────────────▶ │  - 验 PG 大版本                 │
│  - PG：wal_level、复制账号/槽   │  (HTTPS,一次) │  - WG 密钥对、注册对端、起隧道    │
│  - Redis：密码、绑隧道          │               │  - pg_basebackup 建从库          │
│  - 面板 env 改走隧道、重启      │               │  - Redis 配从库                  │
│  - polaris ha-token 发 token    │               │  - 装面板（复用 --db/--redis）    │
└─────────────────────────────────┘               │  - 同步 env（SECRET_KEY 等）     │
        ▲                                         │  - nginx + certbot DNS-01        │
        │ 面板“高可用”页（/ha）                    │  - 装 DNS 看门狗、备份同步       │
        │  状态、token、向导                       │  - 自检汇报                     │
        └─────────────────────────────────────────┘
```

### 角色状态机

`ha.role`（settings）：`none` → `primary`（ha-init 后）/`secondary`（ha-join 后）。两个值只由脚本写，面板只读。

- 平时：双面板进程都在跑，连同一个 PG 主库；leader 租约自动选主（已有机制）。
- dmit 挂：看门狗（HK 上）3 分钟内把 DNS 切到 HK；人按向导提主 PG/Redis、改 HK 面板 env、重启面板。
- dmit 回来：按向导重建成从库；可保持 HK 为主，也可再对调回去。

## 4. 数据模型

### 本机身份（env 变量，不进 settings）

PR2 实现时发现：`settings` 表是全集群共享的一行数据，主备两台读的是同一份，
放不下“每台不一样”的值（角色、WG 地址、公网地址）。所以 PR1 设计里的
`ha.role` / `ha.wg_pubkey` / `ha.wg_ip` / `ha.endpoint` / `ha.peer_wg_ip`
**不写 settings，改放各机自己的 env 文件**（`POLARIS_HA_*`，`internal/panel/config.go`）：

| env 变量 | 内容 |
|---|---|
| `POLARIS_HA_ROLE` | `primary` / `secondary`（空 = 未加入 HA） |
| `POLARIS_HA_WG_PUBKEY` | 本机 WG 公钥 |
| `POLARIS_HA_WG_IP` | 本机 WG 地址（主 `10.8.0.1`，备 `10.8.0.2`） |
| `POLARIS_HA_ENDPOINT` | 本机公网 IP（WG 对端地址；**不用域名**，DNS 切走后备节点的隧道对端不能跟着变） |
| `POLARIS_HA_PEER_WG_IP` | 对端 WG 地址 |

`ha.pg_version` 不需要存：join-config 组装时现查 `SHOW server_version`
（`haJoinConfigBundle`）。`haRole()` 与配置包都改读 `cfg` 里的这几个字段，
不再读 settings。

### cluster_secrets（Box 密封，只放敏感信息）

| name | 内容 |
|---|---|
| `cf_api_token` | Cloudflare API token（Zone:DNS:Edit，仅 `cuijianzhuang.com` 这个 zone） |
| `ha_repl_password` | PG 复制账号密码（init 生成的随机值） |
| `ha_redis_password` | Redis 密码（init 生成的随机值） |

WG **私钥只留在各自主机的 `/etc/wireguard/privatekey`，永不进库、不进配置包**。

### 新表 ha_join_tokens

| 列 | 说明 |
|---|---|
| `token_hash` | `hashToken(pki.Token(24))`，只存 hash |
| `created_by` | `cli` 或管理员账号 |
| `created_at` / `expires_at` | 有效期 10 分钟 |
| `used_at` | 对端注册成功时打时间戳；之后 token 失效 |
| `used_by_wg_ip` | 注册的备节点 WG 地址 |

### 新表 ha_peers

| 列 | 说明 |
|---|---|
| `wg_ip` | 对端 WG 地址，主键 |
| `pubkey` | 对端 WG 公钥 |
| `endpoint` | 对端公网地址 |
| `role` | `primary` / `secondary` |
| `created_at` / `last_seen` | 注册时间 / 最近一次 join-config 交互 |

### 状态聚合（HA 页读这些）

- 实例列表：`SELECT * FROM cluster_members`（已有字段：`instance_id`、`member_id`、`host`、`version`、`edition`、`started_at`、`seen_at`）。
- Leader：`SELECT holder FROM cluster_leases WHERE name='leader'`。
- 主库复制延迟：`SELECT client_addr, state, sync_state, pg_wal_lsn_diff(sent_lsn, replay_lsn) FROM pg_stat_replication;`。
- 从库回放延迟：`SELECT now() - pg_last_xact_replay_timestamp() AS lag;`。
- Redis：`INFO replication`（主从角色、链接状态），面板启动时已连 Redis，状态页直接读共享态的 Redis 客户端；连不上就报红。

## 5. 接口设计

路径相对于面板 `APIBase`（默认 `/api/v1`；自定义管理路径时为该路径加 `/api/v1`）。管理端接口进 `router.go` 的 `p` 组（`authRequired` + `adminRoleGuard`），敏感操作再加 `requireSudo`（与 `POST /admins` 同款）。

### 5.1 管理端 API

| 方法与路径 | 鉴权 | 用途 |
|---|---|---|
| `GET /ha/status` | 管理员 | 角色、实例列表、leader、复制延迟、Redis 状态、看门狗状态（备机）、版本一致性 |
| `POST /ha/token` | sudo 管理员 | 发一个加入 token（10 分钟有效），返回 token 和完整加入命令；旧 token 同时作废 |
| `POST /ha/cf-token` | sudo 管理员 | 保存 CF token（密封进 `cluster_secrets`）；正文 `{"token": "..."}`，永不回显 |
| `GET /ha/peers` | 管理员 | `ha_peers` 列表 |
| `DELETE /ha/peers/:wg_ip` | sudo 管理员 | 移除对端（下线备节点用；只删库记录，WG 配置由 sync-wg 对齐） |
| `POST /ha/failover-check` | 管理员 | 只读：主节点是否可达、从库回放延迟、DNS 当前指向、待执行的提主步骤清单（给向导用，不执行任何写操作） |

### 5.2 加入专用 API（token 鉴权，不走管理员登录）

| 方法与路径 | 鉴权 | 用途 |
|---|---|---|
| `GET /ha/join-config` | `Authorization: Bearer <token>` | 取配置包（§6.2）。token 未使用且未过期才能取；可重复取，直到对端注册成功 |
| `POST /ha/peers` | `Authorization: Bearer <token>` | 注册备节点对端：正文 `{"wg_ip","pubkey","endpoint"}`；成功后 token 标记 `used_at`，后续 `join-config` 不可再取 |

token 鉴权失败记审计、不透露是“不存在”还是“已过期”。`join-config` 加每 IP 限流，复用现有共享限流器（`sharedLimiter`，name 取 `ha-join`）。

### 5.3 CLI

| 命令 | 用途 |
|---|---|
| `polaris ha-token` | 发加入 token（照抄 `node-token` 的写法：`LocalNodeToken` 同款，只是不建节点行）。ha-init 脚本调它；管理员也能在终端直接跑 |
| `polaris ha-seal-secret <name>` | **PR2 新增**：从 stdin 读值（不进 argv，不进 ps 输出），用 Box 密封后存入 `cluster_secrets`（复用 `setHAClusterSecret` 的逻辑，抽成 `setHAClusterSecretDB` 供 CLI 与 HTTP 共用）。ha-init 调它存 `ha_repl_password` / `ha_redis_password`（以及有 CF token 时的 `cf_api_token`） |
| `polaris ha sync-wg` | 读 `ha_peers`，对齐本机 WG 配置（`wg set` 加对端 / 删已移除的对端；同时重写 `wg0.conf` 保证重启后还在）。由 systemd timer 每 60 秒跑，只在主节点上启用 |
| `polaris witness serve [--listen ADDR] [--state-file PATH]` | **PR5 新增**：见证节点服务（maintenance 子命令，见 §13）。内存记两台心跳（可选落盘 JSON），无数据库；`--listen` 默认 `0.0.0.0:8443` |

### 5.4 安装脚本

| 命令 | 用途 |
|---|---|
| `install-panel.sh ha-init [--dry-run]` | §6.1 |
| `install-panel.sh ha-join <token> <panel-url> [--dry-run]` | §6.3 |
| `install-panel.sh ha-join --finish [--dry-run]` | §6.3：只补证书与看门狗，不带 token；CF token 从环境变量 `CF_API_TOKEN` 取 |
| `install-panel.sh ha-status` | 本机 HA 状态速查（角色、WG、PG、Redis、面板），给排障用 |
| `install-panel.sh witness-init [--dry-run] [--listen ADDR]` | **PR5 新增**：在第三台主机上装见证节点（§13）：装面板二进制（即 witness 二进制）、写 `polaris-witness.service`、打印防火墙与验证命令 |

`--dry-run` 只打印将要执行的动作，不改任何东西；review 和 CI 都靠它。
`ha-init` 支持 `--endpoint <IP>`（公网 IP 探测不到时用，见 §6.1 前置检查）。
`ha-init` / `ha-join` 支持环境变量 `WITNESS_URL`（见证节点地址，如 `http://<见证IP>:8443`，**PR5 新增**，见 §13）：配了就装心跳定时器（ha-init 还装围栏），没配就警告并跳过——见证节点可以晚于 ha-init/ha-join 再装，装好后用同一个 `WITNESS_URL` 重跑 ha-init/ha-join 补装。

## 6. 详细流程

### 6.1 ha-init（dmit 上，root 跑一次）

前置检查（任一项失败即退出，不改东西）：

| 检查 | 失败处理 |
|---|---|
| 已安装面板且版本已知（`polaris --version`） | 未安装先正常装面板 |
| 面板二进制包含 HA 命令（`ha-token` / `ha-seal-secret` / `ha sync-wg`，从 usage 文本里 grep） | **PR2 新增**：不包含时退出并提示先 `polaris update`。旧版二进制收到未知子命令会直接启动第二个面板进程，绝不能盲调 |
| 库是 PostgreSQL（DSN 以 `postgres://` 开头） | SQLite 时报错，提示先 `polaris migrate` |
| `ha.role` 不是 `primary`（防重复初始化） | 已是 primary 时只打印状态 + 发新 token；**PR5 补充**：已是 primary 时带 `WITNESS_URL` 重跑 ha-init，会补装心跳定时器与围栏（见 §13） |
| PG 大版本可读 | 读不到退出 |
| `51820/udp`、`6379` 未被 WG/Redis 以外进程占用 | 占用则退出并指明进程；PG 端口取本机集群实际值（常为 `5432`） |
| 磁盘剩余空间 | 小于库大小 × 1.2 时警告（备机也要装得下整个库） |
| 能探测到本机公网 IP（`curl ifconfig.me`，脚本已有此逻辑） | 探测不到则要求 `--endpoint <IP>` 传入 |
| DNS：`polaris` 记录 TTL ≤ 60 且为 DNS-only | 没配 CF token 时只警告（人工去改）；有 token 时调 API 查并提示 |

执行步骤（按顺序，每步可单独重跑）：

1. 生成 WG 密钥对（`/etc/wireguard/privatekey` 0600），写 `wg0.conf`（`10.8.0.1/24`，`ListenPort 51820`），`systemctl enable --now wg-quick@wg0`。
2. 生成随机密码（复制账号、Redis），密封存 `cluster_secrets`（`ha_repl_password`、`ha_redis_password`）。
3. PG：`postgresql.conf` 加 `wal_level=replica`、`max_wal_senders=5`、`max_replication_slots=5`、`wal_keep_size=1GB`、`listen_addresses` 加 `10.8.0.1`；`pg_hba.conf` 加隧道网段的 `replication` 与 `polaris` 账号规则；`CREATE USER replicator WITH REPLICATION`；`pg_create_physical_replication_slot('hk_slot')`；重启 PG。
4. Redis：`bind 127.0.0.1 10.8.0.1`、`requirepass`、`appendonly yes`；重启 Redis。
5. 面板 env：`POLARIS_DB_DSN` host 改 `10.8.0.1`、`POLARIS_REDIS_URL` 改 `redis://:<密码>@10.8.0.1:6379/0`；`systemctl restart polaris`；`wait_healthy` 确认 `/api/v1/info` 200。
   - **PR2 偏差**：dmit 现用 Unix socket peer 认证，`polaris` 角色本来没有密码，DSN 走 TCP `10.8.0.1` 必须先 `ALTER USER polaris PASSWORD`（第 3 步里做，随机值）。§6.2 说的“业务密码就是主节点 env 里现成的那个”只在 DSN 本来就是 TCP 时成立。
6. 写本机 env（`POLARIS_HA_ROLE=primary`、`POLARIS_HA_WG_PUBKEY`、`POLARIS_HA_WG_IP=10.8.0.1`、`POLARIS_HA_ENDPOINT=<公网IP>`）——**PR2 偏差**：原设计写 settings，见 §4，settings 放不下每台不一样的值。
7. 启用 `polaris-ha-sync.timer`（每 60 秒跑 `polaris ha sync-wg`）。
8. **PR5 新增**：安装见证心跳定时器（`polaris-ha-heartbeat.timer`，每 10 秒 `curl POST <WITNESS_URL>/heartbeat -d '{"node":"dmit"}'`）。`WITNESS_URL` 未配时警告并跳过，不阻塞 init；见证节点就绪后重跑 ha-init 补装。
9. **PR5 新增**：安装围栏检查（`polaris-ha-fence.service` oneshot + `polaris.service.d/ha-fence.conf` drop-in：`Requires` + `After`，面板启动前先跑；见 §13）。`WITNESS_URL` 未配时警告并跳过。
10. 调 `polaris ha-token` 发 token，打印加入命令：`install-panel.sh ha-join <token> https://polaris.cuijianzhuang.com`。
   - **PR2 补充**：join 命令里的 URL 要带上管理路径（`https://域名<管理路径>`），因为加入 API 在 `<管理路径>/api/v1` 下面；管理路径是 `/` 时不用带。ha-init 打印时自动拼好。

回滚：ha-init 只在主节点做加法（改配置、加账号、加槽），不删数据。任一步失败，已做的步骤保留，下次重跑跳过已完成项；彻底不要了就按手工文档第 12 节回滚（停备面板、DNS 保持指主，PG 从库停掉不影响主库）。

### 6.2 join-config 配置包

`GET /ha/join-config` 返回（token 鉴权）：

```json
{
  "panel_version": "v0.1.258",
  "edition": "pro",
  "public_url": "https://polaris.cuijianzhuang.com",
  "admin_path": "/",
  "timezone": "Asia/Shanghai",
  "secret_key": "<和主节点完全相同>",
  "secret_key_previous": "<有则给，没有为空>",
  "db_dsn": "postgres://polaris:<业务密码>@10.8.0.1:5432/polaris",
  "redis_url": "redis://:<密码>@10.8.0.1:6379/0",
  "pg_version": "15",
  "repl_password": "<复制账号密码>",
  "wg": {"pubkey": "<主公钥>", "endpoint": "<主公网IP>", "ip": "10.8.0.1", "port": 51820},
  "wg_ip": "10.8.0.2",
  "db_size_bytes": 123456789,
  "cf_api_token": "<有则给，没有为空>",
  "domain": "polaris.cuijianzhuang.com"
}
```

注意：

- `secret_key` 只经此接口取一次，且 token 用完即废；脚本拿到后写 `0600` 的 env 文件，**绝不打印到日志**（`--dry-run` 时把敏感字段打码）。
- `db_dsn` 里的业务密码就是主节点 env 里现成的那个，不新设。端口是主库 PG 集群的实际端口（Debian 集群被占时不在 `5432`，如 `5433`）：`ha-init` 写 DSN 时取 `SHOW port` 的值，`ha-join` 的 `pg_basebackup`/`.pgpass`/从库端口都取配置包里这个值；提主只改 host（`10.8.0.1`→`10.8.0.2`），不改端口，所以从库必须监听同一端口。
- 备机 PG 大版本与主库不同时，`ha-join` 从 PGDG 装主库的版本并用该版本集群建从库（旧版本集群若占了主库端口会被先停掉，数据保留在磁盘上）；本机任一集群已有 `polaris` 库时仍直接退出，须先备份清空。
- 也可以反过来对齐版本：`install-panel.sh pg-upgrade [--to <大版本>]` 把本机 PG 集群就地升级到指定大版本（`pg_upgradecluster -m upgrade`，新集群接管原端口，DSN 不用改；旧集群保留在原地，验证后 `pg_dropcluster` 删除）。不加 `--to` 时升到最新可装的大版本；不支持降级，带表空间的集群不能升，有从库正在流复制时拒绝执行。
- `db_size_bytes` 是 **PR2 新增**：join 时磁盘检查要用库大小，但 `/ha/status` 需要管理员登录、join token 调不动，所以直接放进配置包（PG 上 `SELECT pg_database_size(current_database())`，SQLite 下为 null）。
- `cf_api_token` 为空时，join 脚本跳过 certbot DNS-01 与看门狗安装并明确提示，备节点先以 HTTP 反代跑起来，等 token 补上再跑 `ha-join --finish`（见 §6.3）。

### 6.3 ha-join（HK 上，root 跑一次）

前置检查：

| 检查 | 失败处理 |
|---|---|
| Ubuntu/Debian + systemd + root | 不满足退出 |
| `80/443/8080/9443/51820` 端口 | `8080/9443/51820` 被占退出；`443` 被 OpenResty 占用时切复用模式（不装 nginx，反代自行添加，见 §6.3 步骤 6）；`443` 被节点入站占了时退出并提示先改入站端口（手工文档 §5.1 同款问题） |
| 磁盘剩余 ≥ 主库大小 × 1.2（配置包里的 `db_size_bytes`，**PR2 偏差**：原设计调 `GET /ha/status` 取，但那个接口要管理员登录、join token 调不动） | 不够退出 |
| 本机 PG 大版本 | 未装 PG 时从 PGDG 装与主库相同的大版本；已装但大版本不同时也装主库版本并用该版本集群建从库（旧版本集群若占了主库端口会被先停掉，数据保留在磁盘上）；也可以反过来用 `pg-upgrade` 把主库升级到备机版本（见 §6.3） |
| 面板未安装或 `ha.role` 不是 `secondary` | 已是 secondary 时只做增量收尾（见下）；**PR5 补充**：带 `WITNESS_URL` 重跑时补装心跳定时器（见 §13） |

执行步骤：

1. 生成本机 WG 密钥对，**先 `GET /ha/join-config` 取配置包，再 `POST /ha/peers` 注册**（**PR2 修正**：原设计先注册后取，但注册会消费 token，取配置包失败就把 token 烧了；token 在注册前可重复取）。备机的 WG 地址用配置包的 `wg_ip`（主节点分配，默认 `10.8.0.2`）。
2. 写本机 `wg0.conf`（`10.8.0.2/24`，对端为主），`systemctl enable --now wg-quick@wg0`；`ping 10.8.0.1` 不通则退出。
3. 停本地 PG，清空数据目录，`pg_basebackup -h 10.8.0.1 -U replicator -D <数据目录> -Fp -Xs -P -R -S hk_slot`；配 `hot_standby=on`、`listen_addresses=localhost,10.8.0.2`；启动；验 `pg_is_in_recovery()=t` 且回放追上。**`.pgpass 留在备机**（`/var/lib/postgresql/.pgpass`，0600，postgres 所有）：`-R` 不把密码写进 `primary_conninfo`，从库重连靠它。
4. 装 Redis，配 `replicaof 10.8.0.1 6379` + `masterauth`/`requirepass`；启动；验 `role:slave`、`master_link_status:up`。
5. 调现有安装流程装面板：`--pro --version <配置包版本> --domain <domain> --no-caddy --db <db_dsn> --redis <redis_url> --port 8080 --agent-port 9443`，随后把 env 里的 `SECRET_KEY` / `SECRET_KEY_PREVIOUS` / `ADMIN_PATH` / `PUBLIC_URL` / `TIMEZONE` 按配置包改好，`systemctl restart polaris`。**PR2 补充**：安装时 `SECRET_OVERRIDE` 用配置包的密钥（不重新生成，否则 CA 密封用的密钥对不上）；全新库检查改成只验连通与编码（主库里已有管理员）；首次登录信息不打印（管理员沿用主节点的）。
6. 反代（二选一）：`443` 空闲时走 nginx——写 443 反代 + `stream` 段 9443 直通（复用手工文档 §6.2 的配置）；certbot DNS-01（有 CF token 时）拿证；`nginx -t` 通过后启动；用 `--resolve` 直接验备机 `https://<domain>/api/v1/info` 200。**PR2 补充**：没 CF token 时 nginx 配置照写、暂不启用，等 `--finish` 拿证后再启用。**本次新增**：`443` 已被 OpenResty（如 1Panel 装的）占用时切复用模式——不装 nginx、不申请证书，脚本打印出需在 1Panel 里加的反代配置（443→8080、stream 9443 直通），模式记入 `POLARIS_HA_PROXY_MODE`，`--finish` 时只补看门狗；模式由 `HA_PROXY_MODE`（auto/nginx/openresty，默认 auto）控制，auto 按 443 占用情况自动判断。
7. 装看门狗：把 `scripts/cf-failover.sh` 拷到 `/usr/local/bin/`，按配置包填 token/zone/record（zone 与 record ID 由脚本调 CF API 现查，不用人填）；写 cron 每分钟跑；只装在备机。**PR2 补充**：`install-panel.sh` 里内嵌了一份看门狗脚本（`ha_watchdog_script`），ha-join 装的是内嵌的那份，不是从 release 资产下载；`scripts/test-ha-dryrun.sh` 断言内嵌副本与 `scripts/cf-failover.sh` 字节一致，防止漂移。**PR5 补充**：看门狗 env 里同时写 `WITNESS_URL`（ha-join 时环境变量给），看门狗切 DNS 前按 §13 的三条件规则问见证节点。
8. **PR5 新增**：安装见证心跳定时器（与主节点同款，只是 `node` 为 `hk`）。`WITNESS_URL` 未配时警告并跳过；见证节点就绪后重跑 ha-join 补装（已是 secondary 时只做增量收尾，见 §6.3 前置检查表）。
9. 备份同步 cron：每小时 rsync `/var/lib/polaris/backups/` 到对端（走 WG）。**PR2 补充**：只在免密 SSH 已通时装（先测 `ssh -o BatchMode=yes root@<对端> true`），不通则警告并给出换 key 的命令，跳过 cron。
10. 自检：WG、PG 复制、Redis、双实例（`cluster_members` 两行）、备机直验 200，逐项打勾打印；任一项失败即报错退出，不静默通过。
11. 写本机 env（`POLARIS_HA_ROLE=secondary` 等）——**PR2 偏差**：原设计写 settings，见 §4；实际在第 5 步的 env 同步里一起写好。

`ha-join --finish`：补装第 6、7 步（CF token 后补的场景），其余步骤检测到已完成就跳过。**PR2 补充**：`--finish` 不带 token（`ha-join --finish [--dry-run]`），CF token 从环境变量 `CF_API_TOKEN` 取（主节点上密封的那份备机读不回来）；主节点公网 IP 默认从域名当前 A 记录取，可用 `CF_PRIMARY_IP` 覆盖。

### 6.4 sync-wg 对端同步

- 触发：备机注册成功后，主面板在 `ha_peers` 里多一行；`polaris-ha-sync.timer`（主节点，60 秒）跑 `polaris ha sync-wg`。
- 行为：读 `ha_peers`，与 `wg show` 的当前对端比对；多的执行 `wg set wg0 peer <pubkey> allowed-ips <wg_ip>/32 endpoint <endpoint>:51820`，少的执行 `wg set wg0 peer <pubkey> remove`；随后重写 `wg0.conf`（保证重启后对端还在）。
- 备机上的 timer 不启用（备机的对端只有主，join 时已写死）。

### 6.5 看门狗

`scripts/cf-failover.sh` 进仓库，由 ha-join 安装（内容与手工文档 §7.2 一致）：每分钟验主站公网 443 的 `/api/v1/info`，连续 3 次不通且本机面板健康，才调 CF API 把 `polaris` 的 A 记录切到备机 IP；只单向切，不自动回切；日志写 `/var/log/cf-failover.log`，计数写 `/var/lib/cf-failover.state`。

**PR2 补充**：

- 参数用环境变量传入（也可写 `/etc/cf-failover.env`，0600），不 hardcode 任何 token、IP、域名。
- `CF_ADMIN_PATH`：管理路径前缀（默认 `/`）。API 在 `<管理路径>/api/v1` 下面，探活 URL 要拼上它；管理路径是 `/` 时与手工文档 §7.2 的写法完全一致。
- ha-join 装的是 `install-panel.sh` 里内嵌的副本（见 §6.3 第 7 步），`scripts/test-ha-dryrun.sh` 断言两者字节一致。
- 测试用环境变量 `CF_STATE_FILE` / `CF_LOG_FILE` 覆盖 state 与日志路径（只给测试用，安装时永远用默认路径）。

**PR5 补充（三条件规则，见 §13）**：切 DNS 之前除本机连续 3 次不通与本机面板健康外，还要满足：见证节点也报主节点不可达（`GET <WITNESS_URL>/verdict?node=dmit` 的 `reachable=false`），且见证节点本身可达（问 verdict 成功本身即证明）。任一不满足不切，只记日志；见证节点不可达时同样不切（宁可不切，不可错切）。`WITNESS_URL` 未配时保持旧行为，并在日志写明未配置见证节点。DNS 翻转成功后，看门狗 best-effort 向见证节点 `POST /promoted {"node":"hk"}`（失败只记日志，不影响已翻转的 DNS），供 dmit 恢复时的围栏（§13）判断。

### 6.6 故障切换向导（HA 页，人工执行）

向导只做两件事：**实时检查**与**给出命令**，不代劳执行。

1. 检查：主节点是否可达（调 `POST /ha/failover-check`，面板从备机视角验主站 443）；从库回放延迟（秒）；DNS 当前指向；备机面板是否健康。
2. 每步给一条可复制的命令（提主 PG → 提主 Redis → 改 env → 重启面板 → 验 DNS → 验业务），做完一步勾一步；命令内容与手工文档 §9 一致。
3. 明确提示 RPO：异步复制，故障瞬间可能丢几秒写入。

### 6.7 故障恢复（dmit 回来）

向导给重建从库的命令（停 PG → `pg_basebackup` 从备机拉 → 起为从库），与手工文档 §10 一致。两个选择写清楚：保持备机为主（推荐，什么都不用再改）或对调回去（反向再做一遍提主流程）。

**PR5 补充（围栏）**：dmit 恢复后不能直接以主库身份启动——ha-init 装的围栏（`polaris-ha-fence.service`，在 `polaris.service` 之前跑）会问见证节点 `GET /primary`；若见证节点记着 `primary=hk`，围栏直接退出非 0，面板起不来，并打印“HK 已提主，按文档把本机重建成从库”的提示。恢复流程因此是：先按向导把 dmit 重建成从库（`pg_basebackup` 从 HK 拉），确认见证节点的心跳恢复后，再按 §6.7 的对调流程决定是否切回。见证节点不可达时围栏只警告、放行启动（可用性优先），这个角落情况见 §13 故障模式表。

## 7. 安全设计

- **加入 token**：`pki.Token(24)`，只存 hash，10 分钟过期，对端注册成功即作废；创建与使用都记审计（`ha.token`、`ha.peer_register`）；创建要 sudo 管理员。
- **配置包传输**：只走 HTTPS，脚本默认校验证书，不提供 `--insecure`；token 只在 `Authorization` 头里传，不进 URL。
- **SECRET_KEY**：只在配置包里出现一次；脚本写盘 `0600`，日志里打码；面板日志里不记录配置包内容。
- **CF token**：密封存库；只在两个地方出现——备机 `/root/.cloudflare.ini`（0600）和 certbot/看门狗调用；权限为单个 zone 的 `Zone:DNS:Edit`。
- **复制与 Redis 密码**：init 生成随机值，密封存库；只在配置包里给 join 脚本；PG 侧 `pg_hba.conf` 只放行隧道网段。
- **WG 私钥**：各主机本地生成，永不离机、永不进库。
- **审计**：`ha.init`、`ha.token`、`ha.peer_register`、`ha.cf_token`、`ha.peer_remove` 全记审计日志；join-config 的每次访问记 `ha.join_config`（不记 token 明文）。**PR2 说明**：脚本侧的 init 由 `ha.token`（`polaris ha-token` CLI 已记）与 `ha.seal_secret`（`ha-seal-secret` CLI 已记）两条审计覆盖；独立的 `ha.init` 审计条目随 PR3 的 HA 页/接口一起补（脚本没有写审计的 CLI）。

## 8. 前置检查总表

| 检查 | 位置 | 不通过 |
|---|---|---|
| 库是 PG | ha-init | 退出，提示 `polaris migrate` |
| 面板二进制包含 HA 命令 | ha-init | 退出，提示先 `polaris update`（PR2 新增，见 §6.1） |
| PG 大版本可读 | ha-init / ha-join | 退出 |
| 本机公网 IP 可探测 | ha-init | 要求 `--endpoint <IP>` |
| DNS：TTL ≤ 60 且 DNS-only | ha-init（有 CF token 时调 API 查） | 警告，继续（人工去改） |
| 端口 8080/9443/51820 空闲 | ha-join | 退出 |
| 端口 443 空闲，或被 nginx/OpenResty 占用 | ha-join | 被节点入站等其他进程占用时退出并提示改入站端口；被 OpenResty 占用时切复用模式（不装 nginx，打印 1Panel 反代配置） |
| 磁盘 ≥ 库 × 1.2 | ha-join | 退出 |
| 两台版本一致 | ha-join（配置包版本 vs 本机将装的版本） | join 装配置包里的版本，不追最新 |
| 时间同步 | 两边都查 | 警告（租约时间以库为准，偏一点没事） |

## 9. 待定点（设计时已选默认，见 §12 请用户拍板）

- 复制默认异步；同步复制做成向导里的可选项（`synchronous_standby_names='hk_slot'`，代价是每次写库多一次往返延迟）。
- 看门狗只装备机；备机也挂则不切（可接受：主还活着）。
- 备份同步默认开启（rsync over WG，每小时）。
- 升级：先升备再升主，沿用现有 `update-request` 机制；两台版本短暂不一致可以接受，别拖太久。

## 10. 测试计划

- **Go 单元测试**：token 生成/校验/过期/消费（复用现有 panel 测试的 store 夹具）；配置包组装（断言敏感字段不在日志里）；`sync-wg` 的对端差集计算（纯函数）；状态聚合的 SQL 在测试库上跑通。
- **Shell**：给 `scripts/` 加 shellcheck 到 CI（现在 CI 只跑 `bash scripts/test-install-ctl.sh`）；ha-init / ha-join 的 `--dry-run` 输出做快照测试，沿用 `test-install-ctl.sh` 的夹具方式。**PR2 已做**：CI 的 `mirror-tests` job 跑 `shellcheck -S warning`（四份脚本）+ `bash scripts/test-ha-dryrun.sh`（ha-init / ha-join / `ha-join --finish` 的 dry-run 步骤顺序与敏感字段打码 + dry-run 无文件改动 + 看门狗 5 个场景 + 内嵌看门狗副本一致性）。
- **手工演练**：部署文档 §8 验证清单 + §9 故障切换，在真机上跑一遍；本地单机双 PG 端口模拟只作为可选补充，不阻塞交付（开发机无 Docker）。
- **回归红线**：全仓测试通过才能提交 PR；安装脚本改动必须跑 `test-install-ctl.sh`。

## 11. 分批交付

| 批 | 内容 | 验收 |
|---|---|---|
| PR1 | 数据模型（`ha_join_tokens`、`ha_peers`）+ `polaris ha-token` + 管理端 API（§5.1/§5.2）+ CF token 密封保存 + 单元测试 | token 全流程测试过；API 只有 sudo 能发 token |
| PR2 | `ha-init` + `ha-join` + `ha sync-wg` + `cf-failover.sh` 进仓库 + `--dry-run` + shellcheck | dry-run 输出与手工文档步骤一一对应；真机跑通一次 |
| PR3 | HA 页面（`/ha`）+ 故障切换向导 + 状态聚合 | 页面数据与 `cluster_members` / `pg_stat_replication` 一致 |
| PR4 | user-guide 加 HA 一节；手工部署文档降为附录（故障恢复手册） | 文档里的命令与脚本实际行为一致 |
| PR5 | 仲裁节点（§13）：`polaris witness serve` + 心跳 timer + 看门狗三条件规则 + 围栏 + `witness-init` | 见证服务心跳→裁决→提主记录实测过；看门狗 4 个见证场景测试过；dry-run 步骤顺序与内嵌副本一致性测试过 |

## 12. 待用户拍板的问题

> 2026-11-18 用户已定：Q1 接受人工提主/回切；Q2 CF token 面板 UI 里填；Q3 同步复制做成选项。Q4–Q6 暂按默认执行，有异议再改。

1. **提主与回切永远人工**，接受吗？（要自动提主得引入第三方仲裁或接受脑裂风险，v1 不做。）——**已定：接受。**
2. **CF token 由谁填**：面板 UI 输入（推荐，密封进库，join 时自动下发）还是 ha-init 命令行参数（只落备机本机，不进库）？——**已定：面板 UI 里填。**
3. **同步复制**要不要做成向导里的可选项？（默认异步；开了则每次写库多一次 dmit↔HK 往返延迟。）——**已定：要，做成选项。**
4. **第 3 个节点**以后再说？（v1 只支持 1 主 1 备。）
5. **看门狗只装备机**，接受吗？（备机也挂则不切；主还活着所以业务不受影响。）
6. **备份同步默认开启**，接受吗？（每小时 rsync 走 WG 互相同步。）

## 13. 仲裁节点（见证节点，PR5）

### 13.1 为什么需要

两台节点分不清“主死了”还是“网断了”：HK 连不上 dmit，可能是 dmit 挂了，也可能是 dmit↔HK 之间的链路断了（这时 dmit 还在正常服务用户）。只凭 HK 自己的探测就切 DNS，第二种情况会造成双主——DNS 指到 HK，dmit 还在写库，两边数据分叉。第三台只投票不存数据的见证节点打破这个僵局：它在独立的故障域（另外商家，与 dmit/HK 的搬瓦工不同），HK 切 DNS 前必须拿到它的同意。

这不是 §12 Q4 说的“第 3 个节点”——Q4 指第 3 台面板节点（跑面板、存数据），见证节点不跑面板、不存数据、不参与复制，所以 PR5 不改变 Q4“以后再说”的结论。也不做 Patroni/etcd：太重，v1 不需要。

### 13.2 见证服务

复用 panel 二进制，不新开二进制、不改发版流程：

```
polaris witness serve [--listen ADDR] [--state-file PATH]
```

- `--listen` 默认 `0.0.0.0:8443`；只绑公网 IP，不走 WG（见证节点不在隧道里）。
- 内存记两台的心跳（`lastSeen`），可选落盘一个小 JSON 文件（`--state-file`，原子写 tmp+rename，0600），重启后不丢；无数据库。节点名只认 `dmit` / `hk`，其余 400。
- 接口（纯标准库 HTTP）：

| 方法与路径 | 作用 |
|---|---|
| `POST /heartbeat {"node":"dmit"\|"hk"}` | 记一次心跳，返回 200 |
| `GET /verdict?node=dmit` | 返回 `{"reachable":bool,"lastSeen":RFC3339\|null,"secondsSince":number\|null}`；`reachable` 指最近 30 秒内收到过该节点心跳 |
| `POST /promoted {"node":"hk"}` | 记一次提主（HK 提主时上报） |
| `GET /primary` | 返回 `{"primary":"dmit"\|"hk"\|null,"at":RFC3339\|null}` |
| `GET /health` | 200 即活着，返回 `{"ok":true}` |

- 实现在 `internal/panel/witness.go`：心跳与裁决逻辑抽纯函数（`witnessState` + `verdict`/`promote`/`primaryInfo`），单元测试在 `internal/panel/witness_test.go`（心跳/裁决/提主记录/落盘重载/HTTP 全流程）。
- 注意：`witness serve` 必须在 `LoadConfig` 之前分发——`LoadConfig` 要求 `POLARIS_SECRET_KEY`，见证节点没有面板配置，不能走那条路。

### 13.3 心跳

两台面板主机各装一个 systemd timer（`polaris-ha-heartbeat.timer`），每 10 秒 `curl POST <WITNESS_URL>/heartbeat`：

```
ExecStart=/usr/bin/curl -fsS -X POST --max-time 5 ${WITNESS_URL}/heartbeat \
  -H Content-Type:application/json -d '{"node":"dmit"}'
```

- ha-init 装 dmit 的，ha-join 装 HK 的；`WITNESS_URL` 从安装时的环境变量取，写进 `/etc/polaris/witness.env`（644，无敏感值），timer 的 service 读它。
- `WITNESS_URL` 未配时警告并跳过，不阻塞 init/join；见证节点就绪后，用同一个 `WITNESS_URL` 重跑 ha-init/ha-join 补装（已是 primary/secondary 时只做增量收尾，见 §6.1/§6.3）。
- 心跳失败不重试、不记错（timer 10 秒后再打一次）；见证节点收不到心跳时 `verdict` 自然变 `reachable:false`——丢心跳本身就是信号，不用额外告警通道。

### 13.4 看门狗三条件规则

切 DNS 之前必须同时满足（`scripts/cf-failover.sh`，参数走 `/etc/cf-failover.env` 的 `WITNESS_URL`）：

- (a) 本机连续 3 次连不上 dmit；
- (b) 见证节点也报 dmit 不可达（`GET /verdict?node=dmit` 的 `reachable=false`）；
- (c) 本机能连上见证节点（问 verdict 成功本身即证明）。

任一不满足：不切 DNS，只记日志。见证节点不可达时同样不切（宁可不切，不可错切）；见证节点说 dmit 还活着时不切，并把计数清零（说明是本机到主节点的链路问题）。`WITNESS_URL` 为空时保持旧行为（只按本机探测切），并在日志写明未配置见证节点。

DNS 翻转成功后，看门狗 best-effort 向见证节点 `POST /promoted {"node":"hk"}`（成功记日志，失败记日志但不影响已翻转的 DNS）。手动提主（不经过看门狗翻转）时，人要补调一次同样的 curl，否则见证节点不知道 hk 已提主，dmit 恢复时的围栏（§13.5）就拦不住它。

### 13.5 围栏（dmit 恢复后不能自己当回主）

ha-init 给 dmit 装一个开机一次性检查（`scripts/polaris-ha-fence.sh`，独立文件 + `install-panel.sh` 内嵌副本，测试断言两者字节一致）：

- systemd oneshot `polaris-ha-fence.service`，在 `polaris.service` 之前跑（drop-in `polaris.service.d/ha-fence.conf`：`Requires` + `After`）。
- 问见证节点 `GET /primary`；若 `primary == "hk"`，直接退出非 0 并打印“HK 已提主，按文档把本机重建成从库”的提示，阻止本机 PG 以主库身份启动。
- 见证节点不可达时只警告、放行（可用性优先：不能因为见证节点挂了就让主节点永远起不来）。这个角落情况见 §13.7。
- 围栏只拦面板启动，不拦 PG 本身——PG 即使以主库身份起来了，没有面板写它也不会产生新业务数据；恢复流程（§6.7）仍要求先重建为从库再重新加入。

### 13.6 部署步骤

部署顺序：**先装见证节点，再跑 ha-init/ha-join**（带 `WITNESS_URL`）。见证节点也可以晚于 ha-init/ha-join 再装，装好后重跑补装（§13.3）。

第三台主机（另外商家、Ubuntu、上面跑着一个节点 agent；公网 IP 用占位符 `<WITNESS_IP>`，真 IP 以后再填——地址一律走配置参数，绝不 hardcode）：

```bash
# 第三台上，root 跑；--dry-run 只打印动作，敏感字段打码
curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh | sudo bash -s -- witness-init [--dry-run] [--listen 0.0.0.0:8443]
# 放行端口（或云厂商安全组放行 8443/tcp）
ufw allow 8443/tcp
# 验证
curl http://<WITNESS_IP>:8443/health   # 应返回 {"ok":true}
```

witness-init 做的事：下载面板二进制（checksum 校验；已装且支持 `witness serve` 时跳过下载）、写 `polaris-witness.service`（`ExecStart=<BIN> witness serve --listen 0.0.0.0:8443 --state-file /var/lib/polaris-witness/state.json`，`Restart=on-failure`）、`systemctl enable --now`、curl 验 `/health`。

两台面板主机（ha-init/ha-join 时环境变量给见证地址）：

```bash
# dmit 上
WITNESS_URL=http://<WITNESS_IP>:8443 install-panel.sh ha-init [--dry-run]
# HK 上
WITNESS_URL=http://<WITNESS_IP>:8443 install-panel.sh ha-join <token> https://polaris.cuijianzhuang.com
```

ha-init 还会把 `WITNESS_URL` 写进看门狗 env（`/etc/cf-failover.env`，ha-join 时写；`ha-join --finish` 补装时也写）。

### 13.7 故障模式表

| 场景 | 结果 |
|---|---|
| 见证节点本身挂了 | 看门狗问 verdict 失败 → 不切 DNS，只记日志（降级为“不切”，不是旧行为——旧行为是无见证时切；已配见证时见证不可达一律不切）。告警：看门狗日志里有 `witness unreachable`，巡检时看；dmit/HK 的心跳 timer 也在打，见证恢复后自动续上。dmit 恢复时的围栏放行（只警告）。 |
| HK 被孤立（HK↔dmit 断，HK↔见证通，dmit↔见证通） | 见证节点仍收到 dmit 心跳 → verdict `reachable:true` → 看门狗不切，计数清零。dmit 继续服务，无双主。 |
| dmit 与见证同时挂 | HK 问不到见证 → 不切，等人处理。DNS 不动，业务中断直到人工介入——这是有意为之：两个独立故障域同时挂，无法区分“dmit 死了”还是“HK 被孤立”，宁可停着等人工确认，不可错切。 |
| dmit 挂了，见证活着 | 见证 30 秒后 verdict `reachable:false` → 看门狗三条件满足 → 切 DNS → 上报 `/promoted {"node":"hk"}`。dmit 恢复后围栏拦住它以主库身份启动，必须先重建为从库（§6.7）。 |
| 见证节点重启 | state 文件落盘后重载，心跳与提主记录不丢；落盘文件损坏时服务启动报错（不静默丢状态），删文件可重建（丢心跳历史，30 秒后重新收敛）。 |
| 时钟偏差 | 裁决用见证节点自己的时钟算 `secondsSince`（心跳到达时间），不依赖两台面板主机的时钟；面板主机时钟偏不影响裁决，只影响它自己日志的时间戳。 |
