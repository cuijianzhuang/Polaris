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
- 下发通道：面板构建 `model.NodeSpec`（`internal/panel/spec.go`，其中 `buildFronts`、`resolveFallbacks` 都是在这一层改写
...[truncated 8265 chars]