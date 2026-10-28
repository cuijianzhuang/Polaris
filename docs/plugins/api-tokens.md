# API 令牌与订单接口

本文描述已实现的 API 基础，不代表 Webhook、插件安装或动态页面已经可用。插件整体规划见[设计文档](../design/plugins.md)。

## 令牌与权限

在「系统 → 设置 → 账号安全 → API 令牌」创建令牌；一级「扩展」菜单合并后也提供相同入口。创建和吊销需要管理员验证。令牌只显示一次，数据库保存 SHA-256；请求携带 `Authorization: Bearer plt_…`。

| Scope | 可访问范围 |
| --- | --- |
| `users:renew` | 仅 `POST /users/:id/renew`，返回权益摘要 |
| `users:traffic:add` | 仅 `POST /users/:id/add-traffic`，返回权益摘要 |
| `users:plan` | `GET /plans` 和 `POST /users/:id/plan`（开通 / 续费套餐），返回权益摘要 |
| `subs:read` | 指定用户的节点链接及订阅诊断；包含代理链接 / 订阅地址，需当作凭据访问权限 |
| `users:read` | 现有用户列表、详情、导出、分组、设备、节点配额，以及订阅读取；**可读取用户代理凭据** |
| `users:write` | 现有用户创建、修改、删除及流量操作；兼容续期 / 加流量，保留原有响应字段 |
| `nodes:read` | 节点、节点详情和入站读取 |
| `traffic:read` | 汇总、用户 / 节点及标签流量统计；`GET /users/:id/traffic-sources` 查询按小时和节点的来源（不含用户凭据） |
| `settings:read` | 现有设置读取 |
| `nodes:metrics:write` | 仅 `POST /nodes/:id/metrics`，上报节点外部指标 |
| `cdn:ips:write` | 仅 `POST /cdn-profiles/:id/preferred-ips`，测速点上报 CDN 优选 IP；不能读取或修改配置档 |
| `redeem:write` | `GET` / `POST /redeem-codes`（查询、生成兑换码），`DELETE /redeem-codes` 只能按兑换码作废未使用的；并可读取 `GET /plans`、`GET /traffic-packs`。供 Kaku 等发卡系统使用，不能读取或修改用户 |
| `metrics:read` | 仅 `GET /metrics`，Prometheus 格式的监控指标；不含任何用户名、订阅令牌等个人数据 |

以上范围当前为面板全局范围，不提供按客户或节点隔离的租户权限。只需处理订单的服务，优先申请 `users:renew` 和 / 或 `users:traffic:add`，不授予 `users:write` 或 `users:read`。业务服务应自行保存用户 ID 对应关系。

所有有效令牌都可访问 `GET /tokens/self` 和 `GET /openapi.json`，后者只展示当前令牌能调用的路由；不能通过它读取管理员专属接口。令牌管理、管理员设置等未列入白名单的路由默认拒绝。

## 续期与加流量

`PANEL` 包含完整后台路径，例如 `https://panel.example.com/my-admin`。以下请求使用自己的令牌和用户 ID，不要将真实凭据写入公开日志。

```bash
# 续期 30 天，并清零当前用户流量与节点周期用量
curl -X POST "$PANEL/api/v1/users/$USER_ID/renew" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-1001-renew" \
  -d '{"days":30,"resetTraffic":true}'

# 增加 100 GiB（接口字段名保留 gb）
curl -X POST "$PANEL/api/v1/users/$USER_ID/add-traffic" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-1001-traffic" \
  -d '{"gb":100}'
```

- `days` 为 1–3650 的整数。过期账号从当前时间续期；未开始计时的“首次使用后到期”账号增加有效天数；无限期账号保持无限期。
- `resetTraffic` 默认 false。设为 true 会清零用户流量和节点周期上限的已用量，不补回已消耗的节点一次性流量包。
- `gb` 大于 0、最多 1048576，按 `gb × 2^30` 换算为字节，小于 1 字节或配额溢出时拒绝。不限流量账号保持不限。
- 禁用账号不会因为续期 / 加流量而自动启用；内部链路服务账号不能通过这些接口修改。
- 只有细分写权限的令牌得到 `id`、`username`、`status`、`trafficLimit`、`usedUp`、`usedDown`、`expireAt` 和 `expireAfterDays`，不会取得 UUID、密码或订阅令牌。已有宽权限调用方保留 User 响应兼容性。

## 套餐

```bash
# 列出套餐（id、name、price、trafficLimit、days、level 等）
curl "$PANEL/api/v1/plans" -H "Authorization: Bearer $TOKEN"

# 开通或续费：已是这个套餐则顺延，否则从今天开始并清零已用流量
curl -X POST "$PANEL/api/v1/users/$USER_ID/plan" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-1002" \
  -d '{"planId":"PLAN_ID"}'
```

- `mode` 默认 `renew`：用户已是该套餐时到期日顺延、账号正常则保留已用流量；否则视同 `switch`。`switch` 总是从今天重新计时并清零已用流量。
- `planId` 为空字符串表示取消套餐，保留当前限额。
- 成功后发出 `user.plan_changed` 事件；到期自动降级也发这个事件（`reason: "expired"`）。

## 订单幂等与并发

对上述两个接口，权益变更、业务审计和可重放结果在**同一数据库事务**提交。数据库约束负责跨实例同键去重；用户行锁负责不同订单的并发增量，不依赖单进程互斥锁。

- 键最长 100 字节，按不可变调用者 ID、方法和目标资源隔离。同名令牌互不共享结果；不同用户或不同操作可以使用相同键，但建议订单号附带明确的操作后缀。
- 同键同内容在 24 小时保留期内返回第一次提交的状态和正文，响应头 `Idempotent-Replayed: true`。
- JSON 对象键顺序及空白会归一化。数字的不同文本表达仍可能视为不同内容；调用方重试时应发送相同业务字段与数值表达。
- 同键不同请求内容或查询参数返回 409，不修改用户权益。
- 事务或结果存储失败会回滚权益、审计与键预留；同键重试可以重新执行。
- 不传幂等键时，每个成功请求分别执行，但仍通过事务和行锁避免并发丢失增量。
- 权限认证在缓存读取之前，已吊销 / 过期令牌不能读取旧结果。令牌权限缩小时，历史订单响应也按当前细分权限去除凭据，不重新执行订单。

**24 小时后可能再次执行。** 支付服务必须保存自己的长期订单账本，完成支付平台验签、金额 / 币种 / 商户检查及订单状态转换。幂等键不是支付凭证，网络超时也不代表订单未执行。

其他写接口仍提供响应缓存，不承诺与业务变更在同一事务中的崩溃原子性。不要把普通整体用户修改接口当作订单增量接口。

### 旧版记录与升级

旧版缓存使用展示名称识别调用者，无法安全地归属给同名令牌。升级后如果命中保留期内的对应旧缓存，返回 409，要求先核对订单；不会把旧响应误交给另一个调用者，也不会自动重新执行。

确认已有订单实际结果后，由订单系统决定后续处理。不要仅因收到 409 就换一个新键再次发放权益。旧缓存继续按原有 24 小时保留策略清理。

## 数据库与测试

默认使用 SQLite，事务行为也支持 PostgreSQL。专项测试会通过两个独立主控 / Store 实例验证并发重复请求、不同订单累加、结果落库失败回滚、请求冲突、重放权限、周期用量与历史缓存。

```bash
go test -race ./internal/panel \
  -run 'TestUserCommandTransactions|TestNarrowTokenScopesAndOpenAPI' -count=1
```

设置 `POLARIS_TEST_POSTGRES_DSN` 后，同一专项测试也会运行 PostgreSQL 用例。应连接专用测试数据库，测试用户需要创建 schema 的权限；每个场景创建并清理独立随机 schema。未配置时，PostgreSQL 用例会明确跳过。
