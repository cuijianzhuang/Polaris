# 回程国家、省份、城市归属

入口：**设置 → 网络检测 → 地理归属**。与 ASN 分别管理数据库、开关、版本和内存快照；默认关闭。安装城市库不会安装或启用 ASN，反之亦然。

## 数据库和安装

支持含 MaxMind City 字段的 `GeoLite2-City`、`GeoIP2-City` 等 MMDB。后台检查数据库类型、IPv4/IPv6 地址族、完整性与兼容字段；ASN 库、Country 库和规则分类库不能用于此处。

- [wp-statistics/GeoLite2-City](https://github.com/wp-statistics/GeoLite2-City)：提供 GitHub 与 jsDelivr `GeoLite2-City.mmdb.gz` 下载入口。
- [lwpk110/free-geoip-databases](https://github.com/lwpk110/free-geoip-databases/releases?q=geolite2&expanded=true)：备用来源选择 `GeoLite2-City-日期.mmdb`。

可在后台选择来源后点击“从仓库获取并安装”，由当前实例直接下载；也可在浏览器下载后手动上传。不做定时自动更新，不解析检测目标域名，不使用外部 IP 查询 API。底层数据使用条款以原始提供者说明为准。

原始 `.mmdb` 上限 128 MiB；`.mmdb.gz` 压缩输入上限 64 MiB，解压结果上限 128 MiB。按内容识别 gzip 并验证校验和和完整性，解压超过限制、文件截断、损坏或格式不符均拒绝。磁盘只存解压后的 MMDB，SHA-256 也针对解压后的内容；原始和压缩版相同内容视为同一数据库。

首次安装后需显式开启注释，更新保留原开关。内容验证和暂存完成后，在独立策略锁下安装文件并以 `expectedRevision` 比较切换引用；冲突或数据库事务失败不覆盖旧策略，也不删除其数据库。保留当前和上次文件，成功更新时回收更旧的哈希文件。损坏策略可通过界面重新上传有效文件修复，修复后默认关闭。

文件在当前实例 DataDir 的 `route-geo` 目录，哈希命名、0600 权限。不允许指定服务端路径、文件名或下载 URL。多实例须分别安装相同内容或挂载受控共享目录；全局策略哈希一致不表示每个实例都有文件。文件缺失、不可读、校验不符或无效时显示未生效，不回退到旧版本。已加载的不可变内存快照可继续服务当前进程，重启后须有对应文件。

配置备份包含策略，但不包含城市 MMDB。迁移、恢复到新实例后须重新上传相同数据；不会根据空文件伪造地理归属。

## 管理接口

仅完整管理员会话可用，API 令牌、匿名请求和节点操作员不能管理城市库：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /tools/route-geo` | `policy`、当前实例 `databaseState`、类型/构建时间/IP 版本和 `effectiveEnabled` |
| `POST /tools/route-geo/database/fetch` | `{"source":"wp-statistics","expectedRevision":0}`；还支持 `wp-statistics-cdn`、`free-geoip`，直接下载对应 City 文件并安装 |
| `PUT /tools/route-geo/database?expectedRevision=0` | 原始二进制 MMDB 或 gzip 上传 |
| `PUT /tools/route-geo` | `{"enabled":true,"expectedRevision":1}`；启用前要求当前实例数据库就绪 |
| `POST /tools/route-geo/lookup` | `{"ip":"81.2.69.160"}`；本地预览，不改变开关、策略或检测历史 |

版本冲突返回 409。上传及开关接口保留与 ASN 一致的可选版本字段，仓库获取必须提供版本；管理界面总是带上实际版本。相同内容重复安装、相同开关重复保存不递增版本。缺少有效数据库的查询返回 409 和具体 `databaseState`。

查询接受单个 IPv4/IPv6 字面量，拒绝域名、URL、端口、CIDR 和 zone。返回 `match`、实际使用的 `databaseSHA256` 和 `effectiveEnabled`。关闭注释后仍可预览已安装的城市库。

`match` 包含：`state`、规范化 `ip`、`countryCode`、`country/countryEn`、`province/provinceEn`、`city/cityEn`、匹配的 `network`。省份取第一层 subdivision；不拿 registered_country 推测实际位置，也不根据已知国家补全省市。中文名不存在时可回退英文，英文 UI 优先使用英文名；文本去控制字符并限制长度。

私网、回环、链路本地、共享地址空间和非单播地址不查询；缺失覆盖返回未收录；IPv4 库对 IPv6 地址返回未收录。地理信息是数据库估计，不代表精确物理位置、设备使用者或真实 BGP 路径。

## 回程历史

新检测在面板端按本地快照为每跳前 4 个地址注释：

- `result.geo`：disabled / unavailable / ready、原因、来源 SHA-256、数据库类型、构建时间、IP 版本、注释时间及受限标志。
- `hops[].geo`：对应地址的国家、省份、城市及匹配状态。

始终清除 Agent 提交的这些字段。关闭、缺文件或数据库不可用时，只记录注释状态，不改变实际网络检查结果。手动和定时检测共用同一条执行链路。

新记录保存完整注释快照，旧记录不会在读取、更新数据库或切换语言时重新查询。路径变化比较仍仅依据 IP/TTL，不依赖地理归属，因此仅更新数据库不会触发线路变化通知。界面旧记录缺少 geo 字段时显示空值。

## 验证

- 固定版本的 MaxMind MIT 合成 City 测试库，验证 IPv4/IPv6、中文/英文、缺失省市、私网、未收录、损坏数据库和 gzip 解压边界。
- 管理接口测试覆盖启用、独立策略、格式、CAS、原始/压缩去重、回滚、文件保留、缺文件/哈希不符、权限及策略恢复。
- 回程链路测试覆盖 Agent 伪造字段清除、快照冻结、每跳地址数量限制及更新数据库不改变路径比较。
- `web/scripts/city-settings-ui-smoke.py` 使用模拟 API 覆盖 390/1440 中英文、压缩上传、拒绝 ASN、本地查询、版本冲突、实例状态和回程详情展示；`asn-settings-ui-smoke.py` 验证已有 ASN 页面。
- 已使用 wp-statistics 实际压缩文件验证 Polaris 解析与解压。生产数据库未加入仓库。

仓库下载限制、版本校验、代理及 SHA-256 处理参见 [ASN 仓库获取说明](route-asn.md#仓库直接获取)。城市压缩文件直接下载后使用同一解压、校验、安装链路，不经过外部 IP 查询服务。
