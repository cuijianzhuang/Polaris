# 高可用（主备）

> 本文以 dmit（主）/ HK（备）为例：dmit 是现有的单机面板，HK 是香港节点 `45.78.33.200`。域名 `polaris.cuijianzhuang.com` 全托管在 Cloudflare。

## 为什么要做高可用

单机面板有四个单点：面板进程、PostgreSQL、Redis、入口域名。任意一台整机挂掉，另一台能接管：

- 面板进程：两台各跑一个实例，连同一个 PG；leader 租约自动选主，主挂后 15 秒内另一实例接管定时任务和 Telegram 轮询。
- PostgreSQL：流复制，主 dmit → 从 HK（只读热备）。
- Redis：主从复制，主 dmit → 从 HK。
- 入口域名：Cloudflare DNS 自动切换，看门狗发现主站不通 3 分钟内把 A 记录切到备机。

故障切换是“DNS 自动 + 数据库人工提主”：PG 自动提主有脑裂风险，所以提主永远是人确认“主真的死了”之后再做。切 DNS 之前还要过第三台见证节点的仲裁（见[部署见证节点](#部署见证节点)），避免“主没死、只是链路断了”时误切造成双主。**RTO 约 10–15 分钟**（看人操作的速度），不是秒级；复制默认异步，**故障瞬间可能丢几秒写入**。

```
平时：  用户/节点 ──DNS──▶ dmit:443/9443 ──▶ 面板1 ──WG──▶ PG主/Redis主（dmit）
                                  ▲                       ▲
                                  │         WireGuard 隧道 │ （复制流量加密）
                                  │                       ▼
                      HK 面板2（热备，连主库）   PG从/Redis从（HK）
        见证：  dmit/HK 每 10 秒向第三台见证节点报心跳（8443），切 DNS 前须它确认
故障：  dmit 挂 ──▶ 看门狗 3 分钟内把 DNS 切到 45.78.33.200
        ──▶ 人工提主 PG/Redis（HK）+ 改 HK 面板配置重启 ──▶ HK 接管
```

Polaris 原生支持多实例，不用改程序：CA 存在库里的 `cluster_secrets` 表，两实例共用同一个 CA；JWT 无状态，登录态不用粘滞会话。但有两个前提必须配好：**`POLARIS_SECRET_KEY` 两台必须相同**；**配 `redis_url`**，否则在线 IP 统计、限流、告警去重各实例自己算一份，HA 下会不准。各实例的 `data_dir` 各自独立，不要共享（里面有实例 ID、agent 二进制缓存、备份）。

## 前置条件

| 检查 | 要求 |
|---|---|
| 数据库 | 必须是 PostgreSQL。SQLite 的库先用 `polaris migrate` 迁到 PG 再做 HA |
| 面板版本 | 两台装同一版本；做 HA 之前先把 dmit 升到含 PR1+PR2 的最新版（`ha-init` 会先验二进制里有没有 HA 命令，没有就提示先 `polaris update`）。见证服务（`witness serve`）是 PR5 加的，已含在 v0.1.262 及之后版本；装之前先确认要装的版本已含 PR5 |
| 域名 | `polaris` 的 A 记录改成 **TTL 60、DNS-only（灰云）**。必须是灰云：橙云代理只支持部分端口，**9443 不在其中**，开了代理 agent 连不上 |
| 端口 | 主节点 `51820/udp`、`5432`、`6379` 未被 WG/PG/Redis 以外进程占用；备节点 `8080`/`9443`/`51820` 空闲；`443` 空闲时 `ha-join` 装 nginx 做反代，`443` 已被 OpenResty（如 1Panel 装的）占用时自动切复用模式（反代自行在 1Panel 里加，见[复用 OpenResty](#复用-openresty1panel)）；`443` 被节点入站（如 VLESS+TLS）占了，先把该入站端口改掉 |
| 见证节点 | 第三台主机：另外商家（与 dmit/HK 的搬瓦工不同，独立故障域）、Ubuntu、上面跑着一个节点 agent；公网 IP 记为 `<WITNESS_IP>`，先装好见证服务（见[部署见证节点](#部署见证节点)），再跑 `ha-init`/`ha-join` |
| 磁盘 | 备机剩余空间 ≥ 主库大小 × 1.2 |
| PG 版本 | 两台 PG 大版本必须一致（如都是 15）；HK 未装 PG 时 `ha-join` 会从 PGDG 装同版本，已装但大版本不同时直接退出 |
| 时间同步 | 两台都开 NTP（`timedatectl set-ntp true`）；leader 租约时间以库为准，时钟偏一点没事，别差出分钟级 |
| 权限 | 两台都要 root；dmit 公网 IP 要能探测到，探测不到时 `ha-init` 加 `--endpoint <IP>` |

## 部署见证节点

第三台主机只投票、不存数据：它不跑面板、不参与 PG/Redis 复制，只回答“dmit 是不是真挂了”，让看门狗切 DNS 时不再只凭 HK 自己一家的判断（HK 连不上 dmit，可能是 dmit 挂了，也可能是 dmit↔HK 链路断了；后一种情况切 DNS 会造成双主）。第三台是另外商家的机器（与 dmit/HK 的搬瓦工不同，独立故障域）、Ubuntu、上面跑着一个节点 agent，公网 IP 记为 `<WITNESS_IP>`。

> 版本要求：见证服务（`polaris witness serve`）是 PR5 加的，二进制须为包含它的发版（v0.1.262 及之后已含）。

部署顺序：**先装见证节点，再跑 `ha-init`/`ha-join`**（带 `WITNESS_URL`，见下面）。见证节点也可以晚于主备部署再装，装好后重跑补装心跳与围栏。

第三台上以 root 执行，先 `--dry-run` 演练一遍再真跑：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) witness-init --dry-run
bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) witness-init
```

`witness-init` 按顺序做这些事：下载面板二进制并校验（已装且支持 `witness serve` 时跳过下载）、写 `polaris-witness.service`（默认监听 `0.0.0.0:8443`，状态文件 `/var/lib/polaris-witness/state.json`）、启动服务并验 `curl http://127.0.0.1:8443/health` 返回 `{"ok":true}`。改监听地址加 `--listen 0.0.0.0:<端口>`。

装完放行端口并验证：

```bash
ufw allow 8443/tcp   # 或云厂商安全组放行 8443/tcp
curl http://<WITNESS_IP>:8443/health   # 应返回 {"ok":true}
```

然后在跑 `ha-init`（dmit）/`ha-join`（HK）时把见证地址传进去，脚本自动装心跳定时器与围栏：

```bash
# dmit 上
WITNESS_URL=http://<WITNESS_IP>:8443 bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) ha-init
# HK 上
WITNESS_URL=http://<WITNESS_IP>:8443 bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) ha-join <token> https://polaris.cuijianzhuang.com
```

心跳：dmit 和 HK 各装一个 systemd timer（`polaris-ha-heartbeat.timer`），每 10 秒向见证节点报一次心跳，见证节点 30 秒内没收到某台的心跳就判它不可达。围栏：dmit 上装一个开机一次性检查（`polaris-ha-fence.service`），面板启动前问见证节点谁是主（见[故障切换](#故障切换)）。

注意：

- 没传 `WITNESS_URL` 时，`ha-init`/`ha-join` 只警告并跳过心跳与围栏，不阻塞部署；见证节点就绪后，用同一个 `WITNESS_URL` 重跑 `ha-init`/`ha-join` 补装（已是主/备节点时只做增量收尾，不重复初始化）。
- 8443 目前无鉴权，任何能连上它的人都能调 `/promoted` 改提主记录。建议防火墙只放行 dmit 与 HK 的公网 IP，不要全网放行 8443。

## 部署步骤

先在面板“高可用”页把 Cloudflare Token 保存好（见[日常操作](#日常操作)）。没有 token 也能先做部署，只是备机的证书与看门狗会跳过，后面再补。

### 1. 主节点：跑 `ha-init`

dmit 上以 root 执行，先 `--dry-run` 演练一遍，输出与手工步骤一一对应，确认无误再真跑：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) ha-init --dry-run
bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) ha-init
```

`ha-init` 按顺序做这些事：

1. 备份（`polaris backup` 到 `/root`，`pg_dumpall` 到 `/root/pg-full-<日期>.sql`，`/etc/polaris/polaris.env` 备份到 `/root/polaris.env.bak`）。
2. 生成 WireGuard 密钥对，写 `/etc/wireguard/wg0.conf`（`10.8.0.1/24`，`ListenPort 51820`），启动隧道。
3. 生成复制账号与 Redis 随机密码，密封存进库里（`polaris ha-seal-secret`，不落明文）。
4. PG 改主库配置（`wal_level=replica`、`max_wal_senders=5`、`max_replication_slots=5`、`wal_keep_size=1GB`、`listen_addresses` 加 `10.8.0.1`、`pg_hba.conf` 只放行隧道网段），建 `replicator` 账号与 `hk_slot` 复制槽，重启 PG。
5. Redis 改主库配置（`bind 127.0.0.1 10.8.0.1`、`requirepass`、`appendonly yes`），重启 Redis。
6. 面板 env 的 `POLARIS_DB_DSN` 与 `POLARIS_REDIS_URL` 改走 `10.8.0.1`，重启面板并确认 `/api/v1/info` 返回 200。
7. 启用 WireGuard 对端同步定时器（`polaris-ha-sync.timer`，每 60 秒跑 `polaris ha sync-wg`）。
8. 发加入 token，打印备机加入命令。

注意：

- `ha-init` 会重启一次 PG，面板短暂连不上库（几秒），之后自动恢复。选低峰操作。
- dmit 现在用 Unix socket peer 认证，`polaris` 角色本来没有密码；DSN 改走 TCP `10.8.0.1` 时 `ha-init` 会自动给它设一个随机密码，不用手动改。
- 已是主节点时重跑 `ha-init`，只打印状态并发一个新 token，不会重复初始化。
- 传了 `WITNESS_URL` 时，`ha-init` 还会装见证心跳定时器与围栏检查（见[部署见证节点](#部署见证节点)）；没传只警告跳过，见证就绪后重跑补装。

### 2. 备节点：跑 `ha-join`

用 `ha-init` 最后打印的命令，或面板“高可用”页“加入备节点”里“生成加入命令”得到的命令。HK 上以 root 执行，同样先 `--dry-run`：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) ha-join <token> https://polaris.cuijianzhuang.com --dry-run
bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) ha-join <token> https://polaris.cuijianzhuang.com
```

token 只有 10 分钟有效、一次使用，过期或用过就重新生成。`panel-url` 里要带上管理路径（如果设了的话），比如 `https://polaris.cuijianzhuang.com/管理路径`。

`ha-join` 按顺序做这些事：

1. 生成本机 WG 密钥对，向主节点注册对端并取配置包（先取包再注册，取包失败不会把 token 烧掉）。
2. 写本机 `wg0.conf`（`10.8.0.2/24`，对端为主），启动隧道并 `ping 10.8.0.1` 验证。
3. `pg_basebackup` 从主库拉全量建从库（`-S hk_slot`），配 `hot_standby=on`，启动并验证 `pg_is_in_recovery()=t` 且回放追上。
4. Redis 配从库（`replicaof 10.8.0.1 6379`），启动并验证 `role:slave`、`master_link_status:up`。
5. 调现有安装流程装面板（与主节点同版本、同库、同 Redis，`--port 8080 --agent-port 9443`），再按配置包同步 `SECRET_KEY` / `SECRET_KEY_PREVIOUS` / `ADMIN_PATH` / `PUBLIC_URL` / `TIMEZONE`。
6. 反代（二选一）：`443` 空闲时装 nginx——443 反代 + `stream` 段 9443 TCP 直通（不终结 TLS），certbot DNS-01 拿证；`443` 已被 OpenResty 占用时复用 OpenResty，`ha-join` 不装 nginx、不申请证书，打印出要在 1Panel 里加的反代配置（见[复用 OpenResty](#复用-openresty1panel)）。加完反代再用 `--resolve` 直接验备机 `https://polaris.cuijianzhuang.com/api/v1/info` 返回 200。
7. 装看门狗（`/usr/local/bin/cf-failover.sh`，每分钟跑）与备份同步 cron（每小时 rsync 到 `backups-peer`，只在免密 SSH 已通时装）。
8. 自检逐项打勾：WG、PG 复制、Redis、双实例、备机直验 200；任一项失败即报错退出，不静默通过。

注意：

- 没有 CF token 时，第 6 步的证书与第 7 步的看门狗会跳过并明确提示，备节点先以 HTTP 反代跑起来；拿到 token 后在备机上补跑 `ha-join --finish`（不带 token，CF token 从环境变量 `CF_API_TOKEN` 取）：
  ```bash
  CF_API_TOKEN=<token> bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh) ha-join --finish
  ```
- `ha-join` 只接管全新空库；本机已有面板数据库时会直接退出，请先备份并清空。
- 备机上的看门狗只装在备机；`polaris-ha-sync.timer` 只在主节点启用（备机的对端只有主，join 时已写死）。
- 传了 `WITNESS_URL` 时，`ha-join` 还会装 HK 的心跳定时器，并把 `WITNESS_URL` 写进看门狗配置（`/etc/cf-failover.env`）；没传只警告跳过，见证就绪后重跑补装。

### 复用 OpenResty（1Panel）

备机上 443 已被 OpenResty（如 1Panel 装的）占用时，`ha-join` 不会报错退出，而是自动切复用模式：

- 不装 nginx、不写 nginx 配置、不申请证书（证书由 1Panel 管理）；
- 打印出要在 1Panel 里加的反代配置，模式记入 `POLARIS_HA_PROXY_MODE=openresty`（`ha-join --finish` 时只补看门狗，不补证书）；
- 也可以用环境变量强制指定模式：`HA_PROXY_MODE=nginx`（要求 443 空闲）或 `HA_PROXY_MODE=openresty`（要求 443 已被 OpenResty 占用），默认 `auto` 自动判断。

在 1Panel 的 OpenResty 里给 `polaris.cuijianzhuang.com` 建站（开启 HTTPS），再添加：

```nginx
# HTTP：443 反代面板
server {
    listen 443 ssl;
    server_name polaris.cuijianzhuang.com;
    ssl_certificate <1Panel 里该站点的证书路径>;
    ssl_certificate_key <1Panel 里该站点的私钥路径>;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}

# TCP 直通：9443 节点接入（不终结 TLS，加到 OpenResty 主配置的 stream 段）
stream {
    upstream panel_agent {
        server 127.0.0.1:9443;
    }
    server {
        listen 9443;
        proxy_pass panel_agent;
        proxy_timeout 1h;
    }
}
```

加完后验证：`curl -sk --resolve polaris.cuijianzhuang.com:443:45.78.33.200 https://polaris.cuijianzhuang.com/api/v1/info` 返回 200 和版本号。1Panel 的 OpenResty 若跑在容器里且 127.0.0.1 不通，把 127.0.0.1 换成宿主机 IP。

### 3. 验证

面板“高可用”页（侧边栏“高可用”，`/ha`）逐项确认：

- “状态”卡：角色显示主节点/备节点，数据库角色显示主库/从库，回放延迟为 0 或 1 秒左右，Redis 显示已配置。
- “实例”表：两行，两个实例都在线，其中一个是 Leader。
- “PostgreSQL 复制”卡：主节点上看到 HK 的连接，`state` 为 `streaming`。
- “WireGuard 对端”表：看到对端的 WG IP、公钥、对端地址。
- 直接验备机（DNS 还没切）：`curl -sk --resolve polaris.cuijianzhuang.com:443:45.78.33.200 https://polaris.cuijianzhuang.com/api/v1/info` 返回 200 和版本号。
- 在 dmit 上建个测试用户或改个设置，HK 面板上立刻能看到（共用一个库的证据）。

也可以在命令行查：

```bash
wg show   # 两台都有对端，ping 10.8.0.x 双向通
su postgres -c "psql -c 'SELECT client_addr, state, sync_state FROM pg_stat_replication;'"   # dmit 上
su postgres -c "psql -tAc 'SELECT pg_is_in_recovery();'"   # HK 上应为 t
su postgres -c "psql -tAc 'SELECT pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn();'"   # HK 上应为 t
su postgres -c "psql -d polaris -c 'SELECT * FROM cluster_members;'"   # 应有两行
```

## 日常操作（面板“高可用”页）

- **看状态**：页面顶部“刷新”按钮重新拉取；“状态”卡里的角色、Leader、数据库角色、回放延迟、Redis、版本一眼能看到。SQLite 下复制相关的项会明确写“不可用（需 PostgreSQL）”，不是空白。
- **生成加入命令**：“加入备节点”点“生成加入命令”，复制整行命令到备节点执行。命令只显示一次，有效期 10 分钟；生成新 token 会作废旧 token，用完点“隐藏”。
- **换 Cloudflare Token**：“Cloudflare Token”里填新的保存即可覆盖；保存后只显示“已配置”，面板不会回显 token 内容，也没法查看，只能重新保存覆盖。换 token 后记得同步到备机：看门狗读 `/etc/cf-failover.env`，certbot 读 `/root/.cloudflare.ini`，这两处不会自动跟着变，改完在备机上各更新一次。
- **删除对端**：“WireGuard 对端”表里点“删除”，只删除库里的记录，WireGuard 配置由主节点的 `polaris ha sync-wg` 定时任务对齐。删完备节点就不再是集群对端，下线备节点用。
- **本机 WG 地址**：页面不显示，因为接口没有返回这个字段；不要拿 10.8.0.1/10.8.0.2 去猜，以对端表里的实际记录为准。

敏感操作（生成加入命令、保存 CF token、删除对端）首次执行时会要求二次验证（sudo），与面板其他敏感操作同一套流程。

## 故障切换（dmit 挂了）

按顺序做，别跳步。DNS 由看门狗自动切（3 分钟内），数据库提主人工做。切之前看门狗须同时满足三条件：本机连续 3 次连不上 dmit、见证节点也报 dmit 不可达、见证节点本身可达；任一不满足不切。见证说 dmit 还活着时不切，并把计数清零；见证节点不可达时同样不切（宁可不切，不可错切）。没配见证时保持旧行为（只按本机探测切）。先在备机上打开“高可用”页的“故障切换向导”，点“开始检查”，确认检查结果再动手。

```bash
# 0. 先确认 dmit 真挂了，不是面板假死：ping / SSH 都不通再往下做
#    （WG 隧道断不等于 dmit 挂；看门狗验的是 dmit 公网 443，不是隧道）

# 1. HK 上把 PG 从库提为主库
su postgres -c "psql -c 'SELECT pg_promote();'"
su postgres -c "psql -tAc 'SELECT pg_is_in_recovery();'"   # 应为 f

# 2. HK 上把 Redis 从库提为主库
redis-cli -h 10.8.0.2 -a <REDIS_PASS> REPLICAOF NO ONE
redis-cli -h 10.8.0.2 -a <REDIS_PASS> info replication      # role:master

# 3. 向见证节点上报提主（否则 dmit 恢复时围栏拦不住它以主库身份启动）
curl -s -X POST http://<WITNESS_IP>:8443/promoted -H 'Content-Type: application/json' -d '{"node":"hk"}'
#    若 DNS 是看门狗切的，它已经上报过；手动切 DNS 或不确定时，补调一次（重复调无害）

# 4. 改 HK 面板配置，指向本机（10.8.0.2）的新主库
sed -i 's/@10\.8\.0\.1:/@10.8.0.2:/g' /etc/polaris/polaris.env
grep -E 'POLARIS_(DB_DSN|REDIS_URL)' /etc/polaris/polaris.env
systemctl restart polaris
journalctl -u polaris -n 30 --no-pager
curl -sf http://127.0.0.1:8080/api/v1/info | head -c 200; echo

# 5. 确认 DNS 已切到 HK（看门狗自动做，最多 3 分钟）
dig +short polaris.cuijianzhuang.com        # 应为 45.78.33.200
tail -20 /var/log/cf-failover.log

# 6. 验证业务
#    面板能打开登录；节点列表里节点陆续变在线（agent 按域名重连，最多几分钟）；
#    新用户注册/订阅能拉取；leader 应为 HK：查 cluster_leases 的 holder
```

**如果看门狗没切 DNS**（比如它自己也挂了，或见证节点不可达、不同意切），手动切：Cloudflare 面板把 `polaris` 的 A 记录改成 `45.78.33.200`。手动切完别忘了向见证补报提主（上面第 3 步），否则 dmit 恢复时围栏拦不住它。

**如果 dmit 只是面板进程挂了、机器没挂**：不用走上面全套。`systemctl restart polaris` 就行；还不行就看日志。leader 会在 15 秒内自动漂到 HK，但库和 Redis 都还在 dmit，HK 面板照常连主库提供服务，DNS 不用动。

向导里的步骤与上面一致：向导只做检查（主站可达、回放延迟、DNS 当前指向）与给出命令，不代劳执行；每做完一步在向导里勾一步。

## 故障恢复（dmit 回来了）

### 千万别直接把 dmit 的旧主库起起来

dmit 上的 PG 数据比 HK 旧，直接启动会造成双主、数据分叉。正确做法：dmit 重建成从库。

另外注意：HK 提主后，见证节点记着 `primary=hk`；dmit 面板启动前的一次性检查（围栏）会拦住它以主库身份启动，这是预期行为——按下面流程把 dmit 重建成从库后再启动面板。围栏只拦面板启动，不拦 PG 本身。

```bash
# dmit 上执行
systemctl stop postgresql
rm -rf /var/lib/postgresql/<版本>/main/*
su postgres -c "pg_basebackup -h 10.8.0.2 -p 5432 -U replicator -D /var/lib/postgresql/<版本>/main -Fp -Xs -P -R -S hk_slot"
# 注意：HK 提主后，复制槽还在（槽跟着数据走）；若报错槽不存在，先在 HK 上建一个：
#   su postgres -c "psql -c \"SELECT pg_create_physical_replication_slot('hk_slot');\""
#   再重跑上面命令（去掉 -S hk_slot 或换新槽名）
systemctl start postgresql
su postgres -c "psql -tAc 'SELECT pg_is_in_recovery();'"   # 应为 t
```

Redis 从库同理重建（`replicaof 10.8.0.2 6379`），再把 dmit 面板 env 里的 `10.8.0.1` 改回指向当前主库（HK 的 `10.8.0.2`），重启面板。

### 两个选择

**A. 保持 HK 为主（推荐，省事）**：什么都不用再改。dmit 恢复成从库后就是新的备机，DNS 继续指着 HK。以后有空再做角色对调。

**B. 切回 dmit 为主**：按故障切换的流程反向做一遍（在 dmit 上提主，HK 重建成从；Redis 同理；两台 env 的 IP 改回 `10.8.0.1`；DNS 手动改回 dmit IP；清空 `/var/lib/cf-failover.state`）。DNS 指回 dmit 后，向见证复位再启动 dmit 面板（不复位围栏会拦住面板启动）：

```bash
curl -s -X POST http://<WITNESS_IP>:8443/promoted -H 'Content-Type: application/json' -d '{"node":"dmit"}'
```

只在低峰做。

## 备份同步

备份只落在 leader 本机的 `/var/lib/polaris/backups`，leader 换了备份就换地方。`ha-join` 装好后每小时同步一次，两台互相同步到 `backups-peer`（走 WG 传输，只在免密 SSH 已通时装）：

```bash
# 实际装好的 cron（每小时整点）
0 * * * * rsync -a --delete /var/lib/polaris/backups/ root@10.8.0.<对端>:/var/lib/polaris/backups-peer/
```

手动备份随时能做：`polaris backup`（面板管理端 系统 → 备份 也可以点），再做一份 PG 全量逻辑备份存在 `/root`：`su postgres -c "pg_dumpall > /root/pg-full-$(date +%F).sql"`。

## 可选项：同步复制（零丢失，但要付出延迟）

默认异步复制，故障瞬间可能丢几秒写入。若接受每次写库多一次 dmit↔HK 往返延迟：

```sql
-- dmit 主库上
ALTER SYSTEM SET synchronous_standby_names = 'hk_slot';
SELECT pg_reload_conf();
```

再确认从库的 `primary_conninfo` 里 `application_name` 对得上（`pg_basebackup -R` 默认写的是 `walreceiver`，需改成 `hk_slot` 对应的名字，或把 `synchronous_standby_names` 设为 `*`）。改完验证主库 `pg_stat_replication` 的 `sync_state` 为 `sync`。

## 限制与常见问题

1. **agent 直连 IP 的节点不跟随切换**：安装节点时填的是域名就没问题；若当初填的是 dmit 的公网 IP，该节点故障时不会自动改连 HK，需要重装 agent 或改它的接入地址。先在节点列表里确认各节点的接入地址是域名。
2. **证书**：HK 的证书走 DNS-01，不依赖 80 端口；dmit 的 OpenResty 证书按现有方式续，两边互不干扰。续期走 systemd timer 自动跑，测一次 `certbot renew --dry-run`。
3. **时间同步**：两台都开 NTP，leader 租约时间以库为准，时钟偏一点没事，但别差出分钟级。
4. **dmit 只有 1 核 1.9G**：PG + Redis + 面板同机没问题，但别在上面再跑重活。
5. **不要在两台上同时跑看门狗**，只装 HK。dmit 若装了同款脚本，恢复后会把 DNS 抢回去造成抖动。
6. **不要共享 data_dir**（NFS 之类），各实例的 `cluster-instance-id` 和 agent 二进制缓存必须独立。
7. **升级流程**：先升 HK（备），确认 `/api/v1/info` 版本号和业务正常，再升 dmit；数据库迁移由先启动的实例执行，两台版本短暂不一致一般没问题，但别拖太久。
8. **许可证**：专业版授权走 `license.cuijianzhuang.com`，与面板实例数无关，两台共用同一个授权服务，不用另买。
9. **回滚**：任何一步做错，停掉 HK 的面板（`systemctl stop polaris`）、DNS 保持指 dmit，就回到现在的单机状态；PG 从库停掉也不影响主库。
10. **RTO 与 RPO**：DNS 切换 3 分钟内，整套提主约 10–15 分钟；异步复制下故障瞬间可能丢几秒写入，要零丢失改同步复制（见上节）。
11. **见证节点不可达时不切 DNS**：配了见证后，看门狗必须同时满足三条件才切；见证挂了、或见证说 dmit 还活着，都不切，宁可等人工确认。见证与 dmit 同时挂时 DNS 不动，等人处理。
12. **见证接口无鉴权**：8443 的 `/heartbeat`、`/verdict`、`/promoted`、`/primary` 都不鉴权，防火墙只放行 dmit 与 HK 的公网 IP，别全网放行。

## 命令与路径速查

| 用途 | 命令 / 路径 |
|---|---|
| 初始化主节点 | `install-panel.sh ha-init [--dry-run] [--endpoint IP]`（dmit，root） |
| 加入备节点 | `install-panel.sh ha-join <token> <panel-url> [--dry-run]`（HK，root） |
| 补证书与看门狗 | `install-panel.sh ha-join --finish [--dry-run]`（HK，root，`CF_API_TOKEN` 从环境变量取） |
| 部署见证节点 | `install-panel.sh witness-init [--dry-run] [--listen ADDR]`（第三台，root，默认 `0.0.0.0:8443`） |
| 见证心跳/围栏 | `WITNESS_URL=http://<WITNESS_IP>:8443` 传给 `ha-init`/`ha-join` 时自动装；心跳 timer `polaris-ha-heartbeat.timer`（每 10 秒），围栏 `polaris-ha-fence.service`（dmit 面板启动前） |
| 见证状态 | `curl http://<WITNESS_IP>:8443/health`；`curl "http://<WITNESS_IP>:8443/verdict?node=dmit"`；`curl http://<WITNESS_IP>:8443/primary` |
| 本机 HA 状态速查 | `install-panel.sh ha-status` |
| 发加入 token | 面板“高可用”页“生成加入命令”，或终端 `polaris ha-token`（10 分钟有效，一次使用） |
| 密封存一个 secret | `polaris ha-seal-secret <名字>`（从 stdin 读值，不进 ps 输出；`ha-init` 用它存复制与 Redis 密码） |
| 对齐 WG 对端 | `polaris ha sync-wg`（`polaris-ha-sync.timer` 每 60 秒自动跑，只在主节点启用） |
| 面板 env | `/etc/polaris/polaris.env` |
| WG 配置 | `/etc/wireguard/wg0.conf`（私钥 `privatekey` 0600，永不离机） |
| 看门狗脚本 | `/usr/local/bin/cf-failover.sh`，配置 `/etc/cf-failover.env`，日志 `/var/log/cf-failover.log`，计数 `/var/lib/cf-failover.state` |
| 备份目录 | `/var/lib/polaris/backups`（本机），对端同步目录 `/var/lib/polaris/backups-peer` |
| HA 页面 | 面板侧边栏“高可用”，路由 `/ha` |
