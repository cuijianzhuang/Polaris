# 入站模板：预览、应用与同步

## 固定生成计划

管理界面先调用 `POST /templates/:id/preview`，传入节点、端口避让、内核改派、CDN 和节点 REALITY 覆盖选项。返回 `planId`、`expiresAt`、`plans` 和汇总。确认时调用 `POST /templates/:id/apply`，传入相同选项及 `planId`。

计划保存在 `template_runs` 中。包含生成密钥、路径、固定端口和端点的快照使用现有 SecretBox 加密；HTTP 响应只包含脱敏预览。计划有效期为 10 分钟。应用时核对模板配置、选项、引用和节点配置指纹；心跳、在线状态及流量不参与指纹。端口或配置有变化时返回 409，要求重新预览。节点在事务内再次核对指纹；单节点失败不影响其他节点。

SQLite 的逐节点写入事务会在读取配置指纹前预占写锁，避免其他面板实例提交心跳或执行计划状态更新后，读事务升级写事务时报 `SQLITE_BUSY` 或 `SQLITE_BUSY_SNAPSHOT` 而只生成部分入站。PostgreSQL 继续使用节点行锁。写锁内仍会重新校验配置指纹，真实配置变更不会被忽略；所有入站、端点和分组绑定仍在单节点事务中一起提交或回滚。

同一计划通过数据库条件更新只能被领取一次，支持多面板实例。完成后重复提交返回原结果。入站 ID 在预览时固定，重试可以识别已经提交的入站，不重新生成密钥或重复创建入站。证书按预览中的证书策略在实际应用时获取。

单批最多选择 100 个节点、2000 个模板条目。过期预览会清理，应用记录保留 30 天。没有 `planId` 的历史 API 调用及节点注册自动套用仍保留即时生成路径；如果请求开启 `verifyReality`，必须通过预览计划完成节点检测。

## 应用状态

- `GET /templates/:id/runs`：最近 20 次执行记录。
- `GET /templates/:id/runs/:run`：各节点的写入结果、目标 revision 和 Agent 应用状态。
- `POST /templates/:id/runs/:run/retry`：重试失败节点，保留已提交的入站。

保存成功与 Agent 应用成功分别展示。状态依据目标配置 revision 的失败回报、节点 `appliedRev` 和连接状态计算；旧 revision 的错误不会误标本次发布。重试发布失败节点的新 revision，已应用节点不变。写入失败后重试仍受计划时效和配置指纹约束。

## 保存校验与字段同步

保存模板时检查名称和条目数量、稳定条目 key、端口策略和范围、已知变量、协议/传输/安全组合、Vision、证书策略、分组和入口引用。校验不安装内核、不申请证书。内置模板由服务端拒绝修改和删除，复制后编辑。

同步差异使用 JSON 字段路径，例如 `protocol.visionUDP443`、`security.realityDest`。新手动编辑只保护变化字段，旧的 `protocol`、`security` 整组标记继续生效。差异包含脱敏后的修改前后值。

`POST /templates/:id/sync` 的确认请求可以传入：

```json
{
  "confirm": true,
  "overwrite": false,
  "templateHash": "preview-template-hash",
  "expected": {"inbound-id": "preview-inbound-hash"},
  "selections": {"inbound-id": ["protocol.visionUDP443"]}
}
```

`selections` 未列出的入站和字段不修改。确认前校验差异指纹；保存时通过更新时间进行乐观并发检查。部分失败会展示到具体入站，界面不会将其提示为全部同步成功。

## REALITY 节点检查

选节点时可通过 `realityOverrides[nodeId][itemKey]` 指定 `dest` 和 `serverNames`。生成入站将这两个字段标记为手动覆盖，后续模板同步默认保留。

`POST /templates/:id/runs/:run/reality-check` 传入 `nodeId`。面板向该节点发送 `reality_scan_v2`，分别验证冻结计划中的拨号目标和 SNI；结果与计划绑定并在计划到期时失效。离线、旧 Agent 或无效结果会明确失败，不从面板回退检测。开启 `verifyReality` 时，所有待生成 REALITY 入站所在节点必须检查通过。

检查只验证节点视角的基础 TLS 条件；HTTP 首页行为、完整 REALITY 代理连接和长期稳定性仍需要另行验证。
