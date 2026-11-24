# 注册用户账号身份体系（密码 + 第三方绑定）

> 状态：已批准范围（2026-11-24，用户拍板：批 1 用户名+密码、批 2 GitHub 绑定/登录与绑定管理；SSO/OIDC、passkey 不进普通用户体系），D1–D8 按本文执行。本设计只解决「注册用户用什么凭据证明自己」这一层，不改用户节点的归属/审批/配额（[user-nodes.md](user-nodes.md)），不改 member 主体与会话范式（[panel-member-login.md](panel-member-login.md)）。
>
> 一句话方案：**同一个 `store.User` 账号，四种登录方式殊途同归——邮箱验证码（已有）、用户名+密码（新增独立 `PasswordHash` 列，与核心凭据严格分开）、Telegram（已有绑定）、GitHub（复用现有 OAuth App 与回调，靠服务端 state 分发用户流）；绑定管理复用既有 `account_binding` 的冲突口径与 `PortalEpoch` + `revokeMemberSessions` 的失效双件。**

## 1. 身份矩阵

| 登录方式 | 凭据载体 | 现状 | 本设计 |
|---|---|---|---|
| 邮箱验证码 | `User.Email` + mailcode | 已有：注册/登录/绑换（mailcode.go） | 不动，作为找回与二验的根 |
| 用户名+密码 | `User.Username` + 新增 `User.PasswordHash` | 无（`User.Password` 是核心凭据，不是登录密码） | 批 1 新增：注册/登录/找回/改密 |
| Telegram | `User.TelegramID` | 已有：门户绑定（portal_telegram_binding.go）+ TG 登录 | 不动，纳入绑定管理视图 |
| GitHub | 新增 `User.GitHubID` / `GitHubLogin` | 仅管理员有（Admin.GitHubID，githuboauth.go） | 批 2 新增：绑定/登录，复用同一 OAuth App |
| SSO/OIDC、passkey | — | 仅管理员体系有 | **非目标**：不进普通用户体系（见 §2） |

四种方式绑的是同一个 User：任一方式登录，落到同一个账号、同一份套餐与节点归属。绑定关系就是 User 行上的四个字段，不建绑定表。

## 2. 目标与非目标

### 目标

- 不想用邮箱（或邮箱不可用）的用户能以用户名+密码注册并登录，面板（member）与用户中心两边都认。
- 已有账号能再绑 GitHub 并用它登录；四种方式的绑定状态在一个地方看全、管全（绑定管理）。
- 密码与第三方登录的安全口径不低于现有邮箱链路：同样的 IP 限流、同样的会话失效纪律、同样的审计可追。

### 非目标

- SSO/OIDC 不进用户体系：管理员侧 SSO（sso.go）的主体是 Admin，企业 IdP 对接是管理员面的事，用户侧不复刻。
- passkey 不进用户体系：管理员侧已有 passkey 体系，用户侧本期不做（绑定管理的位置先留给四种既有/新增方式）。
- 不给 User 加 TOTP；不做账号级失败锁定列（理由见 D1）。
- 不用 GitHub 返回的邮箱自动绑邮箱（见 D6），不做跨账号自动合并（邮箱绑定的既有合并语义除外，见 D7）。

## 3. 现状与复用点

- **User 现有字段**（models.go:172 起）：`Username`（size:64，**已有 uniqueIndex**，:174）、`Password`（size:64，:176——**核心凭据**，`ToModel` 时下发给代理核心生成订阅凭据，与登录无关）、`Email`（size:254、仅 index 非 unique，注释 lower case）、`TelegramID`（index）、`SelfService`。**没有登录密码哈希、没有 GitHub 字段。**
- **Username 唯一索引是大小写敏感的**：SQLite / Postgres 的文本唯一索引默认按字节比较，`Alice` 与 `alice` 今天可以共存。存量用户名来源有三：管理员后台建号（`newUser`，api_users.go:225，只靠 DB 唯一索引兜底、无格式校验）、邮箱注册（local 部分强制小写 + `_` + 随机后缀，mailcode.go:393 一带）、商店建号。**结论：大小写不敏感唯一目前没有任何一层在保证**，D2 只能先在应用层做，DB 层函数唯一索引要等存量去重核查后另行立项。
- **邮箱注册建号**（`registerByMail`，mailcode.go:381）：`register_ip_daily`（默认 3，`countDaily("regip:"+ip)`）+ 许可证 `LimitUsers`（含 `userCapFull`，license.go:246）双限额；事务内 `lockAccountBindings` 串行化后 count 邮箱查重；挂 `register_trial_plan` 试用套餐，无套餐则建号即过期；`SelfService=true`；审计 `user.register` + 通知。密码/GitHub 注册照此再写一个同构函数即可，不改它。
- **邮箱绑定已有完整冲突机器**（account_binding.go）：`portalMailBind`（mailcode.go:325）在 `lockAccountBindings` + `lockBindingUsers` 下走 `planVerifiedMailBinding`——目标邮箱已属另一账号时按「对方是否已开通服务」决定合并/退休或报 `errBindingIdentity` / `errBindingConflict`（:21-23）。绑定冲突不是新问题，D7 只需把 GitHub/TG 接进同一错误口径（`bindingError`）。
- **Telegram 绑定形态**（portal_telegram_binding.go）：`POST /u/telegram/bind/start|cancel|poll`（router.go:143-145），待绑定请求存 eph 并由 `sameBindingOwner` 绑死当前门户会话，机器人在聊天里向用户确认后落 `TelegramID`。已绑再绑直接 `errBindingIdentity`。
- **管理员 GitHub OAuth**（githuboauth.go）：配置是**全局单份**设置键 `github_client_id` / `github_client_secret`（Box 密封，`githubCreds()` :71、`githubEnabled()` :75）；回调 URL 只有一个，`githubCallbackURL()`（:80）拼 `<base><APIBase>/auth/github/callback`，路由 `GET /auth/github/callback`（router.go:183）。**GitHub OAuth App 只允许登记一个回调 URL**——login 与 bind 两种意图今天就靠服务端 state 多路复用：`oauthState`（:30，eph 单次消费）带 `Mode`（login/bind）+ `AdminID`，SSO 还共用此结构（`Provider` 字段）。回调里 bind 先查 `git_hub_id` 是否被其他 Admin 占用（:165），login 按 `git_hub_id` 查 Admin（:185），命中后 `finishLogin` 签发。用户流照此扩展 state，不新开回调（见 D6 的标红说明）。
- **管理员密码口径**（可沿用件）：初始密码 12–72 字节、首尾不许空格（admin_roles.go:168/375），`bcrypt.GenerateFromPassword(..., DefaultCost)`；时序防护有 `dummyHash` 先例（auth.go login，用户不存在也跑一次 bcrypt）；`passwordMatches`（auth.go:450）容忍仅首尾空白差异的密码。账号级锁定是 Admin 行的 `FailedAttempts`/`LockedUntil`（5 次/15 分钟）——**用户侧不照抄这一件**（见 D1）。
- **IP 限流与主体无关**：`loginGuard`（loginguard.go:141）+ `recordIPFailure`（:96）纯按 IP 计数、持久化封禁（窗口 15 分钟、阈值读设置 `login_ban_failures` 默认 10、封禁递增至 7 天），已被面板邮箱端点挂用（router.go:190-191 的 `/auth/mail/code|verify`）。用户密码登录直接同挂。
- **会话失效双件现成**：`PortalSessionEpoch` 表（models.go:502）+ `lockPortalEpoch`（portal_sessions.go:18）——改绑邮箱时 epoch+1（account_binding.go:165-169），门户会话与 member 会话（签发时盖 epoch、每请求比对，memberlogin.go:120/135）一起死；`revokeMemberSessions`（memberlogin.go:197）在禁用/删除用户时按 via 精确吊销面板会话。改密失效照此双件齐发。
- **设置键登记处**：可保存键白名单在 api_misc.go:132（`portal_email_register`、`register_*`、`panel_member_login` 都在此），默认值表在 :158。新键 `register_password` 两处都要加。设置 UI 在 SubTemplates.tsx 的同一设置区（`portal_email_register` 开关旁）。

## 4. D1 数据模型（只增不删）

| 新增列 | 批次 | 定义 | 说明 |
|---|---|---|---|
| `User.PasswordHash` | 批 1 | `gorm:"size:100" json:"-"` | 与 Admin.PasswordHash（models.go:484）同规格；空 = 未设登录密码 |
| `User.GitHubID` | 批 2 | `int64`，`gorm:"index"` | 与 Admin.GitHubID（:492）同型；0 = 未绑 |
| `User.GitHubLogin` | 批 2 | `gorm:"size:64"` | 展示用，随绑/解绑写清 |

加列走既有 AutoMigrate + schema stamp 口径（只增不删，符合迁移纪律），无数据回填：存量用户 PasswordHash 为空即「无密码登录方式」，不强制设密。

> ⚠️ 标红：`User.Password`（核心凭据）与 `User.PasswordHash`（登录密码）**严格分开、永不互填**。前者出现在订阅/核心配置里、管理员编辑用户时可见可改；后者只进登录校验、任何接口不回显。管理员编辑用户的 UI 必须把两个字段分开命名（「订阅凭据」/「登录密码」），防止运营误填把登录密码写进核心凭据（会随订阅下发，等于泄露）。

**不加失败计数/锁定列**：账号级锁定（Admin 的 FailedAttempts 模式）对用户面是负资产——用户量大且用户名半公开，任何人知道用户名就能把真主锁死。用户侧只做 IP 级限流（loginguard，跨主体共用、持久化）+ 登录失败审计，运营观察后再议账号级措施。

## 5. D2 用户名规则

- 格式：3–32 字符，仅 `[a-zA-Z0-9_.-]`，不以 `.`/`-` 开头或结尾。注册表单前端同规则预检，后端为准。
- 唯一性：**大小写不敏感唯一**。批 1 在应用层保证：注册/改名在 `lockAccountBindings` 事务内以 `LOWER(username) = ?` 查 User 表查重，命中即拒。DB 层的大小写敏感 uniqueIndex 继续兜「完全同名」。
- **与管理员命名空间互查**：自助注册的用户名若与任一 Admin 的 `LOWER(username)` 相同，同样拒绝——防注册用户冒充管理员名（两张表本是两个命名空间，但用户看到的名字不能撞）。管理员后台建号不受此限（运营自担），但建议同查，只警告不拦截的部分留给实现时按现口径决定，文档口径是：自助侧硬拒。
- 保留名清单（自助注册禁用，含大小写变体）：`admin`、`root`、`support`、`system`、`official`、`moderator`、`service`、`api`、`polaris`、`null`、`undefined`。清单进代码常量，不进设置。
- 系统生成名豁免：邮箱注册生成的用户名（小写 local + `_` + 随机后缀）不走保留名与互查（带后缀不可冒充、且是既有行为），但仍受格式与 DB 唯一约束。
- **存量大小写重复的处置口径**：若存量已有 `Alice`/`alice` 这类重复，密码登录按 `LOWER` 查询会命中多行——**fail closed**：拒绝登录、记审计告警，由管理员改名消解后再登。注册查重在锁内做，不会新增重复。DB 函数唯一索引（`UNIQUE(LOWER(username))`）列为后续项：先决条件是全量扫描确认存量无大小写重复，本设计不含。

## 6. D3 密码规则与找回

- 规则：**8–72 字节**（上限与 bcrypt 对齐），首尾不许有空格（同管理员口径）。下限与管理员的 12 不同：用户面摩擦优先，配合 IP 限流与可选邮箱二验，8 可接受；差异在此写明，不改管理员侧。
- 哈希：`bcrypt` DefaultCost；校验复用 `passwordMatches`（含首尾空白宽容，与管理员登录行为一致，不另开分叉）。
- 失效纪律：**设密/改密/重置成功后，该用户全部会话失效**——事务内 PortalEpoch+1 且提交后 `revokeMemberSessions`，门户与面板两边一起死，与改绑邮箱同口径（§3）。当前发起改密的会话也不例外，改完重新登录（与管理员改密「其他会话全死、本会话换新 token」的差异在此写明：用户侧不做会话接续，简单优先）。
- 找回（自助）：仅限已绑邮箱者。复用 mailcode 加新 purpose（如 `reset`）：发码 → 验码同请求提交新密码（`checkMailCode` 消费码后在同一 handler 内完成设密 + 失效双件），不发独立重置票据、不走链接。面板与用户中心各一个入口，内核共用。
- 找回（无邮箱）：没有自助路径，页面明示「未绑定邮箱，请联系管理员」。管理员处置口：用户管理编辑用户时可**设置/清除其登录密码**（写 PasswordHash，审计 `user.password_reset` 沿用管理员侧同名动作口径）——这是 D7「绑定概览只读」的唯一例外，属账号处置而非绑定管理。

## 7. D4 注册开关与限额

- 新设置键 **`register_password`**（bool，默认 `false`）：进 api_misc.go:132 白名单与 :158 默认值表；设置 UI 与 `portal_email_register` 并列在同一区（SubTemplates.tsx），文案成对出现：「邮箱注册」「用户名密码注册」两个独立开关，互不依赖（密码注册不需要 SMTP）。
- 面板侧不另设开关：沿用 member 准入 `memberLoginEnabled()`（memberlogin.go:93，`panel_member_login` 开且 `user_nodes_mode != off`）。面板上的密码**注册**要求该准入为真；密码**登录**只要求账号已设密码 + 面板准入，不受 `register_password` 影响（开关只管「能不能新建账号」，不管「已有账号能不能登」——邮箱侧同理，`portal_email_register` 关时存量用户仍可验证码登录）。
- 限额全复用：`register_ip_daily`（`countDaily("regip:"+ip)`，与邮箱注册共用同一计数键——同一 IP 当天不管用哪种方式注册合并计数，防绕限额）、许可证 `LimitUsers` / `userCapFull`、试用套餐 `register_trial_plan`。
- 建号函数 `registerByPassword(username, password, email?, ip)` 与 `registerByMail` 并列同构：锁内查重（用户名 LOWER + 与 Admin 互查 + 保留名）→ `newUser` → 写 PasswordHash、SelfService=true → 套餐/过期口径、审计（`user.register`，via=password）与通知全部同邮箱路径。邮箱参数可选：填了只作联系信息落 `Email`，**不验唯一、不发验证码**（无所有权证明，见 D8 红线 8）；后续可用 mailcode bind 流程正式绑定/换绑（那时才走冲突机器）。

## 8. D5 登录面

| 面 | 端点 | 签发 | 备注 |
|---|---|---|---|
| 面板 | `POST /auth/member/password` {username, password} | `issueMemberToken` | 挂 `loginGuard`（同 /auth/mail/* 的挂法）；先查 `memberLoginEnabled()` 准入 |
| 用户中心 | `POST /u/password/login` {username, password} | `issuePortalTokenVia`（portal 会话） | 在 portalGate 内；与邮箱登录并列第三种表单 |

- 校验流程（两面共用内核）：按 `LOWER(username)` 查 User（`is_service = false`）→ 命中多行按 D2 fail closed → PasswordHash 为空、密码不符、用户被禁用，三者**统一文案**「用户名或密码错误」；用户不存在时跑一次 `dummyHash` 保持时序一致（管理员 login 先例）。每次失败 `recordIPFailure` + 审计 `user.password_login_failed`（actor 记提交的用户名，不记密码）。
- 用户中心登录页（portal-login.html）加「用户名+密码」表单，与邮箱验证码、Telegram 并列；面板 Login.tsx 在邮箱 tab 旁加密码 tab（显隐：面板准入开即显示，不依赖 `register_password`）。注册入口：两面的注册页/注册 tab 内加「用户名+密码」方式，受 `register_password` 控制显隐与端点可用性；用户中心注册端点 `POST /u/password/register`，面板注册走同一建号函数、由面板注册入口调用（端点 `POST /auth/member/register`，准入同登录）。
- **红线：两套登录端点永不互通。** 管理员 `POST /auth/login` 只查 Admin 表（现状，一行不改），用户凭据在那儿永远失败；用户侧两个端点只查 User 表，管理员凭据（Admin 行）在那儿不存在。代码上不共用查表函数，只共用 `passwordMatches`、限流与审计件——防止日后有人「顺手」加跨表回退。

## 9. D6 GitHub 用户流

> ⚠️ 标红（与早期设想的修正）：**不新增回调路径。** GitHub OAuth App 只有一个回调 URL，已经是管理员流的 `…/auth/github/callback`；在 GitHub 后台再登记第二个路径不会生效。用户流改为：**入口端点分开、回调共用既有路径、靠 `oauthState` 新增字段分发**——与现有 login/bind 多路复用同一模式，GitHub 后台零改动、部署零注意。若将来要硬隔离用户流，只能注册第二个 OAuth App 并新增设置键 `github_user_client_id` / `github_user_client_secret`，列为备选、本期不做。

- `oauthState` 扩展（eph 服务端单次消费，浏览器只见不透明 key，天然防篡改）：加 `Kind`（`""`=admin 既有 / `"user"`）、`Target`（`panel` / `portal`）、`UserID`（bind 意图时的当前用户）。`githubCallback` 在 `take` 后先按 Kind 分发：admin 走现有 switch 一行不改，user 走新 `githubUserCallback`。
- 入口端点：面板 `POST /auth/github/user/login-url`（公开 + loginGuard）、`POST /auth/github/user/bind-url`（member 会话内，memberRoutes 定向加白名单）；用户中心 `POST /u/github/login-url`、`POST /u/github/bind-url`（portalGate + 已登录）。四个入口都只做一件事：校验前置条件后 `put` 一个带 Kind/Target/Intent 的 state 并返回授权 URL（拼法同 `githubAuthURL`，scope 沿用 `read:user`、`allow_signup=false`）。
- login 意图回调：`githubIdentity` 换得 GitHub 身份后按 `git_hub_id` 查 User（`is_service = false`）：
  - 命中且状态正常 → 按 Target 签发：panel 先查 `memberLoginEnabled()` 再 `issueMemberToken`；portal 走 `issuePortalTokenVia`。回跳面与管理员流同构（fragment 带 token / 错误）。
  - 未命中 → **不自动绑邮箱、不按 GitHub 邮箱找账号**（GitHub 返回邮箱不等于已验证的主邮箱，按邮箱串号是账号劫持面）；走建号判定：该部署注册开放（`portal_email_register` 或 `register_password` 任一为开——注册开放是部署级事实，与具体方式无关）且限额未满 → `registerByGitHub` 建号（用户名取 GitHub login 净化，冲突加随机后缀，同 registerByMail 生成口径；SelfService、试用套餐、IP 日限、审计 via=github 全同构）后签发；注册未开 → 回跳报错「该 GitHub 账号未绑定任何账号，请先注册后在账号设置中绑定」。
- bind 意图回调：state 里的 UserID 必须与回调落点会话的用户一致（面板 member 会话 / 门户会话分别校验，仿 `sameBindingOwner` 的绑死口径；不一致整单作废）。落库前在 `lockAccountBindings` 下查 `git_hub_id` 是否已被其他 User 占用：占用 → `bindingError` 口径拒绝（**不合并**——与管理员 bind 的直接拒绝一致；账号合并只存在于邮箱路径，因为邮箱所有权经验证码证明过）。成功后写 GitHubID/GitHubLogin + 审计。
- 前置条件：`githubEnabled()` 为假（管理员没配 OAuth App）时用户流整体不可用，入口端点明确报错、前端不显示 GitHub 按钮（`/info` 的 `githubLogin` 字段已存在，语义扩为「GitHub 登录可用」，用户侧按钮据此显隐）。

## 10. D7 绑定管理

- **状态口径**（全部读 User 行，无新表）：邮箱 = `Email != ""`（附地址脱敏展示）、密码 = `PasswordHash != ""`、Telegram = `TelegramID != 0`、GitHub = `GitHubID != 0`（附 GitHubLogin 展示）。
- **两面同构 UI**：用户中心账号区（现有邮箱/Telegram 绑定 UI 所在页）加「登录方式」区块；面板 `/account` 页（member 的 scoped Layout 已含此路由）加同一区块。两边调同一组语义 API：用户中心走 portalGate 下的 `/u/bindings`（GET 状态）+ 各方式既有/新增动作端点；面板走 API 组内对应端点并**定向加入 memberRoutes**——这是对 panel-member-login 批 2「self 路由收窄」的一次点名放开，仅限绑定管理这一组，不恢复其他 Admin 向自助路由。
- **绑定动作各自复用既有流**，不新造：邮箱 = mailcode bind（code+verify purpose=bind，冲突走既有合并机器）；Telegram = `/u/telegram/bind/*`（面板侧用户经机器人流绑定的是同一个 TelegramID，面板区块只展示状态与解绑，绑定引导去用户中心完成——机器人确认流在门户侧，不在面板复刻）；密码 = 设密端点，二验要求见下；GitHub = D6 bind 流。
- **二验口径**：绑定新方式、解绑、设密/改密都属敏感操作，要求**近期二验**——当前密码或邮箱验证码任一通过即可（无密码且无邮箱的用户只能用现存最强方式：TG/GitHub 登录态 + 该方式自身的确认流）。面板侧不走 requireSudo（member 永不过 sudo 的红线不变），二验在 handler 内自理。
- **解绑红线**：四种方式中已绑数量 ≤ 1 时，拒绝解绑最后一个（错误文案明示「至少保留一种登录方式」）。解绑 GitHub 清 GitHubID/GitHubLogin；解绑邮箱走既有换绑/解绑口径且 epoch+1；解绑密码清空 PasswordHash。三者都记审计。Telegram 解绑沿用既有门户口径（如已有），面板侧同。
- **管理员视图**：用户管理列表/详情加绑定方式概览——四个布尔徽标（邮/密/TG/GH），**只读**：管理员不能替用户绑/解绑第三方（那等于替他接管身份），处置手段仍是禁用/删除 + D3 的代设密码。

## 11. D8 安全红线（逐条要落测试）

1. **两套登录端点永不互通**：用户凭据调 `POST /auth/login` 必失败；管理员凭据调 `POST /auth/member/password` 与 `/u/password/login` 必失败。两边查表函数不共用、无跨表回退。
2. **限流共用**：密码登录、GitHub 入口、找回端点全部挂 `loginGuard`，失败走 `recordIPFailure`，与管理员/邮箱链路共享同一 IP 封禁池（`login_ban_failures` 阈值）；不因换端点而重置计数。
3. **登录不泄露**：用户不存在 / 未设密码 / 密码错 / 已禁用 / 大小写重复异常，五种失败同文案同时序（dummyHash 兜底）。注册侧的例外与取舍见红线 8。
4. **绑定防劫持**：一切 bind 意图的 state/待绑定请求必须绑死发起时的登录用户（GitHub 走 oauthState.UserID、TG 走 sameBindingOwner、邮箱走 `bind:<userID>` purpose），回调/轮询时会话用户不一致即整单作废；login 意图绝不接受 bind 语义，反之亦然。
5. **解绑不能到零**：任何路径（UI、直调 API、管理员代操作）都不能让账号剩下零种登录方式；管理员代设密码不算绑定动作，不受此限（那是给账号加方式）。
6. **凭据变更全失效**：设密/改密/重置、换绑/解绑邮箱后，PortalEpoch+1 与 revokeMemberSessions 必须成对出现（只做一半就是留长尾会话）；GitHub/TG 解绑后以该方式签发的存量会话同样失效（走同一双件，不单开）。
7. **审计全覆盖**：注册（via 区分 email/password/github）、密码登录成功/失败、设密/改密/重置、每种方式的绑/解绑，全部落审计；actor 沿用所在面口径（门户 `portalActor`、面板 `member:<userId>`、找回记 `mail:<username>` 同邮箱登录先例）。
8. **注册防枚举的取舍（写明不对称）**：登录与发码路径不给可区分信号（既有口径，保持）。但注册表单里**用户名占用必须明确提示**——用户名是公开标识（订阅、节点归属都会露出），不提示用户根本完不成注册，枚举面由 IP 日限兜底。与之相对，密码注册里可选填的邮箱**不验唯一、不因占用报错**：邮箱是非公开标识且填写时无所有权证明，报错即泄露「此邮箱已注册」；邮箱正式占位只发生在 mailcode bind（验证码证明所有权之后），与既有口径自洽。

## 12. 分批计划（每批一个 PR，测试全绿才提交）

| 批 | 内容 | 上线形态 |
|---|---|---|
| 1 | D1 的 PasswordHash 列 + D2 用户名规则 + D3 密码/找回 + D4 `register_password` 与 `registerByPassword` + D5 两面登录/注册端点与表单（面板密码 tab、用户中心密码表单与注册方式）+ 管理员代设密码 | 开关默认关；开了才出现密码注册，存量用户可由管理员代设或自助设密后用密码登录 |
| 2 | D1 的 GitHubID/GitHubLogin 列 + D6 GitHub 用户流（state 扩展、四个入口、共用回调分发、registerByGitHub）+ D7 绑定管理（两面 UI、状态端点、解绑红线、管理员只读概览） | GitHub 按钮随 `githubEnabled()` 显隐；绑定管理上线即四种方式全量 |

### 验收清单（每条对应一个自动化测试，分布在各批）

批 1：
1. 密码注册成功 → 面板与用户中心两边都能用用户名+密码登录，落到同一账号。
2. 大小写变体注册（先注册 `Alice` 再注册 `alice`）被拒；保留名、与管理员同名（大小写不敏感）被拒。
3. 改密后旧门户会话与旧 member 会话下一请求全失效；邮箱码找回全流程走通并同样全失效；未绑邮箱者找回被拒且文案引导找管理员。
4. 连续密码失败达 `login_ban_failures` 阈值 → IP 被封，换邮箱验证码端点同 IP 一样被拒（共用封禁池）。
5. 用户凭据调 `/auth/login` 失败；管理员凭据调两个用户端点失败（红线 1）。
6. `register_password` 关 → 注册端点拒、注册方式不显示；已设密码用户登录不受影响。IP 日限与试用套餐对密码注册同样生效（合并计数：同 IP 先邮箱注册 3 个再密码注册被拒）。
7. 核心凭据回归：设登录密码后订阅拉取、核心配置里的 `User.Password` 不变（两字段互不串扰）。

批 2：
8. 已绑 GitHub 的用户从面板与用户中心两面 GitHub 登录成功；未绑 GitHub 登录在注册开时自动建号、注册关时报错引导先注册再绑定。
9. GitHub bind：state 被另一用户的会话冒用回调 → 作废；目标 GitHub 已被他人绑 → 拒绝且不合并；绑成功后 GitHub 登录命中本账号。
10. 解绑到最后一个登录方式被拒；解绑 GitHub 后该 GitHub 再登录落到「未绑定」分支；解绑/绑定的审计齐全。
11. 管理员用户详情能看到四种绑定状态徽标，且无任何可写的绑定操作入口。
12. Admin / TGAdmin / operator / member 既有登录与权限行为零变化（既有测试全绿即证）；管理员 GitHub 登录/bind 回归不变（state 分发不串扰）。

## 13. 风险与注意

- **LOWER 查询走不了索引**：用户名查重/登录按 `LOWER(username)` 查是全表扫描。注册低频无碍；密码登录是热点前的冷路径（每登录一次），用户量级下可接受。若将来用户量上到万级，再议函数索引（先决条件见 D2）。
- **存量大小写重复**是唯一的历史包袱：批 1 上线前建议跑一次只读扫描（`GROUP BY LOWER(username) HAVING COUNT(*)>1`）摸清有没有，有则先改名消解——扫描只是建议项，不阻塞开发，但登录 fail closed 的行为要让运营知道。
- **回调共用的串扰面**：githubCallback 按 Kind 分发后，admin 分支必须先于一切用户逻辑且不读用户字段；state 是服务端单次消费，串扰风险主要在实现把两个 switch 写混——验收 12 专门钉住。
- **迁移纪律**：两批各加列一次，均只增不删、无回填；回滚 = 关开关（`register_password` / 面板准入）+ 回滚二进制，残留列与已设的 PasswordHash 不影响旧版本行为（旧代码不读新列；注意旧版本下已用密码注册的账号仍可邮箱/TG 登录，不会被锁在门外）。

## 14. 决策

| # | 决策 | 推荐 |
|---|---|---|
| D1 | 数据模型 | 只加三列：`PasswordHash`（批 1）、`GitHubID`/`GitHubLogin`（批 2），规格对齐 Admin 同名字段；不加失败计数列（账号级锁定对用户面是负资产，只做 IP 限流） |
| D2 | 用户名规则 | 3–32 字符 `[a-zA-Z0-9_.-]`；大小写不敏感唯一先在应用层（锁内 LOWER 查重 + 与 Admin 互查 + 保留名清单），DB 函数唯一索引等存量去重后另议；存量重复时登录 fail closed |
| D3 | 密码规则 | 8–72 字节 bcrypt（下限与管理员 12 的差异写明）；设密/改密/重置后 PortalEpoch+1 + revokeMemberSessions 全会话失效；找回只走邮箱验证码，未绑邮箱找管理员代设 |
| D4 | 注册开关与限额 | 新键 `register_password` 默认关、与 `portal_email_register` 并列同区、互不依赖；面板侧沿用 memberLoginEnabled 准入；限额（IP 日限合并计数/LimitUsers/试用套餐）全复用 |
| D5 | 登录面 | 面板 `POST /auth/member/password` → issueMemberToken；用户中心 `POST /u/password/login` → portal 会话；管理员 `/auth/login` 不动，两套端点永不互通（红线 1） |
| D6 | GitHub 用户流 | 复用同一 OAuth App 与既有回调，`oauthState` 加 Kind/Target/UserID 分发（**不新开回调路径**——GitHub 只认一个）；login 未命中按注册开关决定建号或引导绑定；bind 占用直接拒绝不合并；不用 GitHub 邮箱自动绑邮箱 |
| D7 | 绑定管理 | 两面同构「登录方式」区块（面板侧定向加入 memberRoutes）；绑定复用各方式既有流、敏感操作要二验；解绑不能到零；管理员只读概览 + 代设密码是唯一处置例外 |
| D8 | 安全红线 | §11 八条，逐条落测试；重点是端点永不互通、限流共用一池、绑定防劫持、凭据变更双件失效、注册防枚举的不对称取舍 |
