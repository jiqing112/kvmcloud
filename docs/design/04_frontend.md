# 前端交互建议（Vue 3 + TypeScript）

技术栈：Vue 3 `<script setup>` + Pinia + Vue Router + Element Plus（或 Naive UI）+
ECharts + Axios。统一走 `composables/` 封装轮询与幂等。

```
web/src/
├── api/                     # axios 实例（自动携带 Idempotency-Key）
│   ├── tasks.ts             # GET /api/v1/tasks/:id
│   ├── images.ts            # image-sources / remote-images / local-images
│   └── vms.ts / nodes.ts / billing.ts
├── composables/
│   ├── useTaskPolling.ts    # 202 + task_id → 指数退避轮询，终态停止
│   └── useIdempotency.ts    # 每"意图"生成一次 uuid 键，重试复用
└── views/
    ├── nodes/NodeEnrollDialog.vue
    ├── vms/BandwidthChart.vue / CreateVMForm.vue
    ├── billing/TrafficPackages.vue
    └── images/ImageMarket.vue / ImageSourceList.vue
```

## 1. 节点添加（NodeEnrollDialog）

- 表单：选择集群 + 可选期望 Agent 证书 SPKI 指纹 → `POST /nodes/install-token`。
- 展示 `install_command`，**一键复制**（`navigator.clipboard.writeText` +
  复制成功 toast）；命令区用等宽字体 + 折叠。
- **Token 有效期倒计时**：以响应 `expires_at` 计算 `useCountdown`；剩余 <2min
  变红并禁用复制，过期后按钮变为"重新生成"。
- 安全：命令与 Token 只在内存（`ref`），不进 Pinia/持久化，组件卸载即清空；
  页面永不展示第二次（刷新即重新签发）。

## 2. 带宽监控图表（BandwidthChart）

- 数据源：`GET /vms/:id/bandwidth-metrics`（ingress/egress kbps 序列），
  `stale=true` 时图表顶部显示"数据延迟"黄条。
- ECharts line 双序列（入/出），`dataZoom` 支持时间窗缩放；轮询间隔 5s，
  页面不可见时暂停（`document.visibilityState`）。
- **阈值告警**：阈值取自 `bandwidth_policies.max_kbps`（创建/调度响应缓存）；
  egress 超阈值 → 曲线标红点 + 右侧告警列表（`ElNotification`）；
  阈值线用 `markLine` 虚线展示。

## 3. 流量包余量（TrafficPackages）

- 进度条：`remaining_bytes / total_bytes`（`ElProgress`，<10% 变红）；
  字节数以 `formatBytes` 人类化（GB/TB，注意后端返回**十进制字符串**防精度丢失）。
- **FIFO 抵扣明细**：展开行展示 `traffic_debits`（时间、用量、命中包裹序号、
  超额部分单独标灰）；抵扣由后端引擎完成，前端只读渲染，不做本地估算。
- 购买：选商品 + 数量 → 402/409（余额不足）友好提示并引导充值。

## 4. 镜像源管理（ImageSourceList + ImageMarket）

- 源列表：名称 / 类型 Tag（domestic=绿、overseas=蓝、custom=橙）/ 优先级 /
  状态 / `last_synced_at`（相对时间）；行内"同步"按钮 → `POST .../sync` →
  返回 202 + task_id → `useTaskPolling` 完成后刷新远端列表。
- **远端镜像多选拉取**：`ElTable` 多选 + 工具栏"拉取到本地"（批量并发受控
  `p-limit(2)`）；OS 类型弹窗确认（linux/windows 决定注入路径）。
- **本地镜像状态轮询**：`pull_state` 颜色 Tag——
  `pending`=灰 / `downloading`=蓝 / `verifying`=蓝闪 / `converting`=橙 /
  `injecting`=紫 / `available`=绿 / `failed`=红（hover 显示 error_code）；
  非终态行每 3s 轮询，终态停止；失败行提供"重试"（同幂等键语义重新 pull）。

## 5. 创建 VM 表单（CreateVMForm）

- **镜像选择双模式**（Radio 切换）：
  - 本地已有：下拉 `GET /local-images?state=available`；
  - 远端按需拉取：下拉 `GET /remote-images`（支持按源/架构过滤）。
- 选**远端**时：表单下方出现警示条——"该镜像将异步拉取（下载→校验→转换→注入），
  创建请求会立即返回任务 ID，虚拟机将在镜像就绪后自动开始创建"；
  同时显示源提供的 `size_bytes` 与预计时长；`image_os_type` 随所选镜像联动。
- 提交：携带 `Idempotency-Key`；`202 + image_pull_required:true` →
  跳转任务详情页并轮询；`409 quota_exceeded / capacity_unavailable` →
  定位到对应表单分区标红。
- 所有规格输入（vCPU/内存/磁盘/带宽 min·max/优先级）做前端即时校验，
  与后端 422 错误码一一映射，避免无效往返。
