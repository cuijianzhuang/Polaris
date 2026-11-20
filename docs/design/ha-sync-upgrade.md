# HA 面板同步升级

> 状态：设计稿，待定。目标是把现在「每台机器各自升级」变成面板里一键把集群全部实例按顺序升到同一版本：先备后主、备机当金丝雀、升级窗口内看门狗不误切 DNS、每台的结果可跟踪、失败能停在可恢复的状态。
>
> 一句话方案：**在现有 cluster_operations 之上加一个 leader 编排的状态机（新 op kind `syncupdate`），分阶段即时创建现有的 `update` delivery；本机升级结果回写 delivery 作为完成回执；主机阶段开始前由备机面板落一个 30 分钟硬过期的看门狗抑制文件。单机升级路径与脚本 `do_update` 不动。**

## 1. 目标与非目标

### 目标

- 在 /ha 页一键发起「同步升级」：集群全部在线实例按 **备 → 主** 顺序逐台升级到指定版本（默认最新版），而不是现在的全部同时开升。
- 备机先升并验证健康，失败即中止，主机一动不动、用户无感。
- 主机升级重启期间，备机上的 DNS 看门狗被显式抑制，不会把计划内停机误判成故障切走 DNS；抑制有硬过期，真故障不会被永久屏蔽。
- 每台实例的升级进度与成败在 /ha 页可见（不再只有「已受理」），全部完成后有版本一致性校验与明确结论（成功 / 部分成功 / 失败在哪台）。
- 编排状态全落库：编排者进程重启（主机阶段必然发生）后由新 leader 无缝接力，不靠内存状态。
- 设置页「升级面板」与自动升级（auto_update=panel/all）在集群模式下改走同一条编排路径，不留能绕过抑制的并行老路。

### 非目标（v1 刻意不做）

- 升级前先切 DNS 到备机实现零断连。切 DNS 与回切都是额外状态，且与已定的「DNS 回切永远人工」冲突；接受主机重启窗口的短暂断连（典型 1–6 分钟，见 §7）。
- CLI 触发（`polaris ha upgrade`）。root 侧 `polaris update` 保持单机语义不变；编排只从面板发起。
- 3 台以上实例的编排。状态机按「非 leader 成员先、leader 最后」写成通用顺序，但只验收 1 主 1 备。
- 升级失败后自动回滚已升好的备机。版本短暂不一致可接受（见 §5），回滚与否人工决定。
- 破坏性数据库迁移的自动处理。靠发版纪律拦截（见 §5 末），v1 不做程序检查。

### 与现有「集群更新」的关系

现在已经有一套集群更新（设置页升级时 `requestPanelUpdate` 给全部实例同时下发），本设计不是另起炉灶，而是给它补上四件缺失的事：**顺序、看门狗抑制、完成回执、失败中止**。名字沿用用户说法叫「同步升级」——同步指的是最终版本一致，不是同时开始。

## 2. 现状与复用点

| 现有机制 | 位置 | 现状与缺口 |
|---|---|---|
| 面板触发升级 | `internal/panel/panelupdate.go:83` `requestPanelUpdate` | 集群模式（`bus != nil`）时 `queueClusterOperation("update", target, includeSelf=true)` 后立即本机处理；入口有三：`POST /system/update`（`internal/panel/router.go:474`，requireSudo）← 设置页 `web/src/pages/settings/Updates.tsx`、Telegram /update、`autoUpdateTick`（`panelupdate.go:286`，leader 定时任务，`internal/panel/server.go:510`）。缺口：全部实例近乎同时开升，无顺序、无中止 |
| 集群操作下发 | `internal/panel/cluster_operations.go:33` `queueClusterOperation`、`:136` `saveClusterOperation` | op 24 小时过期；建 op 时一次性给当时全部在线成员建 delivery。缺口：delivery 必须改成**分阶段即时创建**，否则阶段控制无从谈起（且旧代码实例看到自己的 delivery 就会立刻执行） |
| 各实例执行 | `internal/panel/cluster_operations.go:62` `runClusterOperations`（`server.go:321` 启动，5 秒轮询）、`:76` `processClusterOperations` | kind=update 时：已是目标版本跳过、本机已有 update-request 文件跳过、否则 `writeUpdateRequest` 并把 delivery 置 `accepted`；失败 15 秒重试并记 `last_error`。这套执行端逻辑编排方案原样复用 |
| 写升级请求 | `internal/panel/clustermembers.go:149` `writeUpdateRequest` | 写 `<data>/update-request`（内容 `latest` 或 tag）；已有文件返回 `errUpdatePending`；dev 版/未装 updater 拒绝。面板不执行系统命令的硬边界靠这层维持，本设计不打破 |
| root 侧执行升级 | `scripts/install-panel.sh:854` `do_update_request`、`:1162` `do_update` | `polaris-update.path`（PathExists 触发，`:826` `install_updater`）→ 读请求文件（先删防重入）→ `do_update`：解析版本 → 已是目标版直接返回 → 下载（GitHub + 镜像回退）→ 新二进制试运行 → 停面板 → 备份（PG 走 `polaris_cmd backup`，120 秒超时在 `:1209`，失败 warn 后无备份继续）→ 旧二进制存 `.prev` → 替换 → 启动 → `wait_healthy`（`:604`，600 秒上限、连续 2 次 HTTP 命中）→ 失败自动回滚 `.prev` 并再验活。结果写 `<data>/update-result`（ok/from/to/log 尾 12 行）。**这层完全不动** |
| 结果回收 | `internal/panel/panelupdate.go:202` `checkUpdateResult` | 每台本机定时跑（`server.go:511` addLocal，默认 20 秒，`internal/panel/config.go:234`）+ 启动时：读 update-result → Telegram 通知 + 审计 + `auto_update_failed` 设置。缺口：结果只在本机消费，**不回写任何集群表**，编排者无从知道某台升完没有 |
| 操作/交付模型 | `internal/store/recovery.go:51` `ClusterOperation`、`:58` `ClusterDelivery` | Operation 只有 id/kind/target/created_at/expires_at，**无状态字段**；Delivery 的 status 只有 `pending`/`accepted`，存了 `BeforeVersion` 却不回填结果。`recovery.go:49` 注释明说：accepted 只表示请求已交本机，结果以本机 updater 报告为准——回执设计必须尊重这条（见 §4.4） |
| 成员与版本 | `internal/panel/clustermembers.go:93` `clusterMembers`、`:28` `subscribeClusterOps` | 心跳 15 秒、在线阈值 45 秒、遗忘 30 分钟；成员行带 version/started_at，是完成推断与最终校验的数据源。无 member_id 的旧实例走 bus `topicUpdate` 广播（收到即写请求文件，无 delivery 跟踪）；bus 是 PG LISTEN/NOTIFY（`internal/panel/bus.go:16`），best effort、靠 DB 轮询兜底 |
| 状态展示 | `GET /cluster`（`clustermembers.go:166`）、`GET /ha/status`（`internal/panel/api_ha.go:171`） | 设置页集群卡片 `web/src/pages/settings/Cluster.tsx` 显示成员版本与「已受理 n/m」，并明示「受理表示请求已交给本机处理，实际升级结果以版本和升级通知为准」；版本不一致有警告条。/ha 页（`web/src/pages/HA.tsx`，309 行）有角色/复制/对端，**没有任何升级 UI** |
| 数据库迁移 | `internal/store/store.go:156` `AutoMigrate` | 任一实例启动即迁移**共享库**；GORM AutoMigrate 只增不删，这是现行并行升级能成立的隐含前提，也是备机先升的前提（见 §5 末） |
| leader 租约 | `internal/panel/cluster.go:24`（TTL 15 秒） | 主机面板重启期间备机自动接管 leader（定时任务、Telegram）；编排接力基于此 + DB 状态 |
| DNS 看门狗 | `scripts/cf-failover.sh`（备机 cron 每分钟） | 直连主公网 IP 查 `/api/v1/info`（`:53`）；本机面板不健康不切（`:58`）；连续 3 次失败（`FAIL_N=3`，`:42`）+ 见证同意（配了见证时，`:71` 起）→ PUT DNS 到备 IP，单向不回切。**没有任何维护抑制机制**。脚本在 `install-panel.sh:1920` 起有逐字节一致的内嵌副本（`ha_watchdog_script`），`scripts/test-ha-dryrun.sh` 校验一致性与看门狗场景 |

### 一个必须点破的现状风险

现行并行升级下，主机的停机窗口（停面板 → 备份最长 120 秒 → 启动验活）完全可能超过看门狗的 3 分钟触发线。至今没误切，只是因为备机同时在升级、本机健康检查（`cf-failover.sh:58`）偶然挡住了——备机若先恢复而主机还没起来、且失败计数已累计，看门狗照样会切。这是本设计要显式修掉的，不是新功能引入的新问题。

### 既有决策衔接

《面板高可用一键部署》§9 已定默认：「先升备再升主，沿用现有 update-request 机制；两台版本短暂不一致可以接受，别拖太久。」本设计沿用该顺序与机制，只补编排、抑制与回执。已定的「提主与 DNS 回切永远人工」不因本功能改变：同步升级全程不碰 DNS 指向（抑制只是让看门狗在窗口内不动作）。

## 3. 方案选项

### 方案 A：leader 编排的分阶段滚动升级（推荐）

新增编排 op（kind=`syncupdate`，带 state/phase），由 leader 上的编排循环（与 `runClusterOperations` 并行、同样 5 秒 tick）推进状态机：备机阶段 → （抑制生效）→ 主机阶段 → 校验收尾。每个阶段开始时才给目标成员创建现有的 kind=`update` delivery，执行端（`processClusterOperations` → `writeUpdateRequest` → 脚本 `do_update`）零改动。每台的完成/失败由本机 `checkUpdateResult` 回写 delivery；回执缺失时用成员版本推断兜底。看门狗加一个抑制文件检查。

- 优点：备机是真金丝雀（新版本先在无流量的一台验证启动与迁移）；任一阶段失败可停在明确状态；编排状态全在库里，leader 重启/易主可接力；对旧实例的兼容靠「delivery 即时创建」天然成立（旧代码只会执行已经轮到它的 delivery）。
- 代价：要动数据模型（op 状态、delivery 回执字段）、加编排循环、改看门狗脚本双副本、/ha 页加卡片。是三个方案里改动最大的。

### 方案 B：保留并行，只补抑制 + 回执（最小改动）

现有下发方式不动，只做两件：主机开升前让备机落抑制文件；delivery 加回执字段让结果可见。

- 优点：改动小、一批可交付；看门狗误切风险立刻消除。
- 缺点：双机同时停机、没有金丝雀——新版本若起不来，两台同时回滚甚至同时出事，期间没有一台健康面板兜底；主机停机与备机约 5 分钟慢启动叠加，用户侧断连时长不可控；「备机失败中止主机」这类语义永远做不出来。只能当 A 的第一批，不能当目标形态。

### 方案 C：外部编排（root 脚本 / SSH 驱动）

`polaris ha upgrade` 在主机以 root 运行：SSH 到备机执行 `polaris update`，验活后再本地执行。

- 缺点：双机之间没有现成的 root SSH 信任，新建密钥即新增攻击面与运维负担；编排状态不在库里，/ha 页看不到进度、面板审计里没有记录；与现有「面板写意图、root 只执行本机请求」的分工相悖。不推荐。

## 4. 推荐方案详细设计（方案 A）

### 4.1 触发方式与权限

- 新增 `POST /ha/sync-update`（requireSudo，与 `POST /system/update` 同级），body：`{target?: string, agents?: bool}`，target 缺省为最新版（服务端用 `latestPanelVersion` 解析成具体 tag 再落 op，避免各台把 `latest` 解析到不同版本）；agents 沿用现有语义（全部面板升完后接节点 Agent 滚动升级，仍由原 leader 侧逻辑在收尾时处理，见 §4.6）。
- /ha 页新增「同步升级」卡片：显示当前版本、最新版本、「同步升级」按钮（二次确认，文案写明顺序与预计时长）、进行中时显示每台的阶段与状态。
- 谁能触发：**任一实例上的 sudo 管理员**。触发只是在共享库里落一条 op，与触发者是不是 leader 无关；编排由当时的 leader 执行。管理员平时从 DNS 指向的实例登录，故障切换后入口在备机，要求「只能在 leader 上点」反而别扭。
- 集群模式下，设置页「升级面板」（`POST /system/update`）与 `autoUpdateTick` 改为创建 syncupdate op（target 同上），与 /ha 页共用一条路径；单机模式（`bus == nil`）保持现有直写请求文件不变。已有一个未终态的 syncupdate op 时，新触发直接报错（指明已有进行中的升级），不排队。
- CLI 不做（见非目标）。

### 4.2 编排状态机

op 新增字段：`state`（running / succeeded / partial / failed / aborted）、`phase`（standby / leader / verify / done）、`detail`（人读结论，如失败在哪台、什么原因）、`updated_at`。阶段推进全部用条件更新（`UPDATE ... WHERE id=? AND phase=?`）做 CAS，防租约切换间隙双 leader 重复推进。

1. **standby 阶段**：编排器给全部**非 leader 在线成员**即时创建 kind=`update` delivery（复用 `saveClusterOperation` 的成员枚举逻辑，但只含本阶段目标）。等待每台 delivery 到达终态（§4.4）。任一台 failed → op 置 aborted，detail 点名，通知；主机不创建 delivery，升级到此为止。全部 done → 进 leader 阶段。
2. **leader 阶段**：先给**看门狗所在成员**（HA 角色为 secondary 的成员）创建 kind=`suppress` delivery（§4.3），等其 accepted（抑制文件已落盘）后，才给 leader 成员创建 kind=`update` delivery，并**先把 op 推进到 phase=leader 落库、再触发本机执行**——因为 leader 面板马上要重启，顺序不能反。leader 重启后，无论新 leader 是升级后的主机还是备机，编排循环从 DB 状态接力，进入等待。
3. **verify 阶段**：leader delivery 终态后，核对全部在线成员 version == target（§4.6），置终态、写 detail、审计、Telegram 通知，并给看门狗成员创建 kind=`unsuppress` delivery 删除抑制文件（删不掉也无妨，硬过期兜底）。
4. 编排循环挂在 leader 上：非 leader 实例不推进状态机、只执行自己的 delivery 与回执。op 的发起者若不是 leader，无需任何转发——leader 的下一次 tick（≤5 秒）自然接手。

超时：单阶段 30 分钟、op 总时长 60 分钟（默认值，依据见 §7）。阶段超时 → 该阶段未终态的成员按 failed 处理（detail 注明「超时，未见完成回执」），后续按失败路径走（standby 阶段超时即 aborted；leader 阶段超时置 failed 并解除抑制）。超时只报错、不自动重试升级动作本身——脚本侧已有回滚，盲目重试可能把已回滚的机器再推向坏版本。

### 4.3 看门狗抑制

- 载体：备机本机文件 `<data>/failover-suppress`（即 `/var/lib/polaris/failover-suppress`），内容两行：`until=<unix 时间戳>`、`op=<op id>`。由**备机面板进程**写/删（面板写自己数据目录，与写 update-request 同类动作，不破硬边界）；看门狗以 root 运行，读该文件无权限问题。
- 写入路径：kind=`suppress` 的 delivery 到达备机后，其 `processClusterOperations` 写文件并置 accepted——accepted 即「抑制已生效」，编排器据此才放行主机阶段。kind=`unsuppress` 同理删文件。两种 kind 只对本机文件负责，不触发任何升级动作；旧代码实例不认识这两种 kind，会走现有「未知集群操作」报错重试分支——因此 suppress delivery 只能发给已确认运行新代码的成员（成员 version 达到引入本功能的最低版本），不满足时编排器拒绝开始 leader 阶段并提示先单独升级备机（这正是 §10 问题 6 的降级边界）。
- 看门狗改动：`scripts/cf-failover.sh` 在主检查之前加一段：抑制文件存在且 `until` 在未来 → 记一行日志（`suppressed by sync upgrade op … until …`）并 `exit 0`；文件不存在或已过期 → 照常走原有三条件规则。**不**重置失败计数（窗口结束后计数从原值继续，行为可预期）。同步改 `install-panel.sh:1920` 起的内嵌副本（两处必须逐字节一致），`scripts/test-ha-dryrun.sh` 增加场景：抑制有效期内不切、过期后照常切、无文件照常。
- 窗口长度：30 分钟硬过期，从 suppress accepted 时刻起算。主机停机窗口的实测上限约 12 分钟（备份 120 秒 + wait_healthy 600 秒 + 余量），30 分钟留足下载排队与重试的余地；同时它也是安全上限：主机真死在升级里，30 分钟后看门狗自动恢复裁决，不需要人去删文件。
- 见证侧**不改**：切 DNS 的决定只在备机看门狗一处作出，抑制在看门狗入口短路，先于见证询问；见证的 verdict 与 promoted/围栏逻辑与计划内升级无关。
- 备机自身升级（standby 阶段）期间不落抑制：那时主机健康，看门狗照常盯主机是对的；备机面板重启期间看门狗的本机健康检查本来就挡住切换，窗口自洽。

### 4.4 完成回执与推断兜底

`ClusterDelivery` 增加：status 扩为 `pending / accepted / done / failed`，新增 `after_version`、`finished_at`；失败原因复用 `last_error`。

- **权威回执（本机写）**：`checkUpdateResult` 消费 update-result 后，除现有通知/审计外，按本机 member_id 找最近一条 status=accepted 的 update delivery（target 与 result 的 to/from 对得上），ok=1 回写 done（after_version=to），ok=0 回写 failed（last_error 取 log 摘要）。这尊重 `recovery.go:49` 的既有约定：结果由本机 updater 报告产生，编排者只读不猜——回写者就是本机自己。
- **推断兜底（编排者写）**：旧版本实例没有回写逻辑，或回执在进程被杀等极端情况下丢失时，编排器按成员行推断：成员以 version==target 且 started_at 晚于该阶段开始时间重新在线 → 判 done；成员曾离线又以旧版本重新在线 → 判 failed（脚本已回滚，面板活着但版本没变）；阶段超时仍无结论 → failed（超时）。推断结论在 detail 里注明「按成员版本推断」，与权威回执区分。
- 进度呈现的数据就来自 delivery：pending=等待本机受理、accepted=已交本机 updater（下载/备份/启动都在这一段，是最长的一段）、done/failed=终态。v1 不做百分比进度——脚本侧的阶段（下载中/备份中/启动中）没有现成的回传通道，硬造不可靠；`accepted` 持续时长 + 阶段开始时间足够判断是否卡住。要更细的进度需脚本往 update-result 之外再写进度文件，列为后续可选增强，不进 v1。

### 4.5 失败处理与幂等续跑

| 场景 | 集群状态 | 系统动作 | 人工恢复 |
|---|---|---|---|
| 备机升级失败（脚本回滚成功） | 主机旧版正常服务，备机回滚到旧版，版本仍一致 | standby 阶段判 failed → op aborted，主机不启动升级，通知点名备机与 log 摘要 | 查备机失败原因（多为下载或启动），修复后重新发起同步升级 |
| 备机升级失败且回滚后也起不来 | 主机正常服务；备机离线（无 HA 冗余） | 同上 aborted；成员离线在 /ha 页可见 | 按现有备机故障流程处理（重装/ha-join），与本功能无关 |
| 主机升级失败（脚本回滚成功） | 备机新版、主机旧版，**版本不一致但都在服务**；主机仍是 DNS 指向，业务无感 | leader 阶段判 failed → op failed，解除抑制，通知写明版本分布 | 二选一：修复后重跑同步升级（备机已达 target 会被执行端自动跳过，只升主机）；或把备机单独降回旧版（`polaris update` 指定旧 tag）。默认前者 |
| 主机升级失败且回滚失败（面板起不来） | 主机面板下线；备机新版健康但 DNS 还指主机 | 抑制到期（≤30 分钟）后看门狗按原规则裁决：主连续 3 次不通 + 备机健康（+见证同意）→ 切 DNS 到备机，服务在备机恢复；PG 提主仍按现有人工流程 | 按现有故障切换 runbook 走；这正是抑制必须硬过期的原因 |
| 编排者中途被杀/断电 | op 停在当前 phase，状态在库里 | 新 leader 的编排循环按 DB 状态接力：standby 阶段继续等回执/推断；leader 阶段同理；已过总超时则置 failed | 无需人工；/ha 页能看到接力后的状态推进 |
| 某台在升级中被人工重启/断电 | 该台 update-result 可能缺失 | 回执缺失走推断兜底：以目标版本回来=done、旧版本回来=failed、一直不回来=阶段超时 failed | 按 failed 的恢复路径走 |
| 重复触发/重复 delivery | — | 未终态 op 存在时拒绝新触发；执行端既有幂等（已达版本跳过、请求文件已存在跳过）在位，同一台不会并发两个 `do_update` | — |

幂等的根基是三条现成的：op/delivery 全落库且阶段推进用 CAS；`writeUpdateRequest` 对已存在请求文件返回 `errUpdatePending`；`do_update` 对已达目标版本直接返回成功。编排器自身不缓存任何内存进度，任意进程在任意时刻接手都收敛到同一状态。

### 4.6 收尾校验与通知

verify 阶段做三件事：

1. 读 `clusterMembers()`：全部**在线**成员 version 必须 == target。离线成员不算失败（它没参与本次升级，op 的成员集合在创建时就只含当时在线者），但在 detail 里点名「某成员离线，未包含在本次升级」。
2. op 置终态：全 done 且版本一致 → succeeded；有 done 有 failed → partial（detail 列明每台结论）；关键阶段失败按 §4.5 已提前置 aborted/failed。
3. 审计 `panel.sync_update` + Telegram 通知（复用现有 `panel_update` 事件与 `hostTag` 风格）：一句话结论 + 每台 from → to。`agents=true` 时，收尾后沿用现有 `upgradeAgentsAfterUpdate` 路径滚动升节点 Agent——注意该逻辑现在挂在单机 `checkUpdateResult` 后，集群模式下改为由编排收尾触发一次，避免每台各触发一遍。

设置页集群卡片与 /ha/status 不改数据来源：/cluster 的 deliveries 带上新字段后，前端即可显示 done/failed；Cluster.tsx 现有「已受理 n/m」文案升级为「已完成 n/m」属于展示层小改，跟着 PR4 一起做。

## 5. 版本一致性与迁移纪律

- 备机先升意味着：**新版本的 AutoMigrate 先作用于共享库，此时主机还跑旧代码**。GORM AutoMigrate 只加表/列/索引、不删不改名，旧代码对多出来的列无感——现行并行升级一直依赖这条，只是从没写下来。本设计把它写成发版纪律：任何版本若含破坏性迁移（删列、改列名、改语义），不得走同步升级，必须停机窗口人工处理；同步升级的发版检查清单加这一条。
- 同理，cluster bus 的消息格式、cluster_operations 的表结构在本功能落地后也进入「只增不改」的兼容面：编排 op 的新 kind 与 delivery 新字段对旧代码都是无害增量（旧代码忽略新 kind 的 op 行、忽略新列）。
- 升级窗口内版本不一致是常态（备机新、主机旧，最长约一个阶段的时间），现有 Cluster.tsx 的版本不一致警告条在升级进行中会被触发——可接受，收尾后自动消失；detail 里会说明这是计划内状态。

## 6. 数据模型与接口变化（汇总）

| 变化 | 内容 |
|---|---|
| `store.ClusterOperation` 加字段 | `state`、`phase`、`detail`、`updated_at`（AutoMigrate 增量，旧行零值兼容） |
| `store.ClusterDelivery` 加字段 | status 扩 done/failed；`after_version`、`finished_at` |
| 新 op kind | `syncupdate`（编排记录，不产生 delivery 给执行端）、`suppress` / `unsuppress`（单成员 delivery，只写/删抑制文件） |
| `saveClusterOperation` 改造 | 支持「指定成员子集」建 delivery（阶段即时创建用）；原全量行为保留给现有 update/restore |
| 新 API | `POST /ha/sync-update`（requireSudo）；`GET /cluster`、`/ha/status` 响应随模型字段自然扩展，不新增接口 |
| 面板内部 | 编排循环（leader，5 秒 tick）；`checkUpdateResult` 回写回执；`requestPanelUpdate` 集群分支改道编排；`processClusterOperations` 认识 suppress/unsuppress |
| 脚本 | `cf-failover.sh` + `install-panel.sh` 内嵌副本加抑制检查；`test-ha-dryrun.sh` 加 3 个抑制场景。`do_update` / `do_update_request` 不动 |
| 前端 | HA.tsx 加「同步升级」卡片（发起 + 每实例阶段/状态 + 结论）；Cluster.tsx 计数文案改「已完成」 |

## 7. 时长预算（超时取值的依据）

单台升级的构成（脚本实测与上限）：

| 环节 | 典型 | 上限/依据 |
|---|---|---|
| 下载 release（GitHub 或镜像） | 数十秒 | 无硬上限，镜像回退逐个试；面板此时仍在服务，不计入停机 |
| 备份（PG，停机内） | 数秒 | 120 秒超时（`install-panel.sh:1209`），超时无备份继续 |
| 替换 + 启动验活 | 主机数秒–1 分钟；**备机约 5 分钟**（两次实测 5m07s/5m08s，根因未查明） | `wait_healthy` 600 秒（`install-panel.sh:604`） |
| 结果回执 | ≤20 秒 | `checkUpdateResult` 本机轮询间隔（`config.go:234`） |

由此：单台停机窗口上限约 12 分钟；单阶段（含下载与回执）30 分钟超时、抑制窗口 30 分钟、op 总时长 60 分钟（两阶段串行 + 校验）。用户侧可感知的只有主机阶段的停机窗口（典型 1–6 分钟，DNS 仍指主机）；备机阶段用户无感。同步升级全程典型总时长约 10–15 分钟（备机慢启动占大头）。

## 8. 测试计划

- **Go 单元测试**：状态机推进（standby 全 done 才进 leader 阶段、任一 failed 即 aborted 且不给 leader 建 delivery）；CAS 防重复推进；回执回写（ok/失败两路、target 对不上不回写）；推断兜底三分支（新版回来/旧版回来/超时）；suppress 只发给达标版本成员；未终态 op 拒绝重复触发。沿用现有 `cluster_operations_test.go` 的夹具风格。
- **Shell**：`scripts/test-ha-dryrun.sh` 加抑制场景（有效期内不切且记日志、过期照常走三条件、无文件行为不变）+ 双副本一致性（现有校验自动覆盖）；shellcheck 零警告。
- **手工演练**：真机跑一遍完整同步升级（备机先升、观察 /ha 进度、主机阶段看门狗日志出现 suppressed 行、收尾版本一致）；再演练一次备机失败（指定一个不存在的 target 版本）确认主机纹丝不动。
- **回归红线**：全仓测试通过才能提交 PR（老规矩）；脚本改动跑全部 `scripts/test-*.sh`。

## 9. 分批交付

| 批 | 内容 | 验收 |
|---|---|---|
| PR1 | 回执：Delivery 加字段 + `checkUpdateResult` 回写 + 编排侧推断兜底函数 + /cluster 展示完成态 | 单测覆盖回写与推断；现行并行升级的结果能在设置页看到 done/failed |
| PR2 | 看门狗抑制：脚本双副本 + 测试场景 + suppress/unsuppress delivery 的面板侧处理 | shell 测试全过；手工在备机落抑制文件验证看门狗不切、过期后恢复 |
| PR3 | 编排器：syncupdate 状态机 + 分阶段 delivery + 超时 + 收尾校验/通知 + `POST /ha/sync-update` + 设置页与 autoUpdateTick 改道 | 状态机单测全过；真机全程演练一次 |
| PR4 | HA 页同步升级卡片（发起、进度、结论）+ Cluster.tsx 文案 + user-guide 更新 | 页面状态与库里 op/delivery 一致；文档命令与实际行为一致 |

PR1、PR2 彼此独立且各自有独立价值（回执让现行升级结果可见、抑制消除现行误切风险），可先合；PR3 依赖 PR1/PR2；PR4 依赖 PR3 的 API。

## 10. 待用户拍板的问题

每条给出默认建议，无异议即按默认执行。

1. **触发入口统一**：集群模式下设置页「升级面板」与自动升级都改走编排路径，不保留并行老路作为逃生口？——默认：**统一走编排**。并行老路绕过抑制，正是误切风险源；真要并行，人工逐台 `polaris update` 还在。
2. **自动升级（auto_update=panel/all）走不走编排**：——默认：**走**。否则凌晨自动升级仍可能触发看门狗误切，且无人值守时双机同升风险更高。
3. **抑制窗口 30 分钟**：主机停机窗口实测上限约 12 分钟，30 分钟是「够用」与「真故障不被拖太久」的折中。——默认：**30 分钟硬过期，不可配**（可配就有人配成无限，安全上限失效）。
4. **超时取值**：单阶段 30 分钟、总 60 分钟，超时只判失败不自动重试。——默认：**按此执行**。
5. **主机失败回滚后的版本不一致**（备新主旧）：只告警、人工决定重跑升级或降备机，不自动回滚备机。——默认：**接受**（与 §9 既有口径一致）。
6. **首次使用的版本门槛**：编排功能随版本 X 发布后，X 本身仍用老方式逐台升（最后一次）；从 X+1 起用同步升级。suppress 要求备机已在 X，编排器会校验并拒绝不达标成员。——默认：**接受这个台阶**，不为旧实例做 suppress 降级（给旧实例硬落抑制文件需要它不认识的通道，得不偿失）。
7. **主机阶段接受短暂断连**（典型 1–6 分钟），不做「先切 DNS 到备机」的零断连变体。——默认：**接受**（理由见非目标）。
8. **进度粒度**：v1 只到「等待受理 / 已受理（执行中）/ 完成 / 失败」四态，不做下载/备份/启动的分环节进度。——默认：**四态足够**，分环节进度留作后续增强。
