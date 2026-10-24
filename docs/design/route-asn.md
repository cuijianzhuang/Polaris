# 回程检测的本地 ASN 归属

本轮仅后端/API，不改 UI。ASN 数据来自管理员上传的本地 MMDB；不自动下载数据库、不发起外部 IP 查询、不做反向 DNS、不使用 Shodan 密钥。初始默认关闭且不包含生产数据库。

## 安装和启用

管理员接口：

- `GET /tools/route-asn`：desired policy、当前实例的数据库状态、类型/构建时间/IP 版本，以及 `effectiveEnabled`。
- `PUT /tools/route-asn/database?expectedRevision=0`：请求体为原始 MMDB 二进制，最多 64 MiB。上传仅安装/替换数据；首次上传不自动启用，更新已经启用的数据源则保留开关状态。
- `PUT /tools/route-asn`：`{"enabled":true,"expectedRevision":1}`。开启前要求当前实例已安装该策略引用的有效数据库。

`expectedRevision` 可省略；建议先 GET 再使用其 policy.revision，冲突返回 409。重复上传相同数据或重复保存相同开关不递增版本。上传接口不接受 URL、服务器文件路径或自定义文件名，不通过通用 settings 接口修改策略。

使用兼容 MaxMind ASN 字段 `autonomous_system_number`、`autonomous_system_organization` 的 ASN MMDB。检查大小、文件完整性、数据库类型、地址族和兼容记录；国家/城市库及字段不兼容的其他 ASN 格式不作为本数据源使用。数据库真实性、覆盖范围和更新由管理员选择的数据源决定；格式校验和 SHA-256 校验不等于验证归属事实。

示例（PANEL_API 为实际管理员 API 根地址，ADMIN_TOKEN 为管理员会话凭据）：

```sh
curl -H "Authorization: Bearer $ADMIN_TOKEN" "$PANEL_API/tools/route-asn"
curl -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/octet-stream' --data-binary @GeoLite2-ASN.mmdb \
  "$PANEL_API/tools/route-asn/database?expectedRevision=0"
curl -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"enabled":true,"expectedRevision":1}' "$PANEL_API/tools/route-asn"
```

版本数字需按实际返回值替换。节点操作员、API 令牌和匿名请求不能调用以上接口。

## 数据和历史

新检测返回并保存：

- result.asn：注释状态 disabled / unavailable / ready；数据来源 SHA-256、数据库类型、构建时间、地址族和面板注释时间。
- hops[].asn：对应 IP、matched / private / not_found / invalid / unavailable 状态、ASN 编号、组织名称和匹配网段。组织名限制长度并去掉控制字符。

不命中或不支持的地址族不猜测归属。私网、回环、链路本地、非单播及共享地址空间不进行数据库查询。最多注释每跳前 4 个地址，超出时 result.asn.limited=true；原始地址和证据仍保留。

Agent 自报的结构化 ASN 字段总是清除，面板只保存本地注释。每次手动/定时检测共用同一个数据库快照进行注释；本地查询不可用时仅记录 ASN 不可用及原因，不把真实网络结果改成失败。

旧历史不在读取时补查或重算。替换数据库后，旧记录保留当时的编号、组织、哈希和构建时间；新记录使用新数据。原始路径对比仅看 IP/TTL，不使用 ASN、组织名或数据库版本，因此更换数据不生成路径变化提醒。通知仍只发已有的变化摘要，不携带这些注释或逐跳原始地址。

ASN 数据是地址网段的归属注释，不证明真实 BGP AS path、实际承载运营商、商业线路等级或业务可用性；不能据此直接标为 CN2、9929 等线路。

## 文件、备份和多实例

文件保存在面板 DataDir 的 route-asn 子目录，使用内容哈希命名和 0600 权限。验证后暂存，再在共享策略事务锁下安装并切换引用；失败不覆盖原策略引用或删除原活动数据库。正常更新清理无关哈希文件，保留当前及本次替换前的数据。进程退出或文件写入后数据库提交失败时可能留下未引用文件，后续成功更新会清理匹配命名规则的旧数据库；临时文件或删除失败的残留可由运维检查。

查询使用不可变内存快照；更新期间已开始的注释仍可使用其捕获的旧版本。文件被手动移除时，已载入的内存快照可能继续可用，重启后须有原文件。接口不返回服务器文件路径。

多实例共享策略中的哈希，但每个实例须有相同哈希的数据文件。应通过定向实例地址逐个上传同一文件，或挂载受控的共享 route-asn 子目录；不要仅向负载均衡地址上传一次就认为全部实例已就绪。向其他实例安装同一哈希不改变全局配置版本。缺文件、文件哈希不符或无法读取时该实例不回退使用另一版本，而报告不可用；不会自动向外部补查。

配置随数据库备份保留，MMDB 文件不打入配置备份。恢复到其他实例后需重新提供相同文件；数据库缺失不会伪装为已有归属。检测历史按现有运行观测规则在恢复时清空。

## 验证

使用固定版本、MIT 许可的 MaxMind 合成测试库（来源和许可在 internal/asn/testdata），不将其当成真实互联网归属。

覆盖 IPv4/IPv6、私网与不命中、损坏和不兼容数据库、默认关闭/显式开启、上传大小与版本检查、存储回滚、数据更新后的历史冻结、Agent 归属清除、地址数量限制、配置损坏恢复、多实例缺文件/哈希不符、相同文件部署及权限。实际查询准确度仍取决于部署时提供的数据。
