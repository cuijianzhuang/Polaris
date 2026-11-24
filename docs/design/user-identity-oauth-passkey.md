# 注册用户身份体系批 3（SSO + passkey）

> 状态：待批准（2026-11-24 拟定）。本文是 [user-identity.md](user-identity.md) 的续篇：批 1（用户名+密码，PR #449）、批 2（GitHub 绑定/登录与绑定管理，PR #451）已落地，本文把那篇 §2 列为「非目标」的 SSO/OIDC 与 passkey 纳入用户体系，决策编号 D1–D7。归属/审批/配额（[user-nodes.md](user-nodes.md)）、member 主体范式（[panel-member-login.md](panel-member-login.md)）、管理员 passkey 的既有设计（[passkeys.md](passkeys.md)）全部不变。
>
> 一句话方案：**passkey 给用户新开一张与管理员同构但独立的凭据表，ceremony 骨架（RP 推导、挑战单次消费、克隆计数拒绝、IP 失败计数）整套照管理员实现搬；SSO 给用户新开一张与管理员 `SSOBinding` 同构的绑定表，回调共用既有 `/auth/sso/callback`、靠 `oauthState` 的 Kind/Target/UserID 分发，绑定完成复用批 2 的票据兑换；两种新方式都进 `loginMethodsView` 与「至少保留一种登录方式」红线计数。**

## 1. 方式矩阵（批 3 后）

| 登录方式 | 凭据载体 | 现状 | 本设计 |
|---|---|---|---|
| 邮箱验证码 / 用户名+密码 / Telegram / GitHub | User 行字段 | 已有（批 1/2 落地） | 不动，纳入新计数 |
| passkey | 管理员：`store.Passkey`（Admin 专属） | 用户无 | 批 3a 新增：新表 `user_passkeys`，两面注册/登录 |
| SSO/OIDC + OAuth2 | 管理员：`SSOProvider` + `SSOBinding` | 用户无 | 批 3b 新增：新表 `user_sso_bindings`，provider 与管理员共用同一批配置 |

## 2. 目标与非目标

### 目标

- 用户能在绑定管理里给自己注册多枚 passkey 并用它在两面登录（discoverable，不输用户名）；能在两面用管理员已配置的 SSO provider 登录，前提是先绑定或满足自动注册口径。
- 新方式与既有四种同等对待：同一绑定管理视图、同一解绑红线、同一会话失效纪律、同一审计与 IP 限流口径。
- 管理员侧 passkey/SSO 的行为、数据、端点**零变化**（只允许在共用件上做加法分发，见 D5/D7）。

### 非目标

- 不给用户引入管理员的账号恢复码体系（`AccountRecoveryCode`）：用户侧已有邮箱验证码这条找回根 + 管理员代设密码处置口，不再叠第二套恢复秘密。
- 不给 User 加 TOTP（沿 user-identity.md 既有非目标）；管理员 SSO 登录的 TOTP 票据步在用户流里没有对应物。
- 不做 id_token 签名校验：管理员流的身份来自 userinfo 端点（见 §3），用户流与之同构，这条姿态是继承的、写明不假装更强。
- 不做 per-provider 的用户可见性粒度（D6 写明取舍）；不做 passkey 跨 RP 转移/同步（换域名重新注册，与管理员口径一致）。

## 3. 现状审计（逐条证据）

- **管理员 passkey 凭据表是 Admin 专属结构**：`store.Passkey`（store/passkey.go:7）字段为 `AdminID`（index）、`CredentialHash`（sha256(credential ID)，**uniqueIndex**）、`RPID`、`Name`、`Data`（Box 密封的完整 `webauthn.Credential` JSON）、`CreatedAt`/`LastUsedAt`。没有主体多态字段，外键就是 Admin。`passkeyUser`（passkeys.go:27）的 WebAuthn handle = `Admin.ID` 字节、name = Username。
- **管理员 ceremony**：注册/登录端点在 router.go:188-189（公开 login begin/finish，挂 `loginGuard`）与 :243-246（管理面 list/register/revoke，挂 `requireSudo`）。注册的管理入口是 `passkeyAccount`（passkeys.go:140，要求普通管理员会话、拒 API token）+ 事务内 `lockPasskeySession`（:157，要求会话 `SudoUntil` 在 10 分钟内——即近期 sudo 复验）。挑战存 eph、单次消费、TTL 2 分钟（`passkeyTTL`），注册挑战绑 AdminID+SID+TokenVersion+Origin（`passkeyChallenge`，:38），登录挑战只绑 Origin。发现式登录 `BeginDiscoverableLogin`（:374），handler 按 `credential_hash + rp_id` 查行、校验 user handle 与行属主一致、账号未锁定（:400-425）。**克隆检测已有口径**：`cred.Authenticator.CloneWarning` 为真直接拒绝并提示改用其他方式（:427-429）；成功后把更新过的凭据（新 sign_count）重新密封回 `Data` 并写 `last_used_at`。失败走 `recordIPFailure`（:449），另有 `shared.allow` 分级限速（register 10 / login begin 10 / finish 20，均按 IP）。每账号上限 20 枚（:240）。登录成功后把所用 key 的 ID 盖进 `AdminSession.passkey_id`，撤销 key 时联动吊销用它签发的会话（:479-497）。
- **RP 配置**：`webAuthn()`（passkeys.go:65）**只信 `cfg.PublicURL`**——RPID = 其 hostname、RPOrigins = 其 origin，明确不信 Host/X-Forwarded-Host；要求 HTTPS（localhost 例外）、拒 IP 主机名。凭据按 `rp_id` 过滤（`passkeyUserOn`，:105），换域名即需重新注册（passkeys.md 明示）。**推论（本设计的关键约束）**：用户中心若部署在与 PublicURL 不同的域名上，那个域名不是现成 RP，浏览器侧 ceremony 根本不会成立——D3 必须给用户中心面一个被配置出来的 RP 来源，不能靠请求 Host 现场推导。
- **管理员 SSO provider 是多行配置表**：`store.SSOProvider`（models.go:563）含 Kind（`oidc` 走 discovery / `oauth2` 手填三端点）、Issuer、Auth/Token/UserInfo URL、ClientID、密封的 ClientSecret、Scopes、SubjectField（默认 `sub`/`id`）、LoginField（默认 `preferred_username`/`login`）、Enabled。管理员 CRUD 在 sso.go:144 起、router.go:267-270（requireSudo）；登录页按钮来自 `publicSSOProviders()`（只列 Enabled，:219）。
- **管理员 SSO 身份映射是绑定表、不是邮箱**：`store.SSOBinding`（models.go:581）以 `(ProviderID, Subject)` 为复合主键、挂 `AdminID` + 展示用 `Login`。登录回调按 `(provider_id, subject)` 查绑定再查 Admin（sso.go:340-346）；**未绑定直接拒绝并引导先用密码登录后绑定，没有 JIT 建号**。绑定先行删除同 (provider, admin) 旧行再插入，被他人占用则拒（:330-337）。删 provider 时联动删其全部绑定（:190）。
- **管理员 SSO 不取 email、更不验 email_verified**：`ssoIdentity`（sso.go:259）换 token（PKCE S256）后只从 userinfo 取 SubjectField 与 LoginField 两值返回；email 仅在 LoginField 取不到时作展示名兜底（:296）。**用户流若要按邮箱做任何判断，必须自己扩一版身份读取**（D4）。
- **state 与 GitHub 共用同一结构与存储**：SSO 的 `ssoAuthURL`（sso.go:238）往 `s.ghAuth` 里 `put(oauthState{Mode, AdminID, Provider, Verifier})`；批 2 已给 `oauthState` 加 `Kind/Target/UserID`（githuboauth.go:31-43，JSON 键 k/t/u，管理员 state 三字段恒空）。SSO 有独立回调 `GET /auth/sso/callback`（router.go:206，挂 loginGuard），与 GitHub 回调分开——用户 SSO 流扩展的是这个回调的分发，不是 GitHub 那个。
- **批 2 绑定管理现状**：`loginMethodsView`（userbindings.go:43）出四方式状态（email/password/telegram/github + githubAvailable + count）；计数 `loginMethodCount`（:29）每种 0/1；红线 `errLastLoginMethod`（:23，HTTP 409 + code `last_login_method`）落在 GitHub 解绑与密码清除两处；二验是 `checkBindingSecondFactor`（:57，有密码必须验当前密码、无密码以活会话为准），二验失败记 `recordIPFailure`；移除成功走失效双件（事务内 `bumpUserEpochTx` + 提交后 `revokeMemberSessions`，:70/:89）。绑定管理端点两面同构（面板 memberRoutes 定向白名单 5 条，memberlogin.go:79-83；用户中心在 portalGate 下）。
- **批 2 票据兑换模式**：GitHub bind 回调落在 `/api` 时两面都没有可用会话（面板 token 在 localStorage、门户 cookie Path 限定 /u），故回调只做占用预检并签发单次票据（`githubBindTicket`，usergithub.go:36；eph 单次消费），回原面后由已登录的 complete 端点兑换、在那里比对 state 的 UserID 与会话用户，不一致整单作废。SSO bind 的回调（`/api` 下的 `/auth/sso/callback`）有完全相同的问题与现成解法。
- **两面会话签发现状**：用户中心 `issuePortalTokenMode`（portal.go:132）已有 telegram/email/password/github 四模式，各模式在 claims 里盖身份指纹（MailFP/PwFP/GhFP，`portalUser` 每请求按模式校验指纹仍成立），actor 前缀由 `portalActorForClaims`（portal.go:211）按模式给（mail:/pw:/gh:，telegram 无前缀）；会话模式判定 `portalSessionMode`（:163）。面板 member 走 `issueMemberToken`（memberlogin.go:140），落 AdminSession 行（via=mail-member 等），与管理员会话同表、靠主体字段区分。passkey/SSO 两面签发都只是在这两处加模式，不新造会话。

## 4. D1 passkey 凭据存储：新表 `user_passkeys`，不混用管理员表

| 方案 | 结论 |
|---|---|
| 复用 `store.Passkey` 加主体多态列（如 owner_kind + user_id） | **否决**：该表被管理员 ceremony、恢复码、备份恢复清理逻辑（passkeys.md：恢复备份会清挑战与恢复码、撤销 passkey 会话）多处按 Admin 语义引用；加多态等于把用户数据引进管理员的撤销/恢复 blast radius，一处漏过滤就是跨体系串号 |
| 新表 `user_passkeys` 与管理员表同构 | **采用**：字段逐项对齐 `store.Passkey`——`ID`、`UserID`（index）、`CredentialHash`（uniqueIndex）、`RPID`、`Name`、`Data`（Box 密封的完整凭据 JSON）、`CreatedAt`、`LastUsedAt`；WebAuthn handle = `User.ID` 字节（与管理员 handle 同为行 ID 字节，但两张表各自查各自的，handle 只在查到的行内比对，不跨表） |

迁移口径：只增一新表（AutoMigrate + schema stamp 的只增不删纪律内），无回填、无旧数据触碰。两表的 `CredentialHash` 各自唯一即可，不需要跨表唯一——同一物理认证器在管理员与用户两侧各注册一次是两个凭据行、两个 credential ID，本就不同。

## 5. D2 passkey 注册：绑定管理内、活会话 + 有密码则二验

- 入口只在绑定管理里（两面各一份，与批 2 的 AccountBindings/账号绑定卡同位）：begin/finish 一对端点，用户中心走 portalGate、面板走 memberRoutes 定向白名单（新增两条，仍属 user-identity D7 的点名放开范围）。**不走 requireSudo**（member 永不过 sudo 的红线不变），改为用户侧等价物：活会话 + `checkBindingSecondFactor` 口径——账号设有登录密码时 begin 前必须验当前密码（失败记 `recordIPFailure`，与解绑二验同），无密码时以活会话为准（会话本身必由某种已验证方式签发）。
- 挑战照管理员形态另起一份用户版结构：eph 单次消费、TTL 2 分钟，绑 UserID + 当前会话标识（面板 AdminSession SID / 门户会话标识）+ 该面 RP 的 Origin + 签发时的 PortalEpoch（等价于管理员的 TokenVersion 校验：epoch 变了，在途注册整单作废）。finish 在事务内复检会话与 epoch 后才落行（照 `lockPasskeySession` 的防在途复活思路，用户版不查 SudoUntil、查会话存活与 epoch）。
- 多枚、可命名、可删：上限 20 枚/账号（与管理员同值）；名称规则同管理员（必填、≤64 字节）；列表返回每枚的 name/rp_id/created_at/last_used_at（Data 与 CredentialHash 永不出接口）。删除端点两面各一，要求同二验口径；删除的红线与会话联动见 D7。
- 注册成功审计 `user.passkey_registered`（actor 沿所在面：面板 `member:<id>`、门户按会话模式前缀，见 D7）。**不生成恢复码**（§2 非目标）。

## 6. D3 passkey 登录：discoverable 两面、RP 按面配置

- **RP 按面取**：新构造函数 `userWebAuthn(face)`。面板面直接复用管理员 `webAuthn()`（PublicURL 推导的 RP，与管理员同一 RP——用户在面板域名注册的 key 与管理员 key 同 RP 不同表，互不影响）。用户中心面新增设置键 **`portal_public_url`**（用户中心公开 HTTPS 地址，进 api_misc.go 的可保存白名单与默认值表，口径同批 1 的 `register_password`）：填了才推导该面的 RPID/Origin（校验规则照 `webAuthn()`：HTTPS、域名、非 IP）；没填则该面 passkey 整体不可用——begin 端点明确报错、登录页/绑定管理不显示 passkey 区（显隐数据随门户登录页既有旗标通道与面板 `/info` 新字段 `memberPasskeyLogin` 下发）。
- **凭据按 RP 隔离**（照管理员 `passkeyUserOn` 的 rp_id 过滤语义）：一枚 key 只在注册它的那个 RP 下能登录；两面同域部署（portal_public_url 与 PublicURL 同 host）时天然共享同一批 key，两面异域时用户各注册一枚，绑定管理列表按 RP 分组展示并明示「通行密钥按访问域名区分」。这是 WebAuthn 的协议事实，不做任何跨 RP 打通尝试。
- 登录 ceremony：两面各一对公开端点（begin/finish），形态照管理员：begin 挂 `loginGuard` + `shared.allow` 限速，`BeginDiscoverableLogin` 不泄露账号是否存在；finish 的 handler 按 `credential_hash + rp_id` 查 `user_passkeys`、校验 user handle == 行的 UserID、凭据克隆警告拒绝（照管理员：`CloneWarning` 直接拒 + 审计告警，sign_count 更新后重新密封回 Data 并写 last_used_at——**克隆检测口径与管理员完全一致，不降级**）。
- 签发：面板面先查 `memberLoginEnabled()` 准入再 `issueMemberToken`，并把所用 key ID 盖进该 AdminSession 行的 `passkey_id` 列（该列已存在、管理员流在用；用户 key 与管理员 key 的 ID 同为全局唯一 NewID，撤销联动按 ID 精确命中，不串）；用户中心走 `issuePortalTokenMode` 新模式 `"passkey"`，claims 盖新指纹 `PkFP`（= 所用 key 的 CredentialHash 指纹，构造照 pwFP/ghFP），`portalUser` 按模式校验该 key 仍存在且属本人——删 key 即杀以它签发的门户会话（与 GhFP 同理），面板面靠 `passkey_id` 联动吊销（照管理员 revokePasskey）。
- 拒绝口径：用户被禁用（Status）→ 拒绝，文案与密码登录统一不泄露细节；用户不存在的 handle、未知凭据、验签失败同为一种失败响应。失败全部 `recordIPFailure`（与管理员 finish 同），成功清零口径不另设（用户侧无账号级计数列，沿批 1 D1 的既定取舍）。

## 7. D4 SSO 身份映射：新绑定表，未命中不静默合并

- **绑定键用表不用列**（审计后对简报建议的调整，理由写明）：SSO 是多 provider 体系（一批 `SSOProvider` 行，用户可在每个 provider 各绑一个账号），User 行上的两列装不下；而管理员先例就是绑定表。故新表 **`user_sso_bindings`** 与 `SSOBinding` 同构：`(ProviderID, Subject)` 复合主键、`UserID`（index）、`Login`（展示）、`CreatedAt`。provider 行本身即 issuer 的部署内代表，故 (provider, subject) 与 (issuer, subject) 等价，不另存 issuer 列；同一 IdP 被管理员建成两个 provider 行时是两个绑定命名空间（与管理员口径一致）。删 provider 时联动删本表对应行（`deleteSSOProvider` 加一处，同它删 SSOBinding 的位置）。
- **身份读取要扩一版**：新 `ssoUserIdentity` 在管理员 `ssoIdentity` 的 (subject, login) 之外多取 `email` 与 `email_verified`（userinfo 标准声明；`email_verified` 兼容 bool 与字符串 `"true"` 两种编码，缺失即 false；kind=oauth2 的 provider 一般不给此声明，恒按未验证处理）。管理员函数一行不改。
- 登录回调命中顺序**定死**（不静默合并）：按 (provider, subject) 查 `user_sso_bindings` → 命中且用户状态正常 → 按 Target 签发（D5）。未命中 → 看邮箱候选：仅当 `email_verified=true` 且邮箱（LOWER 比对）命中某个既有 User 时，**仍不签发、不合并**，回跳报错引导「该 SSO 邮箱与已有账号相同，请先用原方式登录，在绑定管理中绑定此登录方式」——理由：`User.Email` 存在未验证来源（批 1 密码注册的可选邮箱未经所有权证明），按邮箱静默落号是账号接管面；交互式引导把所有权证明交回既有登录方式。邮箱未命中任何既有用户 → 走自动注册判定：注册开放（`userRegistrationOpen()`，批 2 既有口径：任一注册开关开）且限额未满且 `email_verified=true` → 建号（用户名由 SSO login 净化派生、冲突加后缀，邮箱落为已验证邮箱，SelfService/试用套餐/IP 日限/审计 via=sso 全同 `registerByGitHub` 同构）并回填绑定行后签发；注册未开或邮箱未验证 → 拒绝并引导先注册再绑定。
- 绑定占用冲突：目标 (provider, subject) 已绑他人 → 直接拒绝不合并（与 GitHub、管理员 SSO 一致；合并只存在于邮箱验证码路径）。

## 8. D5 SSO 绑定流：共用回调分发 + 票据兑换

- state 沿批 2 扩展复用：用户流 state 写 `Kind="user"` + `Target`（panel/portal）+ `UserID`（bind 时）+ `Provider` + `Verifier`（PKCE，SSO 既有字段），由两面各一对入口端点签发（面板 login-url 公开挂 loginGuard、bind-url 在 memberRoutes；用户中心 login-url/bind-url 在 portalGate 口径下，形态照 usergithub.go 的四个入口）。login 意图的前置门：面板面查 `memberLoginEnabled()`、两面都查 D6 总开关。
- 回调分发：`ssoCallback` 在 `take` 出 state 后、管理员 switch 之前按 `Kind` 分流——`Kind="user"` 走新 `ssoUserCallback`，管理员路径一行不改（与 `githubCallback` 的 Kind 分发同构，验收要钉住管理员回归）。用户回调里完成换码与 `ssoUserIdentity` 读取后：login 意图按 D4 顺序处置并回跳对应面（面板 fragment / 门户 query，与批 2 的回跳通道口径一致）；bind 意图**不直接落库**——回调请求没有可用会话（与 GitHub bind 完全相同的问题），只做占用预检 + 签发单次票据（10 分钟、eph 单次消费，结构照 `githubBindTicket` 加 ProviderID/Subject/Login），回原面后由已登录的 complete 端点（两面各一，面板进 memberRoutes、门户在 portalGate）兑换：兑换时比对票据 UserID 与当前会话用户，不一致整单作废（票据先消费后校验，照批 2）；落库在 `lockAccountBindings` 下复检占用后写行 + 审计 `user.sso_bound`。
- 解绑：两面各一端点（语义照 GitHub 解绑）：二验口径同 `checkBindingSecondFactor`，红线同 `errLastLoginMethod`（D7 计数），成功走失效双件（epoch+1 + revokeMemberSessions）+ 审计 `user.sso_unbound`。管理员不能替用户绑/解绑 SSO（与批 2 的管理员只读概览一致，概览加 SSO 徽标与 passkey 枚数展示）。

## 9. D6 provider 可见性：一个总开关，默认关

- 新设置键 **`sso_user_login`**（bool，默认 `false`；进 api_misc.go 白名单与默认值表，设置 UI 放在 SSO provider 管理区旁，文案明示「允许注册用户使用已启用的 SSO 提供方登录」）。开关开 + provider `Enabled` 为真 → 该 provider 出现在用户两面的登录选项里；开关关 → 用户侧入口端点一律拒、按钮不显示，管理员登录页按钮不受影响（`publicSSOProviders()` 不读此开关）。
- **不做 per-provider 粒度**（取舍写明）：provider 集合本就由管理员以 Enabled 策展、数量是个位数，再叠一层 per-provider 用户开关是双倍设置矩阵；将来真出现「某 provider 只给管理员」的部署需求，再在 provider 行加 `user_visible` 列（只增不删可平滑加），本期不预埋。
- 显隐通道：面板 `/info` 的既有 `sso` 列表之外加 `ssoUserLogin` 布尔（Login.tsx 的 member 区据此 + memberLoginEnabled 显示 SSO 按钮组，按钮仍逐个走 login-url）；门户登录页渲染数据加同名旗标 + provider 列表（照批 2 的 `.GitHub` 旗标通道）。面板面还受 memberLoginEnabled 约束，门户面只受总开关约束（门户登录本身不挂 member 准入，与 GitHub 的门户口径一致）。

## 10. D7 安全红线与计数（跨 3a/3b，逐条落测试）

1. **计数口径升级**：`loginMethodCount` v2 = 邮箱(0/1) + 密码(0/1) + Telegram(0/1) + GitHub(0/1) + passkey(有任一枚 key 则 1) + SSO 绑定数（每个 provider 绑定独立计 1）。`loginMethodsView` 相应加 `passkey`/`passkeyCount`/`passkeys[]`（含 rp 分组信息）与 `sso[]`（provider 名 + login）。红线 `errLastLoginMethod` 统一罩住新移除动作：删最后一枚 passkey（且它是账号唯一方式时）、解绑最后一个 SSO（同理）——判定都走同一个 count 函数，不许各端点自算。
2. **限流同池**：passkey 登录 finish、SSO 用户 login-url 与回调失败（未绑定/被拒/验签失败）全部 `recordIPFailure` 进同一 IP 封禁池（与密码/GitHub/管理员链路共享 `login_ban_failures` 阈值）；passkey begin 与 SSO 入口另有 `shared.allow`/loginGuard 的既有分级限速，照管理员端点的挂法。
3. **审计 actor 扩展**：`issuePortalTokenMode` 加 `"passkey"`/`"sso"` 两模式与 PkFP（D3）；`portalActorForClaims` 加 `pk:`/`sso:` 前缀（与 mail:/pw:/gh: 并列），`portalSessionMode` 同步识别。动作名：`user.passkey_registered`、`user.passkey_revoked`、`user.sso_bound`、`user.sso_unbound`，登录成功/失败审计照各面既有 `user.*_login*` 命名续排；面板面 actor 仍为 `member:<id>`。
4. **绑定防劫持**：passkey 注册挑战绑会话 + epoch、SSO bind 走票据兑换比对会话用户（D2/D5）；login 意图绝不接受 bind 语义，反之亦然（state 的 Mode/Kind/Target 任一不符整单作废）。
5. **管理员零变化**：管理员 passkey/SSO 的表、端点、回调 switch、恢复码逻辑一行不改；`ssoCallback` 与 state 结构只做加法分发。既有 passkeys/sso/githuboauth 全部测试必须原样全绿，这是批 3 每一批的合并前提。
6. **凭据变更失效成对**：删 key、解绑 SSO 后 epoch+1 与 revokeMemberSessions 成对出现（批 2 红线 6 延续）；此外 passkey 会话另有 PkFP/`passkey_id` 的即时联动（D3），SSO 会话的失效靠 epoch 双件（SSO 无稳定指纹可盖——subject 在解绑后即查无绑定行，门户 sso 模式会话的每请求校验须复检绑定行仍存在，写明这一条实现要求，照 GhFP 查 GitHubID 的同构思路落到「查绑定行」）。

## 11. 分批计划（每批一个 PR，测试全绿才提交）

| 批 | 内容 | 上线形态 |
|---|---|---|
| 3a | D1 新表 + D2 注册（两面绑定管理入口、二验、挑战绑会话/epoch）+ D3 登录（两面 discoverable、`portal_public_url` 设置、PkFP/passkey_id 会话联动、克隆拒绝）+ D7 的 passkey 计数/actor/限流接入 | 面板面随 PublicURL 即可用；用户中心面需管理员填 `portal_public_url` 才亮 |
| 3b | D4 新表 + 身份读取扩展 + D5 回调分发/票据绑定/解绑 + D6 `sso_user_login` 开关与两面按钮 + D7 的 SSO 计数/actor/失效复检 + 管理员概览加 SSO/passkey 展示 | 开关默认关；管理员开了且 provider Enabled 才对用户出现 |

### 验收清单（每条对应一个自动化测试，分布在各批）

批 3a：
1. 用户在绑定管理注册 passkey → 登出 → 两面（该 RP 可达的面）discoverable 登录成功且落到同一账号；未知凭据/他人 key 登录失败且文案不泄露。
2. 删 passkey：只剩这一种登录方式时删最后一枚被拒（409 `last_login_method`）；有其他方式时删除成功，且以该 key 签发的两面会话即时失效（PkFP / passkey_id 联动各验一条）。
3. 克隆计数：构造 CloneWarning 的断言响应被拒并记审计；正常登录后 sign_count 前进、last_used_at 更新。
4. 注册挑战被另一会话冒用 finish、epoch 变更后在途注册、过期挑战，三者皆作废；有密码账号未过二验不能 begin 注册。
5. `portal_public_url` 未配 → 用户中心 passkey 区不显示、begin 端点明确报错；配了异域 → 两面 key 按 RP 分组、互不通用；同域 → 一枚 key 两面通用。
6. 禁用用户 passkey 登录被拒；连续失败触发 IP 封禁且与密码端点同池互认。
7. 管理员 passkey 全流回归（既有 passkeys 测试 + 恢复码流）零变化。

批 3b：
8. 已绑 SSO 的用户两面登录成功；同一 subject 绑他人被拒（绑定与自动注册两条路各一）；bind 票据被另一会话兑换作废。
9. 未命中绑定：email_verified 且邮箱命中既有用户 → 拒登并给引导文案、**不建号不合并**；邮箱未命中 + 注册开 + 已验证 → 自动建号即绑即登；邮箱未验证（含 oauth2 provider）→ 一律拒登不建号；注册关 → 拒。
10. 解绑 SSO：最后一个方式被拒；解绑成功后该 subject 再登录落未命中分支、存量 sso 模式门户会话因绑定行消失即时失效。
11. `sso_user_login` 关 → 用户两面无 SSO 按钮、login-url 端点拒；管理员登录页 SSO 按钮与管理员绑/登/解绑全流回归零变化（既有 sso 测试全绿）。
12. 删 provider 后用户绑定行联动清除，该 provider 的用户登录入口消失；管理员用户详情概览能看到 SSO 绑定与 passkey 枚数（只读）。

## 12. 风险与注意

- **RP 是本设计最大的部署约束**：passkey 可用性取决于「访问域名 == 配置的 RP 域名」，面板与用户中心异域部署时要配两个 RP、注册两枚 key，没有捷径；UI 文案必须把这件事说清，否则用户会以为 key「丢了」。同域部署（门户挂在面板域名 /u 下）零感知。
- **`portal_public_url` 是新配置面**：填错（与实际访问域名不一致）会让该面 ceremony 全部失败且报错在浏览器侧，运营排障成本高；设置 UI 旁要给出当前门户访问域名的提示文案，实现时在 begin 端点错误里带上期望 Origin（照管理员 `webAuthn()` 报错风格）。
- **SSO userinfo 信任面**：用户流与管理员流一样把 userinfo 当身份事实来源（token 端点 + TLS 之外无额外签名校验）；`email_verified` 也只是 IdP 的自述——所以它只被用来「建新号或拒绝」，永不用来把身份落到既有账号上（D4 的不合并原则正是为此）。
- **回调分发的串扰面**：`ssoCallback` 的 Kind 分流必须先于管理员 switch 且用户分支不读 Admin 字段（与 GitHub 回调同一纪律，验收 11 钉住）；`oauthState` 已是三方共用结构（GitHub/SSO/用户流），后续再加字段只许加带 omitempty 的新键。
- **迁移纪律**：两批各加一张新表，均只增不删、无回填；回滚 = 关开关（`sso_user_login` / 面板准入）+ 回滚二进制，残留表不影响旧版本（旧代码不读新表；已注册 passkey 的用户仍可用其他方式登录，不锁门外）。

## 13. 决策

| # | 决策 | 推荐 |
|---|---|---|
| D1 | passkey 存储 | 新表 `user_passkeys` 与管理员表同构、严格分开（否决多态复用：管理员恢复/备份逻辑的 blast radius 不进用户数据）；handle = User.ID 字节，只在本表内比对 |
| D2 | passkey 注册 | 只在绑定管理内发起；活会话 + 有密码则当前密码二验（用户版等价物，不走 member 永不过的 sudo）；挑战绑会话 + PortalEpoch、上限 20 枚、可命名可删；不发恢复码 |
| D3 | passkey 登录 | 两面 discoverable；面板面复用管理员 RP，用户中心面新设置 `portal_public_url` 推导 RP（不信 Host，与管理员同纪律）；凭据按 RP 隔离；克隆警告拒绝与 sign_count 回写照管理员；会话联动用 PkFP（门户）+ AdminSession.passkey_id（面板） |
| D4 | SSO 映射 | 新表 `user_sso_bindings`（(provider, subject) 复合主键——多 provider 决定了用表不用列，偏离简报的两列建议，理由见 §7）；身份读取扩 email/email_verified；未命中三分支定死：邮箱命中既有用户只引导不合并、邮箱未命中且已验证且注册开才建号、未验证一律拒 |
| D5 | SSO 绑定流 | 共用 `/auth/sso/callback` 按 Kind 分发（管理员 switch 一行不改）；bind 复用批 2 票据兑换（回调无会话的同一问题同一解法）；占用直接拒不合并 |
| D6 | provider 可见性 | 总开关 `sso_user_login` 默认关 + provider 既有 Enabled 即粒度；per-provider 用户开关列为将来只增列的演进项，本期不预埋 |
| D7 | 红线与计数 | count v2（passkey 计 1、SSO 每绑定计 1）统一罩删/解绑；失败同入 IP 封禁池；actor 加 pk:/sso: 前缀；管理员侧零变化以既有测试全绿为合并前提 |
