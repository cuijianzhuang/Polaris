# 443 端口复用：SNI 分流多协议 + 网站回落（设计稿，待审定）

目标形态（用户拍板）：节点上一个 443 端口，按 TLS ClientHello 的 SNI 分流给多个协议入站，未命中的流量回落到一个真实网站做伪装。本文先盘清现状与各内核能力，再拍形态选型；本轮只出设计，不写功能代码。

## 调研结论

### 已有的两种「共用端口」机制——都不是 SNI 分流

1. **Caddy 前置（路径分流，L7 终止）**：`internal/panel/fronting.go` 的 `buildFronts` 把 CDN 类入站（WS / gRPC / XHTTP / HTTPUpgrade + TLS）改写到回环 `127.0.0.1:40000+n` 且安全层置空，由节点内置 Caddy（`internal/core/caddy`）在公网端口（默认 443）终止 TLS、按**路径**分发；同一监听还挂静态网站（`model.WebsiteSpec`，统一 Web 入口 `unified-web-entry-v1`）。局限：TLS 在 Caddy 终止、只能承载 HTTP 形传输；VLESS-TCP-Vision、Trojan-TCP、REALITY 这类需要端到端握手的入站进不来。
2. **Xray fallback（ALPN / 路径分发，单主入站）**：`model.ProtocolSpec.Fallbacks` + `internal/panel/fallback.go`。一个 VLESS / Trojan + TCP + TLS / REALITY 主入站占公网端口，握手不匹配的流量按 path / ALPN 交给回环上的无安全层子入站（`internal/core/xray/xray.go` 的 `withFallbacks` 只下发 dest / path / alpn / xver）。局限：仅 Xray；全部子入站共享主入站的一个 SNI 与一份证书（REALITY 时共享 dest），做不到「每个协议一个自己的域名」；匹配发生在主入站解开 TLS 之后，不是 SNI 层分发。

另注意两个易混点：面板设置里的 `Decoy`（`internal/panel/config.go`、`router.go`）是**面板自身** HTTP 服务对未知路径的处理（404 或反代一个 URL），与节点伪装无关；REALITY 的 `RealityDest`（`model.SecuritySpec`）是单入站借真实站点的证书与首页行为，入站独占端口时成立，进复用组后的口径见 D5。

### 各内核原生能力（以仓库内下发代码为准）

| 内核 | 原生 SNI 复用 | 依据 |
| --- | --- | --- |
| Xray 26.3.27 | 无 SNI 分发；只有上文 fallback（解 TLS 后按 ALPN / 路径） | `internal/core/xray/xray.go` `withFallbacks` |
| sing-box 1.13.5 | 无。一入站一 `listen` / `listen_port`，无 SNI 路由或共端口表达 | `internal/core/singbox/singbox.go` 入站渲染 |
| mihomo 1.19.15 | 无。`listeners` 逐条一端口 | `internal/core/mihomo/mihomo.go` `renderListener` |
| Hysteria 2.6.1 | 不适用：独立 UDP 服务 | `internal/core/hysteria` |
| Caddy 2.10.2（官方 stock 构建） | 只有 L7 站点（按域名终止 TLS 后按路径）；SNI 透传需第三方 caddy-l4 插件，官方构建不含 | `internal/panel/coreversions.go` 固定版本与来源 |

结论：**三条内核路线没有一条原生支持 SNI 分流**，只走原生必然分裂成「Xray 用 fallback 近似、其余内核没有」，且 fallback 的单 SNI 模型与目标形态不符。

### 入站模型、证书与冲突检测现状

- 入站模型：`model.Inbound` 是「一节点、一内核、一个监听端口」（`internal/model/inbound.go`），`UsesTCP / UsesUDP` 供冲突判定。面板保存时 `portConflict`（`internal/panel/inbounds.go`）拒绝同节点同端口同 L4 的任意两个入站——复用组需要一处刻意放行（先例：Caddy 前置的 `frontBlocker` 是「非前置入站占住公网端口则整组跳过并告警」的口径）。
- 证书体系：面板集中签发（`internal/panel/certs.go`，lego；DNS-01 走 Cloudflare、HTTP-01 由节点 Agent 在 80 端口应答，见 `internal/panel/acmehttp.go` 与 `internal/agent/acmehttp.go`），证书存 `certificates` 表，入站以 `CertificateID` 引用、构建 NodeSpec 时解析为 `CertPEM / KeyPEM` 下发。**每个 TLS 入站本来就有自己的证书**——SNI 分流缺的只是把它们藏到同一个公网端口后面。
- 下发通道：面板构建 `model.NodeSpec`（`internal/panel/spec.go`，其中 `buildFronts`、`resolveFallbacks` 都是在这一层改写入站的监听形态）——SNI 分流的成员改写天然同层：spec 构建时把成员入站改写到回环内部口，再把分流表随 NodeSpec 一起下发，面板与 Agent 的分工不用新开通道。

### 节点侧现状与 ClientHello 解析

- Agent 本身是 Go 程序，且有自监听先例：ACME HTTP-01 临时服务（80 端口应答）、限速回环监听等都是 Agent 自己起的监听，不是内核进程。在 Agent 里再加一个 L4 分流监听，形态上有先例、分发上零新增二进制。
- 端口占用检查现成：`internal/agent/portcheck.go` 已有「外来进程占用端口」的显式报错（注释原例即 nginx 占 443），复用组只需把归因扩展到「组」。
- 节点环境差异真实存在：备机 BWG-HK 的 443 被 1Panel 的 OpenResty 占着。分流器只能在组端口空闲时接管，**不抢占**——被占就报错、先腾挪或换组端口（见 D7）。
- ClientHello 解析没有现成件可借：仓库内的 realityscan 用 `tls.ClientHelloInfo` 做的是主动探测方向的事，没有「从裸 TCP 字节流里读 TLS 记录层、拼出 ClientHello 取 SNI、且原始字节一份不丢地透传」的解析器。分流器的记录层解析器需新写，这是本设计唯一的新增解析面，按实现红线约束（见文末）。

### 回落候选现状

组级回落没有现成实体：本地静态网站有（`model.WebsiteSpec`，经 Caddy 挂在统一 Web 入口下）；REALITY 的 dest 是单入站借站、入站独占端口时才成立。回落配置随复用组新建（见 D6），三形态里「本地网站」复用 WebsiteSpec 的站点内容，不新造站点管理。

## 设计决策

### D1 形态选型：混合分层，L4 分流由 Agent 内置分流器承担

两层并存、各管一段：**L7 维持 Caddy 前置不动**（HTTP 形传输继续按路径分）；**新增 L4 SNI 分流层，由 Agent 内置分流器承担**——监听组公网端口，读 ClientHello 取 SNI，按分流表把连接原样透传到成员的回环内部口或默认回落目标。

不用第三方分流二进制：nginx stream 多一个分发与升级物；caddy-l4 需 xcaddy 自构建，会改掉 stock Caddy 的固定版本管线（`coreversions.go`）。内置 Agent 的红利是**分流发生在内核之外，xray / sing-box / mihomo 一视同仁，批 1 即三内核可用**，且随 Agent 版本统一升级回滚。管理面只暴露「复用组」概念，不暴露分流器实现。代价是新增一段热路径代码（含调研所述新写的记录层解析器），用文末实现红线兜住。

### D2 模型：节点级 MuxGroup + Inbound.MuxGroupID

- 新增节点级实体 `MuxGroup`：名称、所属节点、公网端口、启用状态、默认回落配置。入站加 `MuxGroupID`（additive），为空表示不属于任何组。
- 成员 SNI **不另存**：从入站自身 TLS 的 ServerName / REALITY 的 serverNames 派生，单一事实来源，避免组配置与入站配置两处漂移。
- spec 构建时成员入站改写到回环内部口，编号续 `buildFronts` 的 `40000+n` 段（与前置段错开不重叠）；`model.NodeSpec` 新增 `Muxes` 字段携带分流表（SNI → 内部口、默认目标）下发 Agent。
- 冲突口径：`portConflict` 只对同组放行——同组成员共享组端口不算冲突；组端口与组外入站、其他组之间仍互斥。同节点同公网端口至多一个组、同组内 SNI 唯一，均在保存时校验。
- 组有成员时**拒绝停用组、拒绝改组端口**（先移出成员再改），避免一次操作打断全组入站。
- 红线：存量入站零改动——不进组的入站，其配置与下发逐字节不变。

### D3 成员资格：TCP + TLS / REALITY 入站

成员限「内核在回环上终止 TLS」的形态：TCP + TLS 入站（VLESS / Trojan / VMess 等）与 REALITY 入站。HTTP 形传输（WS / gRPC / XHTTP / HTTPUpgrade）**不进 SNI 组**，继续走 Caddy 前置按路径分。两层可叠加：某个 SNI 先被 L4 分到 Caddy 占的域名，Caddy 再按路径分给其后的 HTTP 形入站。

额外排除：启用了中转来源限制（RelayAccess）的入站不许进组——来源校验在防火墙层按公网端口做，进组后内核只见回环来源，校验会失效。保存时拒绝并说明原因。

### D4 证书与 SNI：每成员各自证书，分流器不持私钥

每成员各自证书就是现状（面板集中签发、入站以 CertificateID 引用），组不引入共享证书。保存校验：成员的每个 SNI 必须被其证书 SAN 覆盖（TLS 入站）或属于其 REALITY serverNames（REALITY 入站），不覆盖的拒绝保存并点名哪个 SNI。分流器只读 ClientHello 里明文的 SNI 字段做路由，**透传不解密、不持有任何私钥**——私钥仍只存在于各内核进程内。

### D5 REALITY 成员：口径不变，端到端借 dest 证书

REALITY 成员的 dest / serverNames 口径完全不变：分流器按 SNI 把连接原样递给回环上的 REALITY 入站，握手仍由入站与客户端端到端完成、借 dest 的证书与行为，分流层不参与密码学。结论：主动探测下看到的表现与单入站直连 REALITY **无差异**，复用不削弱 REALITY 本身的伪装。

### D6 回落：三形态，默认推荐外部真站透传

未命中任何成员 SNI 的流量（含**无 SNI、未知 SNI、非 TLS 流量，同走默认、不区别对待**，更抗探测；ClientHello 解析失败则直接关连接）按组配置回落，三形态：

1. **外部真站透传**：把连接原样递给一个真实站点的 443（默认推荐）。
2. **组内成员兜底**：递给组内某个成员入站（通常 REALITY 成员），由它按自身逻辑处理未命中流量。
3. **本地网站**：递给节点本地静态站（批 2 接入 WebsiteSpec）。

外部真站的风险如实写明：探测流量被转给无辜第三方（量小属实）；真站可能限速或封节点 IP；真站证书背后的 ASN / 地理与节点 IP 不匹配，深度探测下仍可疑。一句话定性：**SNI 分流解决的是同机共存，抗探测强度不高于单 REALITY**，回落只是让未命中流量有个像样的去处，不承诺更强伪装。

### D7 边界与冲突：说清占用者，不抢占

保存期校验把占用者说清楚（哪个入站、哪个组占了目标端口）。运行期沿用 Agent portcheck 的外来进程占用报错，扩展 Owner 标注把归因标到组——报错要能读出「哪个组的端口被谁占了」。分流器不抢占：组端口被节点自有服务占着时组不生效并报错，处理办法是先腾挪或换组端口（先例：BWG-HK 的 443 被 1Panel OpenResty 占用，那台机器须先处理 OpenResty 才能用 443 组）。

### D8 自带节点：v1 不开放

复用组是节点级共享资源，SNI 是节点级命名空间：不同用户的入站落在同一节点时，SNI 冲突与责任归因都跨用户，v1 先只给管理员节点。同先例：fallback 在用户自带节点的入站清洗（`sanitizePortalInbound`）里被剥离。自带节点是否开放留批 3 评估。

### D9 UDP 443：与 TCP 正交，本期不做 QUIC 复用

现有冲突判定本就 TCP / UDP 分开：Hysteria2 / TUIC 继续独立监听 UDP 443，与 TCP 复用组同号并存、互不影响，不需要为它们做任何组内表达。QUIC 的 SNI 藏在加密的 Initial 包里，做 UDP 侧 SNI 复用要解析 QUIC Initial，成本与收益不匹配，**不在本期**，批 3 再评估。

### D10 与既有机制的边界：一个入站只属一种共享方式

| 机制 | 层 | 分发键 | TLS 终止点 | 适用入站 |
| --- | --- | --- | --- | --- |
| Caddy 前置 | L7 | 路径 | Caddy | HTTP 形传输 |
| Xray fallback | L7（内核内） | ALPN / 路径 | 主入站 | Xray 子入站（全组共享一个 SNI / 证书） |
| SNI 分流（本设计） | L4 | SNI | 各成员内核 | TCP + TLS / REALITY（每成员各自 SNI / 证书） |

互斥规则：一个入站只属一种共享方式——进了 SNI 组，就不能再被 Caddy 前置，也不能当 fallback 子入站，保存时校验拦截。

## 实现红线（非拍板，批 1 必备）

分流器是节点公网端口上的新热路径，以下各项首批就必须有，不留到后续批次：

- ClientHello 读取超时：慢速 / 只连不发字节的连接不能长期占着分流器；
- 连接数上限与半开连接回收；
- 记录层解析器只读拼 ClientHello 所需的前若干记录并设字节上限，异常输入直接关连接；
- 透传全程不缓冲业务数据，解析只碰连接头部。

## 分批计划

- **批 1**：Agent 内置分流器 + MuxGroup 模型与 `NodeSpec.Muxes` 下发 + 面板组管理 + 回落两种（组内成员兜底 / 外部真站透传）。验收：真实节点上 REALITY（SNI A）与 Trojan-TLS（SNI B）同 443 并存可用；未知 SNI 命中外部真站、拿到真站证书；443 被外来进程占用时报错可读、能说清占用者。（实现对应 PR #479，本稿口径与之保持一致。）
- **批 2**：回落接本地网站（WebsiteSpec）；成员入站的分享链接 / 订阅逐项核对（沿 fallbackMasters 的改写先例，确认对外呈现的仍是组端口与成员 SNI）；入站模板支持组字段。
- **批 3**：评估 UDP / QUIC 侧复用、自带节点开放、分组流命中计数进探针。

## 需用户拍板

1. **分流器载体**：Agent 内置（推荐，D1）vs 独立小二进制（多一个分发物，换可独立升级 / 回滚）。
2. **端口范围**：只许 443 vs 任意端口、默认 443（推荐后者，组概念不绑死端口；批 1 已按此实现）。
3. **默认回落出厂值**：外部真站透传（推荐，D6）vs 组内 REALITY 成员兜底（零第三方牵连，但未命中流量进代理入站）。