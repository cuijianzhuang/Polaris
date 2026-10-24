# Snell 独立用户接入

## 范围与配置

Polaris 使用 **sing-box 1.14.0+ 的 Snell 多用户入站**，不安装只有共享 PSK 的独立 snell-server。需先升级 Agent，让节点完成一次配置拉取以报告 `snell-users-v1` 能力，再安装带 `with_v2ray_api` 的 sing-box。现有安装器优先下载 Polaris 带计数构建；若回落到无用户计数的官方构建，Snell 配置应用会明确失败，不会继续提供无法扣量的服务。面板也会拒绝已报告 `noUserTraffic` 的内核。

“入站”与模板编辑器可选 Snell，提供 v5/v6 和对应选项。内置“Snell 独立用户”模板使用随机高位 TCP 端口，无需域名或证书。现有分组、直接节点/入站授权、账号停用、到期、流量配额与倍率策略继续生效。

- 服务端 v5：客户端线协议为 v4，支持 `none` / `http` 混淆，不支持 v5 QUIC 代理模式。
- 服务端 v6：客户端 v6，提供 `default` / `unshaped` / `unsafe-raw` 流量整形。
- TCP 监听同时承载 TCP 与 UDP 代理业务，不额外开放 UDP 监听端口。
- 新增 Snell 不改变现有“全协议”模板，避免旧内核批量执行时产生新失败项。

## 凭据与撤销

`protocol.serverKey` 是每个入站生成的加密 PSK，沿用已有密钥加密存储、配置修订密文和脱敏预览。编辑留空保留原值，重新应用模板为每个节点生成独立 PSK。原生 `users` 每项使用用户 ID 作为 `name`、用户密码作为独立 `userkey`，同一入站不接受重复或空用户密钥。

加密 PSK 不能替代用户授权。原生 sing-box 在 `users` 为空时会切换到共享 PSK 模式，因此 **零授权用户时完全移除该监听器**，而非下发空数组。禁止通过入站高级 JSON 覆盖 `type/tag/users/psk/version/mode/obfs_mode/detour`，避免破坏身份、路由与计数。

用户变更通过现有配置发布和 Agent 收敛执行。sing-box 不提供 Polaris 可用的逐用户热更新 API，重载会关闭该内核现有连接，其他用户也需重连；不是只断开被撤销用户。Agent 在重载前读取、重置并入队统计，再重载。停用/到期/超额的生效依赖策略检查、配置发布和 Agent 成功应用；界面和下发错误仍按现有机制展示。不能把这一机制宣称为内核原生的即时硬配额。

## 客户端与订阅处理

本轮只验证并输出 **sing-box 1.14+ 原生 JSON**，保留 `psk` 与 `userkey` 两个必需字段。不把只包含共享 PSK 的 Surge/通用分享链接视为已验证的独立授权接入。Surge、Clash/mihomo、Loon、QuanX、普通分享链接会跳过 Snell，不泄露一个被误认为可直接使用的共享凭据配置。

已知低版本 sing-box/SFA/SFI/SFM/SFT，以及没有可识别兼容内核版本的上述客户端或 Hiddify，会过滤 Snell。显式 `?format=singbox` 的浏览器/手动导出可包含 Snell，由使用者导入 1.14+；无法从不携带内核版本的普通下载请求判断最终客户端。预发布客户端暂不声明兼容。

含 Snell 的 sing-box 输出使用内置转换器；外部转换器的分享链接回调无法表达 `userkey`，不能交给它转换后丢失独立身份。sing-box 来源解析、可复用处理组协议过滤和去重均支持 Snell；去重保留不同用户密钥、版本和模式的节点，来源中无 `userkey` 的共享 Snell 条目不导入。

Snell 目前是入站/订阅能力。L4 中转可透传；没有增加 Snell 类型的链式出站或 Snell 真实业务诊断实现。

## 验证

真实内核固定为 upstream **sing-box v1.14.4**，源码提交 `c005f35300ce76427dc285481ec5695f5b179cd2`，本地/CI 构建启用 `with_v2ray_api,with_clash_api`，测试仅访问回环目标，不依赖公网探测。

```bash
POLARIS_SNELL_BIN=/path/to/sing-box go test -count=1 -v ./internal/core/singbox ./internal/panel -run Snell
# 链接 Pro 源码后验证实际限速绕回路径
POLARIS_SNELL_BIN=/path/to/sing-box go test -tags pro -count=1 -v ./internal/agent -run Snell
```

覆盖：

- v5、v5 HTTP、v6 三种整形模式，两名用户 TCP 上下行与 UDP 回包。
- 读取真实计数，逐用户核对精确业务字节数；限速绕回也不重复计用户流量。
- 错误/空 `userkey`、重载后旧凭据、新旧授权用户隔离、已有连接关闭、零授权用户关闭监听。
- 面板生成客户端配置 → 实际传输 → 原生采集 → 流量收件箱 → 2 倍率结算 → 同批次重试去重 → 超额撤销订阅与内核凭据。
- 密钥落库加密/编辑脱敏/留空保持、旧 Agent 协商、旧内核拒绝、降级保护、危险高级配置拒绝、到期/停用联动。
- 来源往返转换、客户端过滤、独立凭据去重、手机和桌面编辑器与过时内核禁用。

UI 回归：启动 Vite 后运行 `POLARIS_WEB_TEST_URL=http://127.0.0.1:5187 python3 web/scripts/snell-ui-smoke.py`。此脚本使用模拟 API，只证明界面行为，不替代上述真实内核验收。
