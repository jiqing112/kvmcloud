# kvmcloud 部署指南

四种使用路径：**单机全栈**（开发/体验/小规模生产）、**主控+多节点**（生产集群）、
**单节点接入已有主控**（扩容）、**源码部署**。

> 通用端口规划：HTTP 8080 / gRPC 9443 / PostgreSQL 5432 / Redis 6379 / NATS 4222。
> Agent 不监听端口（主动连主控）。

---

## 0. 准备：获取二进制

方式 A —— Release 包（推荐）：

```bash
# 从 GitHub Releases 下载 kvmcloud-control 与 kvmcloud-agent
# 或 make release 生成本地发行包（goreleaser）
```

方式 B —— 源码编译：

```bash
git clone https://github.com/ojoti/kvmcloud.git kvmcloud
cd kvmcloud && make build
# 产物：bin/kvmcloud-control  bin/kvmcloud-agent
```

---

## 1. 单机全栈（all-in-one）

```bash
export DATABASE_URL="postgres://kvmcloud:<密码>@127.0.0.1:5432/kvmcloud"
sudo -E ./install.sh all-in-one
```

安装器行为：

1. 装 PostgreSQL/Redis（**只装一份**），启动 `cloud-control`；
2. 自动生成注册令牌 `/etc/cloud/join-token`（0600）；
3. 装 KVM/libvirt，Agent 以 `CONTROL_ADDR=127.0.0.1:9443` 接入（不经外部网络）；
4. systemd 顺序：`cloud-agent.service` `After=cloud-control.service`。

验证：

```bash
systemctl is-active cloud-control cloud-agent   # 均 active
curl -s http://127.0.0.1:8080/healthz           # {"status":"ok"...}
curl -s http://127.0.0.1:8080/api/v1/nodes      # 至少 1 个节点 online
```

---

## 2. 主控 + 多节点（生产推荐）

### 2.1 主控机

```bash
export DATABASE_URL="postgres://kvmcloud:<密码>@<db-host>:5432/kvmcloud"
export CONTROL_JOIN_TOKEN="<随机 40+ 位令牌>"   # 或让安装器生成
sudo -E ./install.sh control
# 安装器会打印/保存令牌：cat /etc/cloud/join-token
```

可选令牌策略（`/etc/cloud/control.env`）：

```ini
CONTROL_JOIN_TOKEN_TTL_HOURS=24       # 有效期
CONTROL_JOIN_TOKEN_SINGLE_USE=false   # 一次性使用
```

### 2.2 每台计算节点

```bash
sudo ./install.sh agent \
  --control-addr 10.0.0.10:9443 \
  --token <JOIN_TOKEN> \
  --node-name node-01 \
  --cpu-ratio 4 --mem-ratio 1.5 --disk-ratio 2 --bw-ratio 1
```

- agent 模式 **不安装 PG/Redis/NATS**，只装 KVM/libvirt/TC；
- 注册幂等：同 `--node-name` 重装返回同一节点 ID；
- 验收：主控上 `GET /api/v1/nodes` 可见全部节点且 `status=online`。

---

## 3. 单节点接入已有主控

与 2.2 完全一致（节点零控制面依赖）：

```bash
sudo ./install.sh agent --control-addr 10.0.0.10:9443 --token <JOIN_TOKEN>
```

已装环境跳过系统依赖：

```bash
sudo ./install.sh agent --control-addr 10.0.0.10:9443 --token <JOIN_TOKEN> --no-deps
```

---

## 4. 源码直跑（开发）

```bash
# 1) 准备数据库
createdb kvmcloud

# 2) 控制面
export DATABASE_URL="postgres://postgres@127.0.0.1:5432/kvmcloud?sslmode=disable"
export CONTROL_JOIN_TOKEN="dev-token-123"
go run ./cmd/control

# 3) Agent（另一终端；无 KVM 的开发机可正常注册，libvirt 相关功能自动降级）
export CONTROL_ADDR="127.0.0.1:9443"
export JOIN_TOKEN="dev-token-123"
export NODE_NAME="dev-node"
go run ./cmd/agent
```

---

## 5. 放置 VM 与带宽语义

```bash
# 建共享池：100 Mbps
curl -X POST http://127.0.0.1:8080/api/v1/pools \
  -H 'Content-Type: application/json' \
  -d '{"tenant_name":"t1","name":"pool-a","total_capacity_kbps":100000}'

# 放置 VM（四维校验：cpu/mem/disk/bw 任一超限返回 409 overcommit_exceeded）
curl -X POST http://127.0.0.1:8080/api/v1/vms \
  -H 'Content-Type: application/json' \
  -d '{"tenant_name":"t1","name":"vm-1","flavor":"2c4g","vcpus":2,"mem_mb":4096,
       "disk_gb":40,"min_kbps":10000,"max_kbps":50000,
       "shared_pool_id":"<pool-id>"}'

# 借用（池余量足→granted；不足→抢占低优先级→仍不足→denied）
curl -X POST http://127.0.0.1:8080/api/v1/policies/<policy-id>/borrow \
  -H 'Content-Type: application/json' -d '{"requested_kbps":30000}'

# 回收借用
curl -X POST http://127.0.0.1:8080/api/v1/policies/<policy-id>/release

# 借用审计 / 水位告警
curl -s http://127.0.0.1:8080/api/v1/events
curl -s http://127.0.0.1:8080/api/v1/alerts

# 节点超开比调优（在线生效）
curl -X PUT http://127.0.0.1:8080/api/v1/nodes/<node-id>/ratios \
  -H 'Content-Type: application/json' \
  -d '{"cpu_overcommit_ratio":4,"mem_overcommit_ratio":1.5,
       "disk_overcommit_ratio":2,"bw_overcommit_ratio":1}'
```

---

## 6. 卸载

```bash
sudo ./uninstall.sh agent    # 仅计算节点
sudo ./uninstall.sh control  # 仅主控（不删数据库）
sudo ./uninstall.sh all      # 全部
```

---

## 7. 故障排查

| 现象 | 排查 |
| --- | --- |
| Agent 起不来 | `journalctl -u cloud-agent`；确认 `/etc/cloud/agent.env` 的 CONTROL_ADDR/JOIN_TOKEN |
| 注册 403 | 主控未配置 `CONTROL_JOIN_TOKEN` 或令牌过期/已一次性消耗 |
| 放置 409 overcommit_exceeded | 错误信息含维度名（cpu/mem/disk/bw），调超开比或换节点 |
| 借用 409 | 抢占后仍不足（`denied`），查看 `/api/v1/events` |
| 心跳断、节点 offline | 看门狗 90s 无心跳自动置 offline；恢复心跳自动回 online |
