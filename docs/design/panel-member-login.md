# 注册用户登录面板（受限角色 member）

> 状态：已批准（2026-11-24，用户拍板：注册用户能直接登录管理面板、新建只看自己节点的受限角色；D3 选案 B 完整自助、分两批落地），D1–D8 按本文执行。本设计只解决「同一注册账号进入面板」这一层，不改用户节点的既有语义（归属、审批、配额见 [user-nodes.md](user-nodes.md)）。
>
> 一句话方案：**把 `store.User` 做成面板的第三类登录主体（照 TGAdmin 的范式：新 claims 字段判别、独立签发函数、`authRequired` 第三分支、共用 AdminSession 与签名），角色值 `member`，走独立路由白名单；scope 不用登录快照，每请求按 `owner_id = 本人 AND review_state = 'approved'` 现算。登录方式复用邮箱验证码全链路，不给注册用户设密码。**

## 1. 角色

member 不是新人群，就是 user-nodes 里的「节点管理员」在面板侧的主体形态：同一个人、同一个 `store.User` 账号，门户是他的自助入口，面板 member 是他的第二个入口。

| 主体 | 凭据 | 域 | 说明 |
|---|---|---|---|
| **全局管理员**（`store.Admin`） | 密码 / TOTP / passkey / GitHub / SSO | 全部 | 现有，不动 |
| **节点操作员**（`store.Admin` role=operator、`store.TGAdmin`） | 密码 / Telegram | 管理员圈定的节点集 | 运营方的人，scope 是管理员分配的静态名单 |
| **member**（`store.User`） | 邮箱验证码 | 仅自己的节点（OwnerID = 本人且 approved） | 注册用户本人，scope 是归属关系本身，随审批实时变 |

member 与 operator 的根本区别：operator 的节点集是「管理员给的」，member 的节点集是「他自己的」——前者适合静态快照，后者必须动态现算（D4）。

权限矩阵（✓ 可，— 无；member 列按 D3 案 B 写，案 A 见 §7 注脚）：

| 操作 | 全局管理员 | operator | member（仅自己节点） |
|---|---|---|---|
| 看自己域内节点/指标 | ✓ | ✓ | ✓ |
| sync / rollback / 重启核心 | ✓ | ✓ | ✓ |
| 建/删/改名节点、拿安装命令 | ✓ | — | ✓（案 B，第 2 批） |
| 自己节点上的入站 CRUD | ✓ | — | ✓（案 B，第 2 批，字段收口见 D5） |
| 全局设置、用户管理、套餐/许可 | ✓ | — | — |
| requireSudo 的一切敏感操作 | ✓（复验后） | 部分 | —（永不，见 §12 红线 2） |

## 2. 目标与非目标

### 目标

- 已注册用户（邮箱注册的 `store.User`）能从面板登录页直接登录面板，登录后只看到自己的节点，进入与 operator 相同的 scoped 界面骨架。
- 准入可控：管理员不开开关就没人能进；用户被禁用/删除、节点被拒绝/停用，面板侧下一请求即生效，不等会话过期。
- 不新造凭据体系、不新造会话体系：凭据复用邮箱验证码，会话复用 AdminSession 与既有吊销/列表能力。

### 非目标

- 不给注册用户设登录密码，不进 Admin 的 bcrypt / TOTP / passkey 体系。
- 不动门户：门户（/u）继续存在且语义不变，面板登录是新增入口、不是替代；两套会话不合并（见 §12 红线 1）。
- 不改用户节点的审批、配额、隔离语义（user-nodes §5/§6/§8 全部照旧）；member 在面板里能做的事不超过他在门户里能做的事加上运维动作。
- 不给 member 任何全局面：设置、用户管理、分组、套餐、许可、中转、模板、探针策略，一律白名单外（§12 红线 3）。

## 3. 现状与复用点

- **面板已有两类主体，范式现成**：`store.Admin` 由 `issueToken`（auth.go:45）签发，claims（auth.go:27）为 `{Subject=Admin.ID, Version=TokenVersion, SID}`，会话落 `AdminSession` 服务端行，TTL 24h（`tokenTTL`），签名共用 `signToken`（auth.go:61）；`store.TGAdmin` 由 `issueTGToken`（tglogin.go:52）签发，靠 claims 的 `TG` 字段判别，`authRequired`（auth.go:138）在 :158 有独立分支加载。第三类主体照此再加一类即可，不需要新框架。
- **portal token 被明确拒绝**：`authRequired` 在 auth.go:148 对 `cl.User != ""` 直接 unauthorized。门户走 `issuePortalTokenVia`（portal.go:86）、cookie `polaris_u`、`PortalSession` 表与 `PortalEpoch` 代际（portal.go:387 每请求比对）。这条边界是对的，本设计保持它，只在旁边新开一扇门。
- **角色闸门是两道路由级白名单**：`adminRoleGuard`（admin_roles.go:45）按 role 查 `selfAccountRoutes` / `supportRoutes` / `scopedRoutes` 三张表（`map[string]bool`，键为 `"METHOD /path"`）；`scopeGuard`（tglogin.go:134）在白名单之上再查路径参数归属，现只识别 `/nodes/:id`、`/cores/:id`、`?node=` 三种模式。
- **operator 的 scope 是登录快照**：Admin 的 `NodeIDs` 在登录时写进请求上下文（auth.go:170/190 一带），handler 层普遍不查 scope——全靠两道 guard 兜底。推论：**白名单与 guard 的完整性就是安全边界本身**，member 的每一条新路由都要过这两道。
- **scopedRoutes 现状**（tglogin.go:107，共 23 条）：自身账号/会话路由 + 节点读 + 指标 + sync/rollback/restart + stats。不含入站路由，不含节点建/删——案 B 的写面是净新增，不能靠「顺手加进 scopedRoutes」解决（见 D5 三表隔离）。
- **邮箱验证码全链路现成**（mailcode.go）：发码/验码/限流/冷却、域名白名单 `registerDomainOK`（:74）、注册建号 `registerByMail`（:381，建 `store.User`、无登录密码、SelfService、发试用套餐），开关 `mailRegisterOn()`（:67）= SMTP 已配且 `portal_email_register` 开。面板邮箱登录只差一个新 purpose 与一个新签发动作。
- **sudo 有个坑**：`sudoOK`（loginguard.go:261）与 `sudo`（:287）对 tgAdmin 主体直接放行（理由：TG 在绑定设备上已确认身份且无密码）。member 同样无密码，但**绝不能照抄放行**——那等于注册用户免复验过全部敏感操作。必须显式排除（D8 红线 2）。
- **前端登录与角色驱动**：`Login.tsx` 无邮箱入口，登录方式显隐由 `GET /info`（router.go:185 `publicInfo`）下发的字段决定；`App.tsx:467` 登录后 `GET /auth/me` 取 role 驱动渲染，:487 的 role 白名单 `['owner','operator','viewer','support','finance']` 之外的 role 直接渲染报错卡；operator 的 me 带 `nodeIds` → `scoped=true` 走 scoped Layout（仅 /nodes、/account，侧边栏标签「节点管理员」）。member 全部沿用这条链，只加一个 role 值与一个登录方式。
- **User.Email 仅 index 非 unique**：唯一性靠 `registerByMail` 事务内 count + `lockAccountBindings` 维持。邮箱登录按邮箱找 User 时沿用同一口径，不假设唯一索引存在。

## 4. 数据模型（只增不删）

**本功能预计零新表、零新列。** 新增物只有三样非 schema 项：

| 新增 | 性质 | 说明 |
|---|---|---|
| claims 字段 `Mem`（`json:"mem,omitempty"`，值为 User.ID） | JWT 内容，不落库 | 照 claims 既有 `TG` 字段的范式判别主体类型 |
| 设置键 `panel_member_login`（bool，默认关） | settings KV | 进 editableSettings，见 D6 |
| AdminSession 行的 via 新取值（如 `mail-member`） | 既有列的新枚举值 | 主体 ID 列存 User.ID（`newSession` 的主体参数本就不绑定 Admin 类型），会话列表/吊销直接复用 |

归属与状态全部读既有字段：`Node.OwnerID` / `Node.ReviewState`、`User.Status`、`PortalEpoch`，Node/User 表一列不动。

> ⚠️ 标红约定：若实现中发现必须加列（例如想给 User 记面板登录时间），停下来单独评审，不许顺手加。本设计预判不需要。

## 5. D1 主体路线：User 作第三类面板主体

照 TGAdmin 范式落地：

- claims 加 `Mem` 字段（不设 `User` 字段——那是 portal claim，设了会在 auth.go:148 被第一关拒掉，且语义上会把两个面混起来）。
- 新签发函数 `issueMemberToken(u *store.User, gc)`，结构照 `issueTGToken`：填 `Subject=User.ID`、`Mem=User.ID`、`PortalEpoch=当前代际`，调 `newSession` 落 AdminSession 行（via=`mail-member`），`signToken` 签发，TTL 与管理员同为 24h。
- `authRequired` 在 TG 分支之后加第三分支：`cl.Mem != ""` → 加载 User、做 D6 的状态/epoch/开关校验、按 D4 现算 scope、置 `memberUser` 与 role=`member` 后放行。三个分支互斥，Admin/TGAdmin 的既有路径一行不改。

**明确排除「影子 operator」路线**（给每个注册用户自动建一个 role=operator 的 Admin 行）：

1. **密码体系对不上**：Admin 登录是 bcrypt 密码（+TOTP/passkey/GitHub/SSO），注册用户根本没有密码。影子行要么塞随机密码——用户永远走不了密码路径、sudo 复验无源可验；要么给 Admin 加第二套凭据，污染全体管理员的凭据模型。
2. **NodeIDs 静态双写漂移**：operator 的 scope 是 Admin.NodeIDs 的登录快照，用户的节点集随建/删/审批实时变，影子行必须靠钩子双写同步，漏一处就是越权（多了不该看的）或误杀（看不到自己的）。而 D4 的现算 scope 本来就不需要这份副本。
3. **生命周期跨表同步**：用户禁用/删除/改邮箱，都要同步影子行的禁用/删除/TokenVersion 失效。多一张必须永远一致的表，漏同步一次就是一个留着的后门管理员账号。

也不复用 portal token 直接放行：portal 会话是低权面（订阅自助），面板会话必须独立签发、独立会话行、独立吊销，只共享 epoch 这个失效信号（D6）。

## 6. D2 登录方式：邮箱验证码，不设密码

- 复用 mailcode 全链路，只加一个新 purpose（如 `panel`）区分发码场景：发码冷却、IP 日限、验码次数、域名白名单全部沿用既有常量与口径。
- 验码通过后按邮箱找 User：**已有账号** → 直接 `issueMemberToken`；**无账号且 `mailRegisterOn()`** → 先走 `registerByMail` 建号（试用套餐等语义与门户注册完全一致，同一个入口两张脸）再签发；**无账号且注册未开** → 统一失败文案，不泄露账号是否存在（沿用 mailcode 既有口径，见 §12 红线 7）。
- 可用条件是合取：`panel_member_login` 开（D6）且邮件体系可用（SMTP 已配）。`GET /info` 加一个开关字段（如 `memberMailLogin`），`Login.tsx` 据此显隐邮箱 tab——与现有登录方式的显隐机制一致。
- 不给注册用户设密码：不加密码字段、不进 TOTP/passkey 体系。将来若要密码登录，另行立项，不在本设计内预埋。

## 7. D3 可达面范围（已定：案 B 完整自助，分两批）

| | 案 A：只读 + 运维 | 案 B：完整自助（推荐） |
|---|---|---|
| 内容 | 看自己节点/指标 + sync/rollback/重启核心 + 自身账号/会话 | A + 节点建/删/改名/重置接入（拿安装命令）+ 入站 CRUD（字段收口见 D5） |
| 价值 | 能看能维护，但建节点、改入站还得回门户 | 面板内完成用户节点全生命周期，与门户能力对齐 |
| 风险 | 最低，批 1 即可上线 | 增量风险集中在 scopeGuard 的 inbound 反查一处（D5），且写面全部有门户先例可抄 |

推荐 B 的理由：用户节点的全生命周期在门户已经做全了（user-nodes 第 1/3 批），面板登录若只给只读，用户建节点还得回门户，「直接登录面板」只剩半截价值。B 按两批落地（§12），批 1 先以案 A 的面安全上线，写面在批 2 独立评审。

降级注脚：若最终拍板只做 A，§8–§11 不变，仅 memberRoutes 不放写面、批 2 取消；主体、动态 scope、前端骨架都不返工。

无论 A/B 都不进的面：全局设置、用户管理、分组/套餐/许可、中转、模板、探针策略、管理员与他人节点——白名单外一律 403。

## 8. D4 动态 scope：每请求现算

- `authRequired` 第三分支每请求现算 scope：`SELECT id FROM nodes WHERE owner_id = ? AND review_state = 'approved'`（`owner_id` 已有 index，user-nodes §4），结果注入 `nodeScope` 上下文——与 operator 静态快照同名同用法，`scopeGuard` 与 handler 无感知差异，改动只在分支内部。
- **代价**：member 每个 API 请求多一次索引查询。节点数受配额约束（社区版 3 台、许可放宽也有界），行数极小、毫秒级；Admin/TGAdmin/operator 请求走各自的分支，零影响。
- **收益**：审批与处置实时生效——管理员 reject/suspend 某节点，member 下一请求即不可达；用户新节点被 approve，下一请求即出现；节点删除即消失。不需要失效广播、不需要重签会话。operator「改了 NodeIDs 要重新登录」的老问题不在 member 身上重演。
- **列表读路径例外**：节点列表对 member 走专用分支，列自己全部节点（含 pending/rejected + 审批状态徽标，与门户「我的节点」同语义），否则用户看不到自己被拒的节点和原因。分工是：**读列表按 owner 全量（仅元数据），一切操作路径（sync/入站/指标/stats）只认 approved scope**。

## 9. D5 白名单与归属校验

- 新 role 值 `member`，新路由表 `memberRoutes`（与 `scopedRoutes` 并列的独立 `map[string]bool`）。`adminRoleGuard` 加 member 分支：只查 `selfAccountRoutes ∪ memberRoutes`，**不查 supportRoutes、scopedRoutes**——三表隔离，防止日后给 operator 加路由时顺手把 member 的面放大。
- 节点类路由复用 `scopeGuard` 现有三种模式（`/nodes/:id`、`/cores/:id`、`?node=`），判据换成 D4 现算的 owner scope 即可。
- **入站路由进白名单的前提（批 2）**：`scopeGuard` 新增第四种参数模式——路径含 inbound id 时反查 `inbound → node_id → node.owner_id == 当前 User 且节点 approved`，未命中按 404（与门户 `portalOwnedNode` 的「不属己即不存在」口径统一，防枚举探测）。反查与 D4 的 scope 现算在同一请求上下文里完成，不重复查库。
- **字段收口**：member 走管理员入站 handler 时，handler 按 role 分流净化请求，清单直接复用门户第 3 批的净化口径：剥离/拒绝 `security.certificateId`、`protocol.fallbacks`、`protocol.snellUserId`、`transport.acceptProxyProtocol`；请求结构禁 `groupIds` / `relayOnly` / `relayAccess` / `templateId` / CDN / `outboundTag` / `speedLimit`；建入站时分组参数传显式空集（与门户同一陷阱：传 nil 会绑默认组）。节点写操作同理只收门户安全字段集（displayName/address/region/flag/labels 等），内部 Name 由服务端生成，用户不可指定。
- `/auth/me` 对 member 返回 `role="member"`、`scoped=true`、`nodeIds=` 现算集合——仅供前端渲染，安全判据永远以后端每请求现算为准，前端拿到的 nodeIds 不作任何依据。

## 10. D6 准入与开关

- 新设置键 `panel_member_login`（bool，默认 `false`，进 editableSettings）：关 → `/info` 不下发邮箱 tab、panel purpose 的验码端点直接拒、**已签发的 member 会话在第三分支入口即时失效**（分支先查开关再查会话）。默认关、管理员显式开，是本功能敢上线的底气。
- **联动 `user_nodes_mode`**：mode=`off` 时 member 登录同样不可用——没有自有节点可管，登录只徒增攻击面。approval / open 均可。
- **账号状态即时失效**：第三分支每请求加载 User 后校验其存在且 `Status != disabled`（store.User.Status，models.go:186），删除/禁用下一请求即 401。与 user-nodes D6 互补：那边禁用用户时吊销其节点证书（管 Agent 通道），这边管面板会话。
- **PortalEpoch 比对**：member claims 带签发时的 `PortalEpoch`（复用 claims 既有字段），第三分支与 `PortalSessionEpoch` 现值比对，不一致即失效。这样用户改绑邮箱、门户侧重置会话代际时，面板会话跟着死，不留长尾。代价是第三分支多一次 epoch 查询，与 D4 的 scope 查询同请求内完成，可接受。

## 11. D7 前端

- `App.tsx`：:487 的 role 白名单加 `'member'`；:467 的 scoped 判定加 member（`role==='member'` → `scoped=true`），走与 operator 相同的 scoped Layout——侧边栏只有 /nodes、/account，标签沿用「节点管理员」。
- 路由集按批次扩：批 1 只有 /nodes（只读 + 运维）与 /account；批 2 在节点页内开建/删/改名与入站管理 UI，复用管理员组件但按 role 裁字段，裁剪清单与 D5 的字段收口是同一份。
- `Login.tsx` 加邮箱 tab：显隐读 `/info` 新字段；表单复用门户 portal-login 的验证码节奏（60 秒冷却、6 位码、错误文案不区分账号是否存在）；登录成功走既有 `onLogin` → `/auth/me` 流程，前端不为 member 写特殊分支。
- me 的展示名：member 无 Admin 式用户名，展示名以邮箱为准（以 User 现有字段为准，不为此加列）。

## 12. D8 安全红线（逐条要落测试）

1. **portal token/cookie 永远进不了面板 API**：带 `User` claim 的 token 在 auth.go:148 第一关即拒（现行为，保持）；面板只认 `Mem` 字段 + 对应 AdminSession 行存在且未吊销。member token 不得设置 `User` 字段，`polaris_u` cookie 不得能换面板会话。反向亦然：门户不认面板会话（现状不变）。
2. **member 永不过 requireSudo**：在 `sudoOK` / `sudo`（loginguard.go:261/287）里对 member 主体显式排除——不许照抄 tgAdmin 的直接放行。双保险：memberRoutes 本身不包含任何 requireSudo 路由。
3. **白名单外 403**：member 请求 `selfAccountRoutes ∪ memberRoutes` 之外的任何路由 → `adminRoleGuard` 403。默认拒绝：日后新增的管理员路由自动对 member 不可见，不靠逐条登记排除清单。
4. **归属逐类校验**：路径与参数里的 node id、core id、inbound id、`?node=` 全部经 scopeGuard 的现算 scope 或反查校验；他人资源与不存在资源同码（404），防枚举。
5. **写操作审计可追**：member 的写操作审计 actor 记 `member:<userId>`，与管理员、门户 actor 三者可区分；面板登录成功/失败都记审计。
6. **限流双层**：面板登录同时走 `loginGuard`（IP 失败计数/锁定）与 mailcode 自有限流（发码冷却、日限、验码次数），任一触发即拒；邮箱 tab 的发码/验码端点不得绕过 loginGuard 单独暴露。
7. **不泄露账号是否存在**：邮箱不存在 / 用户被禁用 / 注册未开，三种情况的登录文案与响应口径与 mailcode 既有一致，不回答「这个邮箱有没有账号」。
8. **会话边界**：member 会话 TTL 与管理员同为 24h；AdminSession 行在管理员的会话管理里可见（主体标注 member 来源）且可吊销；改邮箱 / PortalEpoch 变更后旧 member 会话即时失效（D6）。

## 13. 分批计划（每批一个 PR，测试全绿才提交）

| 批 | 内容 | 上线形态 |
|---|---|---|
| 1 | D1 第三类主体 + D2 邮箱登录（新 purpose + /info 字段 + Login 邮箱 tab）+ D4 动态 scope + D6 开关与即时失效 + D7 前端骨架（role/Layout/me）+ 只读运维白名单（案 A 面） | 即可安全上线：开关默认关，管理员开了才有人能进；member 能看能运维自己的节点 |
| 2 | D3 案 B 写面：节点建/删/改名/重置接入 + 入站 CRUD + scopeGuard inbound 反查（D5）+ handler 字段收口 + 节点页写 UI + 列表 pending 可见性收尾 | 门户自助能力的面板版，独立评审 |

### 验收清单（每条对应一个自动化测试，分布在各批）

1. member 用他人 node id 调读 / sync / rollback / 指标 → 全部 403/404；core id、`?node=` 同。
2. （批 2）member 用他人 inbound id 读/改/删 → 403/404；给入站塞 certificateId / groupIds 等收口字段 → 被剥离或拒绝，落库无该字段。
3. 自己 pending/rejected 的节点：操作类路由不可达；被 approve 后下一请求立即可达（不重新登录）；被 suspend 后下一请求即不可达。
4. 用户被禁用/删除 → 现有 member 会话下一请求 401；改绑邮箱（PortalEpoch 变更）→ 旧会话失效。
5. `panel_member_login` 关 → 邮箱 tab 不出现、验码端点拒、存量 member 会话失效；`user_nodes_mode=off` 时同样不可用。
6. 任取 requireSudo 路由用 member token 调 → 403；`POST /auth/sudo` 对 member 不放行。
7. portal token（`cl.User` / polaris_u）调面板 API → 401；member token 调门户 API → 与现状一致地被拒。
8. 白名单外抽查：设置、用户管理、分组、许可路由用 member token → 全部 403。
9. Admin / TGAdmin / operator 的登录与权限行为零变化（既有测试全绿即证）。

## 14. 风险与注意

- **白名单即边界**：handler 层不普遍查 scope 是既有事实，memberRoutes 每加一条都要同时确认 scopeGuard 覆盖了它的参数形态；批 2 的 inbound 反查没落地之前，入站路由一条都不许进白名单。
- **每请求现算的查询纪律**：第三分支的 scope/epoch/状态三查必须走索引、合并在同一请求内，禁止在 handler 里再各自补查放大成 N+1。
- **注册即入口**：面板邮箱登录在注册开启时同时是注册入口，等于把注册面从门户扩到了面板域名。缓解都在既有机制里：邮箱验证、IP 日限、域名白名单、默认审批制 + 配额（user-nodes §11 的滥用面分析同样适用）。管理员若只想让存量用户登录面板，可关 `portal_email_register` 而单开 `panel_member_login`——此时无账号邮箱一律统一失败（红线 7）。
- **迁移纪律**：零 schema 变更，无迁移、无同步升级风险；回滚就是关开关（会话即时失效）加回滚二进制。

## 15. 决策

| # | 决策 | 推荐 |
|---|---|---|
| D1 | 主体路线 | User 作第三类面板主体，照 TGAdmin 范式（新 claims 字段 + 独立签发 + authRequired 第三分支 + 共用 AdminSession/signToken）；排除影子 operator 路线（理由见 §5） |
| D2 | 登录方式 | 邮箱验证码，复用 mailcode（新 purpose）；不给注册用户设密码 |
| D3 | 可达面范围 | **案 B 完整自助**（2026-11-24 用户拍板），分两批落地（批 1 先上只读+运维面，批 2 开写面）；案 A 仅作降级注脚（§7） |
| D4 | scope 计算 | 每请求现算 `owner_id=本人 AND review_state='approved'`，不用登录快照；代价一次索引查询，收益审批/处置即时生效 |
| D5 | 白名单与归属校验 | 新 role `member` + 独立 memberRoutes 表（三表隔离）；入站进白名单的前提是 scopeGuard 新增 inbound→node→owner 反查；写面按门户净化清单收口字段 |
| D6 | 准入与开关 | 新设置键 `panel_member_login` 默认关、管理员显式开；`user_nodes_mode=off` 时不可用；每请求查 User 状态 + 比对 PortalEpoch，禁用/删除/改邮箱即时失效 |
| D7 | 前端 | App.tsx role 白名单加 member、走 scoped Layout；Login 加邮箱 tab（显隐读 /info 新字段，验证码节奏复用门户） |
| D8 | 安全红线 | §12 八条，逐条落测试；重点是 portal token 永不进面板、member 永不过 sudo、白名单外默认 403 |
