# 贡献指南

## 开发环境

- Go 1.22+（`go version`）
- Node 20+（前端）
- PostgreSQL 14+（本地开发可用 Docker）
- protoc 28+ 与 `protoc-gen-go`/`protoc-gen-go-grpc`（仅改动 `api/proto` 时需要）

## 目录速览

```
api/proto/          gRPC 协议定义（注册/心跳/QoS）
cmd/control         控制面入口（HTTP 8080 / gRPC 9443）
cmd/agent           计算节点 Agent 入口
internal/model      数据模型与状态常量
internal/store      持久层（squirrel 构建器，源码零 SQL 文本；migration.sql 内嵌 DDL）
internal/bandwidth  带宽仲裁纯函数（保障/借用/抢占）
internal/monitor    四维水位计算与告警
internal/grpcserver 节点注册/心跳 gRPC 服务端
internal/api        REST API（Gin）
internal/agentlib   Agent 侧 TC HTB QoS / 采集 / 气球 / KSM
deploy/             systemd 单元、SQL
web/                Vue3 控制台
install.sh          三形态安装（all-in-one / control / agent）
```

## 常用命令

```bash
make build      # 编译 control + agent 到 bin/
make test       # 全部单元测试
make coverage   # 覆盖率
make lint       # go vet
make frontend   # 构建前端
```

## 提交规范

- feat: 新功能
- fix: 缺陷修复
- perf: 性能优化
- docs/test/chore: 文档/测试/杂项

PR 请附：变更动机、验证方式（测试输出）、对四维超开或带宽语义的影响说明（如涉及）。

## 设计红线（评审必查）

1. **四维一致性**：任何新资源路径必须同时校验/更新 vCPU、内存、磁盘、带宽四个维度，
   复用 `store.PlaceVM` / `store.CheckOvercommit`，禁止绕行。
2. **保障值不可压穿**：抢占后 victim 的 `current_ceil_kbps >= min_kbps`，
   由 `bandwidth.Decide` 保证，改动需同步单测。
3. **同池串行化**：借用/回收必须经过 `pg_advisory_xact_lock(hashtext(pool_id))`。
4. **源码零 SQL 文本**：语句一律经 squirrel 构建器；参数一律绑定占位符。
5. **凭据不落源码**：只从环境变量读取；`config/*.env.example` 不含可用凭据。
6. **命令白名单**：Agent 新增命令执行需在 `agentlib.ExecRunner.Run` 白名单注册并写测试。
