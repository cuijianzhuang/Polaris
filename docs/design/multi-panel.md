# 多机管理

在一个控制台里查看和操作多个**独立的面板实例**，每台各有自己的数据库、节点、用户与订阅。

本文只描述设计，不含实现。已确认：成员面板均有可达入口（hub 出站拉取即可）、首版包含只读汇总与白名单写操作、社区版限 3 台只读而专业版不限且含写操作。

## 与现有 `cluster` 的区别

仓库里已有 `cluster.go` / `clustermembers.go`：那是**一个逻辑面板**跑多个实例、共享同一套 PostgreSQL，靠数据库租约选主（`isLeader()`），多个实例分担管理界面、订阅与 Agent 连接。它解决的是高可用与水平扩容。

本文的"多机"是**多个互不共享数据库的面板**，一个控制台横向汇总。两者互不替代，也不应复用同一套表与术语：本文的实现使用 `remote_panels` 前缀，界面入口叫"多机"，避免与"集群实例"混淆。

## 角色与数据权威

- **控制台（hub）**：保存远程面板注册表、缓存汇总快照、提供聚合视图与白名单代理操作。
- **成员面板（member）**：行为完全不变，仍是自身节点、用户、流量的唯一权威；不需要为被管理而改动部署形态。
- **数据边界**：hub 只持久化"注册信息 + 最近一次快照 + 错误状态"。用户、订单、节点配置仍只存在于成员面板。删除 hub 上的记录不会影响成员面板，反之亦然。

这条边界是设计前提：任何"从 hub 直接读写成员数据库"的方案都不采用。

## 连接方向与鉴权

首版采用 **hub 主动拉取成员**：

- hub 出站 HTTPS 请求成员的既有管理 API，携带 `Authorization: Bearer <api token>`。
- 成员侧沿用现有 `authToken` 路径与作用域校验，不新增鉴权机制。
- 成员无需向 hub 开放任何入站端口，也不需要把成员加到 hub 的网络里。

令牌管理：

- 新增只读作用域 `panel:read`：返回版本、能力、计数类汇总，**不含**用户身份、订阅内容、订单与节点连接信息。只有这个作用域时，hub 也能工作。
- 需要下钻明细时，由成员管理员额外授予现有作用域（`nodes:read`、`users:read`、`traffic:read`），哪些开、开多少由成员决定。
- 写操作使用独立的新作用域（见"分阶段"），不复用现有写作用域，避免一次授权覆盖全部写能力。
- hub 侧以 `s.st.Box` 加密保存令牌（与 `TunnelAuth`、Reality 私钥同一套密封机制），接口只在创建时回显一次，之后不再返回明文。

TLS：

- 默认强制校验证书；部署自签证书的成员可登记证书指纹做固定校验（复用 Agent 侧已有的 CA 指纹思路）。
- 允许显式"跳过校验"，但必须在界面上标注风险并在审计里记录，不作为默认。

## 数据模型

```go
// remotePanel is one member panel registered on this hub.
type RemotePanel struct {
    ID           string    `gorm:"primaryKey;size:32"`
    Name         string    `gorm:"size:64;uniqueIndex"`   // 显示名
    BaseURL      string    `gorm:"size:255"`              // 成员管理入口，https://host:port
    Token        string    `gorm:"type:text"`             // 密封后的 API 令牌
    Scopes       JSON[[]string] `json:"scopes"`           // 该令牌实际被授予的作用域（仅展示）
    CAPin        string    `gorm:"size:64"`               // 可选：证书 SHA-256 固定
    Insecure     bool      `gorm:"default:false"`         // 显式跳过校验，需审计
    Labels       JSON[[]string] `json:"labels"`
    Enabled      bool      `gorm:"default:true"`
    CreatedBy    string    `gorm:"size:64"`
    // 最近一次状态，仅用于展示与退避，不作为业务权威
    Snapshot     JSON[remoteSnapshot] `json:"snapshot"`
    SnapshotAt   *time.Time  // hub 收到的时刻
    LastOKAt     *time.Time
    LastError    string      `gorm:"size:255"`            // 固定原因代码，不含远端原始报文
    Failures     int         `gorm:"default:0"`           // 连续失败次数，驱动退避
    NextTryAt    *time.Time
    CreatedAt, UpdatedAt time.Time
}
```

快照字段刻意保守：

```go
type remoteSnapshot struct {
    Version      string   // 成员版本
    Capabilities []string // 支持的能力/接口版本，便于版本错位时降级
    Users        int
    OnlineUsers  int      // 有在线 IP 或活动连接的用户数
    Nodes        int
    NodesOnline  int
    TrafficUp, TrafficDown int64 // 自然月累计，见"口径与不一致"
}
```

整数与时间之外不保存任何成员业务数据；快照结构变更时以 `Capabilities` 判断能否解读，不假设同版本。

## API

### 控制台侧

全部要求管理员会话（非 API 令牌），并按现有角色模型限制：

| 方法与路径 | 说明 |
|---|---|
| `GET /panels` | 注册表 + 每台健康状态 + 快照新鲜度 |
| `POST /panels` | 新增（名称、URL、令牌），令牌密封保存 |
| `PUT /panels/:id` | 改名、改 URL、轮换令牌、改证书固定 |
| `DELETE /panels/:id` | 仅删除本机注册，不影响成员 |
| `POST /panels/:id/probe` | 立刻探测一次，返回结果，不影响后台退避 |
| `GET /panels/aggregate` | 跨面板合计 + 每台一行 + 陈旧标记 |
| `GET /panels/:id/overview` | 成员汇总（代理其 `panel:read` 接口） |

`GET` 只读缓存，不触发远端请求（除非显式 `/probe`），避免界面刷新放大远端负载。

写操作统一挂在 `POST /panels/:id/actions/:action`，动作白名单化，每个动作声明所需作用域，并在 hub 与成员两侧各留一条审计。

### 写操作白名单

hub 不代理任意路径，成员侧提供**一个类型化入口** `POST /panel/actions`：

```json
{"action":"node.sync","nodeId":"n_xxx"}
{"action":"node.upgrade-agent","nodeId":"n_xxx"}
{"action":"user.renew","userId":"u_xxx","days":30}
{"action":"user.reset-traffic","userId":"u_xxx"}
{"action":"user.add-traffic","userId":"u_xxx","bytes":10737418240}
```

成员的 `panel:actions` 作用域只放行下表动作，每个动作复用成员已有的处理函数与审计，不新写业务逻辑：

| 动作 | 复用的成员实现 | 说明 |
|---|---|---|
| `node.sync` | `syncNode`（`POST /nodes/:id/sync`） | 重新下发该节点配置，可替代"重启 Agent"的常见用途 |
| `node.upgrade-agent` | `upgradeAgentAPI`（`POST /nodes/:id/upgrade-agent`） | 升级 Agent 版本 |
| `user.renew` | `renewUser`（`POST /users/:id/renew`） | 续费指定天数 |
| `user.reset-traffic` | `resetUserTraffic`（`POST /users/:id/reset-traffic`） | 清零已用流量 |
| `user.add-traffic` | `addUserTraffic`（`POST /users/:id/add-traffic`） | 追加流量包 |

刻意不开放：删除用户/节点、改价格与套餐、改系统设置、查看用户密码或订阅链接、批量导入导出。需要这些能力时直接在成员面板上操作，或作为后续单独评估的动作。

### 成员侧

- 新增 `GET /panel/self`：返回上面的 `remoteSnapshot` + 版本 + 能力清单，要求 `panel:read`。
- 新增 `POST /panel/actions`：上表的类型化写入口，要求 `panel:actions`；未授予该作用域时返回 403，且不暴露可用动作清单。
- 复用现有 `/stats/overview` 作为可选明细来源（已带 30 秒 TTL 缓存），但只在成员显式授予 `traffic:read` 时由 hub 调用。
- 以上接口默认不开放给匿名访问；`panel:read` 令牌不能读取用户身份、订阅、订单。

## 采集与失败处理

- **单实例采集**：后台轮询只在 `isLeader()` 的实例上运行，多个 hub 实例不会重复请求同一成员。
- **节奏**：默认 60 秒一轮，加抖动；同一轮内对成员并发上限固定（例如 8），避免 N 台时瞬时打满出站连接。
- **超时与退避**：单台 10 秒超时；失败按指数退避（60 秒起步，上限 30 分钟），成功后清零。`Failures` 与 `NextTryAt` 都落库，重启不丢进度。
- **错误分类**：只保存固定原因代码（`timeout`、`tls`、`unauthorized`、`http_5xx`、`schema` 等），不保存远端原始报文，避免把成员内部信息带进 hub 日志与界面。
- **陈旧优于空白**：界面永远显示"最近一次成功快照 + 距今多久"，失败不清空已有数字；这比显示 0 更安全，也不会让人误判为"成员没有用户"。
- **不阻塞界面**：所有列表与聚合读本地缓存，远端刷新在后台完成。

## 口径与不一致

跨面板求和容易产生误导，明确以下规则：

- 用户数、节点数、在线用户数可以直接相加。
- 流量统一为**自然月**口径（成员 `GET /panel/self` 返回当月累计），因此跨面板可以直接相加，不会出现窗口不一致。
- 所有时间以 hub 收到快照的时刻为准，不采用成员返回的时间戳，规避时钟偏差。
- 版本不同导致字段缺失时，聚合显示为"未知"而不是 0。

## 安全

- **出站 SSRF 防护**：URL 只允许 `https`（本地调试可放开 `http`）；回环、链路本地、云元数据地址（`169.254.169.254` 等）在**连接建立时**按实际解析结果拦截（dialer 层），DNS 重绑定无法绕过注册时的静态检查。这是防御性措施，即使只有 owner 能配置也保留。
- **最小权限**：默认只申请 `panel:read`；成员可随时在自身界面吊销令牌。
- **令牌不落地明文**：密封存储、只在创建时回显、日志与审计中脱敏。
- **审计**：hub 侧每次新增/改令牌/删除/探测失败/写操作都写 `audit`，包含管理员身份、面板名、动作；成员侧原有审计照常记录被代理的写操作。
- **影响面**：hub 被攻破时，攻击者最多拿到成员授予该令牌的作用域；不会得到成员数据库直连能力。

## 界面

- 侧边栏新增"多机"页：表格列出名称、版本、健康、用户/在线/节点、流量、快照年龄；异常行用状态色区分，不隐藏数字。
- "打开面板"跳转到成员自身地址的新标签页。不使用 iframe：成员通常有独立的 Cookie 域与 `X-Frame-Options`，嵌入会失效或引发安全争议。
- 详情抽屉：单台快照、最近错误、作用域清单（该令牌实际被授予什么）、探测按钮。
- 仪表盘增加"全部面板"卡片：合计数字 + 每台一行，陈旧或失败的面板单独标出。
- 新增文案全部走 `web/src/locales/en/` 分页文件，与现有 i18n 结构一致。

## 分阶段交付

1. **P1 只读汇总**：`remote_panels` 表、`panel:read` 作用域、成员 `GET /panel/self`、hub 的注册表 CRUD、单实例轮询与退避、列表与聚合界面。
2. **P2 受限操作**：成员 `POST /panel/actions` 与 `panel:actions` 作用域、hub 的 `POST /panels/:id/actions/:action`、动作白名单、双侧审计、按角色与面板限制。
3. **P3 增强**：跨面板用户搜索、成员告警汇总到 hub、批量操作。

P1 与 P2 是本次交付范围；P3 视使用情况再排。内网成员所需的出站 mTLS 通道（复用 Agent 的 CA 与连接模型）已确认当前不需要，作为将来独立阶段，不影响本文的数据结构与作用域设计。

每一阶段都能独立交付，P1 完成后即使暂不做 P2 也是有价值的。

## 许可证与版本差异

新增 `FeatureMultiPanel` 特性位，与现有 `FeatureSpeedLimit` 等同级：

- **社区版**：最多登记 3 台面板，只读（`panel:read`）。
- **专业版**：数量不限，并可用 `panel:actions` 的写操作。

超出数量上限时注册接口直接拒绝并说明原因，不静默丢弃；写操作在社区版上返回 `feature_required`，界面按钮置灰并提示升级。计数与校验在 hub 侧完成，成员侧不感知许可类型。

## 测试计划

- 单元：注册表 CRUD、令牌密封/解封往返、作用域校验、URL 与 SSRF 校验、快照解码的向前兼容。
- 集成（`httptest` 假成员）：
  - 正常快照写入缓存、`LastOKAt` 与 `Failures` 重置；
  - 超时、TLS 失败、401、5xx 的原因分类与退避曲线；
  - 非 leader 实例不轮询；
  - 失败后界面仍返回旧快照且标记陈旧；
  - 聚合在版本缺字段时显示"未知"；失败面板跳过计数但不隐藏行。
- 端到端：docker-compose 起两台真实面板 + 一台 hub，校验合计数字与手工核对一致，吊销令牌后 hub 显示 `unauthorized`。
- 回归：确认 `cluster`（共享数据库多实例）行为完全不受影响。

## 不做的事

- 不做共享数据库：那不是多机，是 `cluster` 已解决的另一个问题。
- 不做跨面板用户迁移、余额/流量互通。
- 不做自动故障转移与统一订阅出口。
- 不在 hub 上镜像成员的业务表，避免产生第二份权威数据。
