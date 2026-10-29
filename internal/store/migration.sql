-- kvmcloud 数据迁移（幂等，PostgreSQL 14+）
-- 与 deploy/sql/001_schema.sql 保持一致，由控制面启动时自动执行。

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS tenants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(128) NOT NULL UNIQUE,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 节点：四维物理容量 + 四维超开比 + 四维已分配
CREATE TABLE IF NOT EXISTS nodes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    hostname VARCHAR(255) NOT NULL UNIQUE,
    cluster_id UUID,
    status VARCHAR(32) NOT NULL DEFAULT 'online',

    physical_vcpus INT NOT NULL,
    physical_mem_mb BIGINT NOT NULL,
    physical_disk_gb BIGINT NOT NULL,
    physical_nic_bandwidth_kbps BIGINT NOT NULL,

    cpu_overcommit_ratio  NUMERIC(4,2) NOT NULL DEFAULT 4.00,
    mem_overcommit_ratio  NUMERIC(4,2) NOT NULL DEFAULT 1.50,
    disk_overcommit_ratio NUMERIC(4,2) NOT NULL DEFAULT 2.00,
    bw_overcommit_ratio   NUMERIC(4,2) NOT NULL DEFAULT 1.00,

    allocated_vcpus INT NOT NULL DEFAULT 0,
    allocated_mem_mb BIGINT NOT NULL DEFAULT 0,
    allocated_disk_gb BIGINT NOT NULL DEFAULT 0,
    allocated_min_kbps BIGINT NOT NULL DEFAULT 0,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (cpu_overcommit_ratio  >= 1.00),
    CHECK (mem_overcommit_ratio  >= 1.00),
    CHECK (disk_overcommit_ratio >= 1.00),
    CHECK (bw_overcommit_ratio   >= 1.00)
);

CREATE TABLE IF NOT EXISTS vms (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    node_id UUID NOT NULL REFERENCES nodes(id),
    name VARCHAR(128) NOT NULL,
    flavor VARCHAR(64) NOT NULL,
    vcpus INT NOT NULL,
    mem_mb BIGINT NOT NULL,
    disk_gb BIGINT NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'creating',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_vms_node ON vms(node_id);
CREATE INDEX IF NOT EXISTS idx_vms_tenant ON vms(tenant_id);

CREATE TABLE IF NOT EXISTS vnics (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    vm_id UUID NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
    mac VARCHAR(32) NOT NULL UNIQUE,
    ipv4 INET,
    ipv6 INET,
    bridge VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_vnics_vm ON vnics(vm_id);

CREATE TABLE IF NOT EXISTS shared_bandwidth_pools (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    name VARCHAR(128) NOT NULL,
    total_capacity_kbps BIGINT NOT NULL,
    allocated_min_kbps BIGINT NOT NULL DEFAULT 0,
    current_borrowed_kbps BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS port_bandwidth_policies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    vnic_id UUID NOT NULL REFERENCES vnics(id) ON DELETE CASCADE,
    direction VARCHAR(16) NOT NULL CHECK (direction IN ('ingress','egress')),
    min_kbps BIGINT NOT NULL DEFAULT 0,
    max_kbps BIGINT NOT NULL,
    shared_pool_id UUID REFERENCES shared_bandwidth_pools(id),
    priority INT NOT NULL DEFAULT 100,
    current_ceil_kbps BIGINT,
    state VARCHAR(32) NOT NULL DEFAULT 'guaranteed',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (vnic_id, direction),
    CHECK (min_kbps <= max_kbps)
);

CREATE INDEX IF NOT EXISTS idx_policies_pool ON port_bandwidth_policies(shared_pool_id);

CREATE TABLE IF NOT EXISTS borrow_events (
    id BIGSERIAL PRIMARY KEY,
    policy_id UUID NOT NULL REFERENCES port_bandwidth_policies(id),
    requested_kbps BIGINT NOT NULL,
    granted_kbps BIGINT NOT NULL,
    reason VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS overcommit_alerts (
    id BIGSERIAL PRIMARY KEY,
    node_id UUID NOT NULL REFERENCES nodes(id),
    dimension VARCHAR(16) NOT NULL,
    allocated BIGINT NOT NULL,
    physical BIGINT NOT NULL,
    ratio NUMERIC(4,2) NOT NULL,
    severity VARCHAR(16) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_alerts_node ON overcommit_alerts(node_id, created_at DESC);

-- v0.2.1: 建机任务回写与失败原因
ALTER TABLE vms ADD COLUMN IF NOT EXISTS error TEXT NOT NULL DEFAULT '';
