# 贡献指南

感谢参与 Polaris。本文说明开发环境、提交前需要通过的检查，以及提交 PR 的约定。使用问题请先看[用户手册](docs/user-guide.md)和[故障排查](docs/user-guide.md#troubleshooting)；安全漏洞请按[安全策略](SECURITY.md)私下报告。

## 开发环境

- Go 1.25 或更新的兼容版本
- Node.js 22 与 npm（前端构建产物嵌入主控二进制，修改前端后要重新构建主控）
- 可选：PostgreSQL 与 Redis（多实例相关功能的测试）、真实内核二进制（内核集成测试）

```bash
git clone https://github.com/cuijianzhuang/Polaris.git
cd Polaris
make web build   # 安装锁定的前端依赖，构建前端、主控和 Agent
make test        # go vet + Go 测试
```

本地运行主控需要配置密钥、数据目录和监听地址，见[配置项](docs/user-guide.md#configuration)。前端开发服务器：

```bash
cd web
npm run dev      # 后端需同时运行，并使用根后台路径 /
```

## 目录结构

| 目录 | 内容 |
| --- | --- |
| `cmd/panel`、`cmd/agent` | 主控与 Agent 入口 |
| `api/proto` | 主控与 Agent 的通信定义（`buf generate` 生成代码） |
| `internal/panel` | 主控 API、调度、计费、通知等 |
| `internal/agent` | 节点 Agent：配置同步、内核管理、流量采集 |
| `internal/core` | 各内核适配器与能力矩阵 |
| `internal/store`、`internal/model` | 数据模型与存储（SQLite / PostgreSQL） |
| `internal/sub` | 订阅生成与格式转换 |
| `internal/license` | 许可证校验 |
| `web` | React + TypeScript + Vite 管理后台 |
| `docs` | 用户手册、运维与设计文档，见[文档索引](docs/README.md) |

## 提交前检查

CI 会运行以下检查，提交前请在本地先跑通与改动相关的部分：

```bash
# Go
test -z "$(gofmt -l .)"
go vet ./...
go test -count=1 ./...

# 前端（改动了 web/ 时）
cd web
npm ci
npm run i18n:check   # 新增界面文案必须同时提供英文翻译
npm run build
```

按改动范围补充：

- **数据库相关**：PostgreSQL 上也要跑通。准备一个空库后执行
  `POLARIS_TEST_ALL_POSTGRES=1 POLARIS_TEST_POSTGRES_DSN='postgres://user@127.0.0.1:5432/db?sslmode=disable' go test -count=1 ./internal/panel/`。
  Redis 相关同理使用 `POLARIS_TEST_ALL_REDIS=1 POLARIS_TEST_REDIS_URL=…`。
- **内核相关**：`make test-cores XRAY=/path/to/xray SINGBOX=/path/to/sing-box`。没有提供二进制时相关测试会跳过，CI 会用真实内核再跑一遍。
- **并发相关**：加上 `-race`。
- **专业版**：有 `polaris-pro` 访问权限时，用 `scripts/pro.sh link ../polaris-pro` 链接后执行 `go vet -tags pro ./...` 和对应测试，结束后 `scripts/pro.sh clean`。不加 `pro` 标签始终构建社区版，社区版必须能独立编译和运行。

## 编写约定

- **界面文案**：中文为源语言，英文翻译放在 `web/src/locales/en/`，由 `npm run i18n:check` 检查。
- **数据模型**：新增表或字段要同时考虑 SQLite 和 PostgreSQL（类型、长度、默认值），以及旧数据和旧备份的兼容。
- **后台任务**：只应由主节点执行的定时任务放在主节点任务列表里；后台状态写入要检查错误并记录日志。
- **权限**：新接口默认只对超级管理员开放；需要给 API 令牌、节点管理员或其他角色使用时，显式加入对应权限并补测试。
- **Agent 兼容**：主控和 Agent 可能版本不一致。新能力通过 Agent 上报的能力标记协商，旧 Agent 要有明确的拒绝或降级行为。
- **测试**：修 bug 先写能复现问题的测试；新功能覆盖主要路径和边界情况。
- **文档**：影响用户操作的改动同步更新[用户手册](docs/user-guide.md)；较大的功能在 `docs/design/` 写一份设计说明，并加入[文档索引](docs/README.md)。

## 提交 PR

1. 从最新的 `main` 拉分支，一个 PR 只做一件事。
2. PR 描述写清楚：改了什么、为什么改、怎么验证的（跑了哪些测试，界面改动附截图）。
3. 确认 CI 全部通过；有失败先修，不要跳过或禁用测试。
4. 涉及数据库结构、权限、计费或 Agent 协议的改动，在描述里单独说明兼容性和升级影响。

## 许可证

本仓库采用 [AGPL-3.0-only](LICENSE)。提交贡献即表示你同意以该许可证发布你的代码。
