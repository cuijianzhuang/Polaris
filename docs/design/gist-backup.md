# Gist 加密备份

借鉴 flyx 的第 6 项：把整机备份加密后多存一份到 GitHub Gist，换异地容灾 + Gist 自带版本历史。功能小，定位一句话：**Gist 是现有备份推送的第 4 个目标**（Telegram / WebDAV / S3 之后），不新建备份体系、不新造加密。

## 现状（调研结论）

- **备份产物**：格式 2 逻辑备份（`internal/backupfmt/backupfmt.go` 的 `WriteArchive`）：全表 gob 行 + 内部 CA，打成 `.tar.gz`；面板侧生成/定时/保留在 `internal/panel/backup.go`（`createBackup`、`scheduledBackup` 每日/每周 + `backup_hour`、`pruneBackups` 按 `backup_keep` 默认 7 份），存储层 `backupstore.go`（本地目录或 S3）。CLI 同源：`polaris backup / restore / restore-drill`（`cmd/panel/main.go`、`admincli.go:Backup`）。
- **加密已现成**：设了备份口令（设置项 `backup_passphrase`，Box 密封保存）即全档加密——scrypt（N=2¹⁵）派生 + AES-GCM 分块（`backupfmt.go:backupKey/EncryptWriter`，魔数 `PLRSENC1`），且加密档内含面板 `secret.key`，可恢复到任意新服务器。未加密档是明文 gob + 仅被面板密钥密封的 CA 私钥，**绝不能出机**。
- **推送已现成且是专业版能力**：目标清单 `pushTargets()` 在主仓 `internal/panel/backuppush.go`，实际推送在 polaris-pro 的 `backuppush_pro.go:pushBackup`，许可证门 `license.FeatureBackupPush`（`internal/license/license.go:22`），社区版是 stub（`edition_oss.go`）。推送失败不影响本地备份，走既有事件 `backup_failed` 通知（`notification_policy.go`）。
- **GitHub 凭据不能复用**：现有 GitHub 集成只有登录 OAuth（`githuboauth.go`，scope 仅 `read:user`，令牌是登录会话级的、不落库），没有可调 Gist API 的服务凭据，必须新增。
- **Gist 硬限制**（GitHub 官方文档）：写/读需 `gist` scope；REST 每文件 `content` 只给 1 MB（超出置 `truncated`），`raw_url` 上限 10 MB，再大只能 git clone；单 Gist 文件数上限 300；secret Gist 不是私密——知道链接的人无需登录即可看；修订即 git 提交，**没有删除单个修订的 API**，只能 DELETE 整个 Gist；认证限额 5000 次/小时，每日/每周备份只用几次，可忽略。

## 决策

- **D1 加密写死前置，复用现有口令**：只允许上传 `.enc` 档——`backup_passphrase` 为空时 Gist 目标自动不生效，设置页明示「先设备份口令」。不新增第二套备份口令/算法。口令即唯一钥匙（档内含 `secret.key`）：**口令丢失 = Gist 上的备份全部不可恢复**，设置页与手册沿用现有文案并加一句「口令请离线另存」。上传前代码断言文件名以 `.enc` 结尾，明文档误入推送路径直接报错。
- **D2 单个固定 secret Gist + 分片**：首次推送 `POST /gists`（`public=false`）创建，id 存设置 `backup_gist_id`；之后每次 `PATCH` 覆盖。二进制档先 Base64（+33% 体积），按 **700 KiB/片**切成文本文件 `backup.b64.000、001…`（单片 < 1 MB，保证恢复时走 API `content` 直读、不碰 raw/clone 路径），另加明文 `manifest.json`（原文件名、创建时间、明文 SHA-256、原大小、片数、格式版本——不含任何秘密）。多出的旧分片在 PATCH 中置 null 删除。**超限降级**：原档 > 50 MB 直接判推送失败并通知（理由：每修订在 git 历史里整存一份，体积失控会让历史爆炸；大体量请用 S3/WebDAV），不做无限分片。
- **D3 令牌单独 PAT、最小权限**：新增设置 `backup_gist_token`——classic PAT 只勾 `gist`，或细粒度令牌只给 Gists 读写；按既有惯例进 `secretSettings`（Box 密封）+ `sudoSettings`（改目标需 sudo 复验），与 `backup_webdav_*` 同档处理。开关 `backup_gist`（bool）+ token 非空才进 `pushTargets()`。许可证沿用 `FeatureBackupPush` 同门（推送实现仍在 polaris-pro，主仓只加设置/目标/UI）。
- **D4 保留策略只管本地，Gist 只保最新一份**：`backup_keep` 不作用于 Gist——Gist 当前态永远只有最新一份，历史份数 = 修订数，**删不动单个修订**，不承诺「Gist 上保留 N 份」。设置页提供「重置 Gist」（DELETE 旧 Gist + 下次推送重建）用于清空历史。安全推论写明：**轮换备份口令后，旧修订仍能被旧口令解开**；口令泄露/轮换时必须重置 Gist，只改口令没用。
- **D5 触发与失败：并行扇出，不另设周期**：Gist 只是 `pushBackup` 扇出里多一个 case，与本地定时备份共用 `backup_schedule/backup_hour`；手动「立即备份并推送」（`POST /backup?push=1`）自动带上。失败按既有口径：结果进 push 结果表 + 发 `backup_failed` 通知，永不拖垮本地备份。设置页加「测试连接」小按钮（校验 token + 已存 id 的 Gist 可读），仿现有 S3 测试端点。
- **D6 恢复走现成链路，只补「拉回」一段**：设置页备份卡加「从 Gist 恢复」：按 id 拉全部分片 → 校验未 `truncated` → 拼回 Base64 解码 → 对 `manifest.json` 的 SHA-256 → 落临时文件 → 喂现有 `stageRestore`（口令默认取当前 `backup_passphrase`，也可在恢复表单里改填）→ 重启应用，与本地恢复完全同一条路。恢复前建议先跑现成的 `restore-drill` 隔离演练再真恢复；纯手工兜底：从 Gist 网页下载分片拼回 `.enc` 后 `polaris restore FILE -passphrase …`。

## 分批

- **批 1（推送侧）**：主仓加设置项/UI 目标卡/测试端点/`pushTargets` 接入，polaris-pro 加 `gistPut`（Base64 分片 + PATCH 覆盖 + 50 MB 门 + `.enc` 断言）。验收：对真实 GitHub 账号往返一次——手动推送后在 Gist 网页看到 manifest 与分片、修订数 +1；清空口令时目标不生效且有提示；错 token 时本地备份成功、推送失败且收到 `backup_failed`。
- **批 2（恢复侧 + 打磨）**：「从 Gist 恢复」+ 恢复前 drill 入口 + 「重置 Gist」+ 用户手册备份章节补 Gist 段。验收：在一台干净测试实例上完成 **Gist 拉回 → drill 通过 → 真恢复 → 节点/用户数据与 CA 校验一致** 的全链路演练一次；再演练一次口令轮换 + 重置 Gist 后旧修订不可达。

## 需拍板

1. **专业版门**：推荐跟现有推送同门 `FeatureBackupPush`（定位已定：付费许可证解锁全部高级功能、不拆卖）；若想把 Gist 当社区版引流功能，需另开许可证口径。
2. **口令复用**：推荐复用 `backup_passphrase`（一个口令管全部备份）；备选 Gist 专属口令——多一个要离线保管的口令，只在「不想让 Gist 档与本地档同钥匙」时才值得。
3. **令牌形态**：推荐单独 PAT（只 `gist` scope）；不推荐给登录用 OAuth App 扩 `gist` scope——其令牌不落库、且会放大登录应用的权限面。
