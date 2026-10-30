-- =====================================================================
-- KVMCloud 全量 DDL 快照（评审用单文件视图）
-- 权威来源（以此为准，勿在本文件直接改）：
--   deploy/sql/core/001_schema.sql   12 大业务模块核心表 + RLS + append-only
--   deploy/sql/core/002_roles.sql    数据库角色与最小授权
--   deploy/sql/core/003_images.sql   镜像模块三表 + vms.image_source_name
-- 应用顺序：002_roles → 001_schema → 003_images；以 migration-owner 角色执行，
-- 禁止使用 HTTP 服务角色（kvmcloud_core_api）执行迁移。
-- =====================================================================

-- Run once as the role administrator after 001_schema.sql.
-- NOLOGIN group roles: provision login credentials through your secret manager.
BEGIN;
CREATE ROLE kvmcloud_core_api NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
CREATE ROLE kvmcloud_core_enrollment NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
GRANT USAGE ON SCHEMA kvmcloud_core TO kvmcloud_core_api, kvmcloud_core_enrollment;

GRANT SELECT ON kvmcloud_core.tenants, kvmcloud_core.images,
    kvmcloud_core.networks, kvmcloud_core.nodes, kvmcloud_core.tenant_quotas,
    kvmcloud_core.shared_bandwidth_pools, kvmcloud_core.idempotency_keys,
    kvmcloud_core.vms, kvmcloud_core.tasks, kvmcloud_core.audit_logs TO kvmcloud_core_api;
GRANT UPDATE (used_vcpus, used_memory_mib, used_disk_gib, used_min_kbps)
    ON kvmcloud_core.nodes TO kvmcloud_core_api;
GRANT UPDATE (used_vms, used_vcpus, used_memory_mib, used_disk_gib, used_min_kbps, version)
    ON kvmcloud_core.tenant_quotas TO kvmcloud_core_api;
GRANT UPDATE (guaranteed_kbps, version) ON kvmcloud_core.shared_bandwidth_pools TO kvmcloud_core_api;
-- PostgreSQL row locking requires UPDATE privilege on at least one column.
GRANT UPDATE (name) ON kvmcloud_core.tenants, kvmcloud_core.networks TO kvmcloud_core_api;
GRANT INSERT ON kvmcloud_core.vms, kvmcloud_core.vm_nics, kvmcloud_core.volumes,
    kvmcloud_core.bandwidth_policies, kvmcloud_core.resource_reservations,
    kvmcloud_core.tasks, kvmcloud_core.outbox_events, kvmcloud_core.audit_logs,
    kvmcloud_core.idempotency_keys TO kvmcloud_core_api;
GRANT SELECT, UPDATE (response_status, response_body, location)
    ON kvmcloud_core.idempotency_keys TO kvmcloud_core_api;

GRANT SELECT ON kvmcloud_core.clusters TO kvmcloud_core_enrollment;
GRANT INSERT ON kvmcloud_core.node_join_issuances, kvmcloud_core.audit_logs TO kvmcloud_core_enrollment;
CREATE POLICY enrollment_audit_insert ON kvmcloud_core.audit_logs
    FOR INSERT TO kvmcloud_core_enrollment WITH CHECK (
        tenant_id IS NULL AND action = 'nodes.install-token' AND resource_type = 'node'
    );

-- 镜像模块（003_images.sql 的授权在 003 内联执行：GRANT 依赖其表先行创建）。
-- No broad worker grants: enrollment-completion, scheduler/outbox and billing
-- workers require separate roles with explicit grants for their own operations.
-- Never grant a tenant HTTP login membership in any of these worker roles.
COMMIT;

-- ================= 001_schema.sql（12 大模块核心表） =================

-- Standalone baseline, PostgreSQL 14+. NOT an upgrade of public.*.
-- Apply with a migration-owner role, never with the HTTP service role.
BEGIN;
CREATE SCHEMA kvmcloud_core;
SET LOCAL search_path = kvmcloud_core, pg_catalog;

CREATE TABLE tenants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name varchar(128) NOT NULL UNIQUE,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','suspended')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE principals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject text NOT NULL UNIQUE,
    disabled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tenant_memberships (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    principal_id uuid NOT NULL REFERENCES principals(id),
    role text NOT NULL CHECK (role IN ('owner','operator','viewer','billing')),
    PRIMARY KEY (tenant_id, principal_id)
);

CREATE TABLE api_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    principal_id uuid NOT NULL,
    key_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(key_sha256) = 32),
    scopes text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, principal_id)
        REFERENCES tenant_memberships(tenant_id, principal_id)
);

CREATE TABLE clusters (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name varchar(128) NOT NULL UNIQUE,
    region varchar(64) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE node_join_issuances (
    id uuid PRIMARY KEY,
    cluster_id uuid NOT NULL REFERENCES clusters(id),
    node_id uuid NOT NULL UNIQUE,
    issued_by uuid NOT NULL REFERENCES principals(id),
    token_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(token_sha256) = 32),
    expected_spki_sha256 text CHECK (expected_spki_sha256 ~ '^[0-9a-f]{64}$'),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);

CREATE TABLE nodes (
    id uuid PRIMARY KEY,
    cluster_id uuid NOT NULL REFERENCES clusters(id),
    hostname varchar(253) NOT NULL,
    state text NOT NULL DEFAULT 'joining'
        CHECK (state IN ('joining','online','draining','offline','quarantined')),
    schedulable boolean NOT NULL DEFAULT false,
    agent_spki_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(agent_spki_sha256) = 32),
    physical_vcpus bigint NOT NULL CHECK (physical_vcpus > 0),
    physical_memory_mib bigint NOT NULL CHECK (physical_memory_mib > 0),
    physical_disk_gib bigint NOT NULL CHECK (physical_disk_gib > 0),
    physical_nic_kbps bigint NOT NULL CHECK (physical_nic_kbps > 0),
    cpu_ratio numeric(6,3) NOT NULL DEFAULT 4 CHECK (cpu_ratio BETWEEN 1 AND 64),
    memory_ratio numeric(6,3) NOT NULL DEFAULT 1.5 CHECK (memory_ratio BETWEEN 1 AND 8),
    disk_ratio numeric(6,3) NOT NULL DEFAULT 2 CHECK (disk_ratio BETWEEN 1 AND 64),
    bandwidth_ratio numeric(6,3) NOT NULL DEFAULT 1 CHECK (bandwidth_ratio BETWEEN 1 AND 64),
    used_vcpus bigint NOT NULL DEFAULT 0 CHECK (used_vcpus >= 0),
    used_memory_mib bigint NOT NULL DEFAULT 0 CHECK (used_memory_mib >= 0),
    used_disk_gib bigint NOT NULL DEFAULT 0 CHECK (used_disk_gib >= 0),
    used_min_kbps bigint NOT NULL DEFAULT 0 CHECK (used_min_kbps >= 0),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (cluster_id, hostname),
    CHECK (used_vcpus <= floor(physical_vcpus::numeric * cpu_ratio)),
    CHECK (used_memory_mib <= floor(physical_memory_mib::numeric * memory_ratio)),
    CHECK (used_disk_gib <= floor(physical_disk_gib::numeric * disk_ratio)),
    CHECK (used_min_kbps <= floor(physical_nic_kbps::numeric * bandwidth_ratio))
);
CREATE INDEX nodes_schedule ON nodes(cluster_id, id)
    WHERE state = 'online' AND schedulable;

CREATE TABLE tenant_quotas (
    tenant_id uuid PRIMARY KEY REFERENCES tenants(id),
    max_vms bigint NOT NULL CHECK (max_vms >= 0),
    max_vcpus bigint NOT NULL CHECK (max_vcpus >= 0),
    max_memory_mib bigint NOT NULL CHECK (max_memory_mib >= 0),
    max_disk_gib bigint NOT NULL CHECK (max_disk_gib >= 0),
    max_min_kbps bigint NOT NULL CHECK (max_min_kbps >= 0),
    used_vms bigint NOT NULL DEFAULT 0 CHECK (used_vms BETWEEN 0 AND max_vms),
    used_vcpus bigint NOT NULL DEFAULT 0 CHECK (used_vcpus BETWEEN 0 AND max_vcpus),
    used_memory_mib bigint NOT NULL DEFAULT 0 CHECK (used_memory_mib BETWEEN 0 AND max_memory_mib),
    used_disk_gib bigint NOT NULL DEFAULT 0 CHECK (used_disk_gib BETWEEN 0 AND max_disk_gib),
    used_min_kbps bigint NOT NULL DEFAULT 0 CHECK (used_min_kbps BETWEEN 0 AND max_min_kbps),
    version bigint NOT NULL DEFAULT 1
);

CREATE TABLE images (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid REFERENCES tenants(id),
    name varchar(128) NOT NULL,
    artifact_sha256 bytea NOT NULL CHECK (octet_length(artifact_sha256) = 32),
    storage_ref text NOT NULL,
    state text NOT NULL CHECK (state IN ('verifying','ready','retired')),
    min_disk_gib bigint NOT NULL CHECK (min_disk_gib > 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE networks (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    cluster_id uuid NOT NULL REFERENCES clusters(id),
    name varchar(128) NOT NULL,
    ipv4_cidr cidr,
    ipv6_cidr cidr,
    state text NOT NULL DEFAULT 'ready' CHECK (state IN ('ready','disabled')),
    UNIQUE (tenant_id, id),
    CHECK (ipv4_cidr IS NULL OR family(ipv4_cidr) = 4),
    CHECK (ipv6_cidr IS NULL OR family(ipv6_cidr) = 6)
);

CREATE TABLE shared_bandwidth_pools (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    cluster_id uuid NOT NULL REFERENCES clusters(id),
    name varchar(128) NOT NULL,
    capacity_kbps bigint NOT NULL CHECK (capacity_kbps > 0),
    guaranteed_kbps bigint NOT NULL DEFAULT 0 CHECK (guaranteed_kbps >= 0),
    leased_kbps bigint NOT NULL DEFAULT 0 CHECK (leased_kbps >= 0),
    version bigint NOT NULL DEFAULT 1,
    UNIQUE (tenant_id, id),
    CHECK (guaranteed_kbps::numeric + leased_kbps <= capacity_kbps)
);

CREATE TABLE vms (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    node_id uuid NOT NULL REFERENCES nodes(id),
    image_id uuid REFERENCES images(id),
    domain_uuid uuid UNIQUE,
    network_id uuid NOT NULL,
    name varchar(64) NOT NULL,
    vcpus bigint NOT NULL CHECK (vcpus BETWEEN 1 AND 4096),
    memory_mib bigint NOT NULL CHECK (memory_mib BETWEEN 128 AND 16777216),
    root_disk_gib bigint NOT NULL CHECK (root_disk_gib BETWEEN 1 AND 1048576),
    state text NOT NULL DEFAULT 'creating' CHECK (state IN
        ('creating','running','stopped','resizing','adopting','migrating',
         'recycling','recycled','deleting','deleted','error')),
    desired_version bigint NOT NULL DEFAULT 1,
    observed_version bigint NOT NULL DEFAULT 0,
    deleted_at timestamptz,
    purge_after timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, network_id) REFERENCES networks(tenant_id, id),
    CHECK (observed_version BETWEEN 0 AND desired_version),
    CHECK (purge_after IS NULL OR (deleted_at IS NOT NULL AND purge_after >= deleted_at))
);
CREATE UNIQUE INDEX vms_live_name ON vms(tenant_id, name) WHERE state <> 'deleted';
CREATE INDEX vms_node ON vms(node_id) WHERE state <> 'deleted';
CREATE INDEX vms_recycle ON vms(purge_after) WHERE state IN ('recycling','recycled');

CREATE TABLE vm_nics (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    vm_id uuid NOT NULL,
    network_id uuid NOT NULL,
    mac macaddr NOT NULL UNIQUE,
    private_ipv4 inet,
    private_ipv6 inet,
    primary_nic boolean NOT NULL DEFAULT true,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, vm_id) REFERENCES vms(tenant_id, id),
    FOREIGN KEY (tenant_id, network_id) REFERENCES networks(tenant_id, id),
    CHECK (private_ipv4 IS NULL OR (family(private_ipv4) = 4 AND masklen(private_ipv4) = 32)),
    CHECK (private_ipv6 IS NULL OR (family(private_ipv6) = 6 AND masklen(private_ipv6) = 128))
);
CREATE UNIQUE INDEX vm_nics_primary ON vm_nics(vm_id) WHERE primary_nic;
CREATE UNIQUE INDEX vm_nics_ipv4 ON vm_nics(network_id, private_ipv4) WHERE private_ipv4 IS NOT NULL;
CREATE UNIQUE INDEX vm_nics_ipv6 ON vm_nics(network_id, private_ipv6) WHERE private_ipv6 IS NOT NULL;

CREATE TABLE bandwidth_policies (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    nic_id uuid NOT NULL,
    direction text NOT NULL DEFAULT 'egress' CHECK (direction IN ('ingress','egress')),
    min_kbps bigint NOT NULL CHECK (min_kbps >= 0),
    max_kbps bigint NOT NULL CHECK (max_kbps BETWEEN 1 AND 1000000000000),
    shared_pool_id uuid,
    priority smallint NOT NULL DEFAULT 100 CHECK (priority BETWEEN 0 AND 100),
    desired_version bigint NOT NULL DEFAULT 1,
    observed_version bigint NOT NULL DEFAULT 0,
    UNIQUE (nic_id, direction),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, nic_id) REFERENCES vm_nics(tenant_id, id),
    FOREIGN KEY (tenant_id, shared_pool_id) REFERENCES shared_bandwidth_pools(tenant_id, id),
    CHECK (min_kbps <= max_kbps),
    CHECK (observed_version BETWEEN 0 AND desired_version)
);
CREATE INDEX bandwidth_policies_pool ON bandwidth_policies(shared_pool_id);

CREATE TABLE bandwidth_leases (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    policy_id uuid NOT NULL,
    pool_id uuid NOT NULL,
    node_id uuid NOT NULL REFERENCES nodes(id),
    borrowed_kbps bigint NOT NULL CHECK (borrowed_kbps > 0),
    generation bigint NOT NULL CHECK (generation > 0),
    expires_at timestamptz NOT NULL,
    state text NOT NULL CHECK (state IN ('pending','active','revoking','expired')),
    FOREIGN KEY (tenant_id, policy_id) REFERENCES bandwidth_policies(tenant_id, id),
    FOREIGN KEY (tenant_id, pool_id) REFERENCES shared_bandwidth_pools(tenant_id, id)
);
CREATE INDEX bandwidth_leases_expiry ON bandwidth_leases(expires_at) WHERE state <> 'expired';

CREATE TABLE resource_reservations (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    vm_id uuid NOT NULL,
    node_id uuid NOT NULL REFERENCES nodes(id),
    purpose text NOT NULL CHECK (purpose IN ('create','resize_delta','migration_target','adopt')),
    vcpus bigint NOT NULL CHECK (vcpus >= 0),
    memory_mib bigint NOT NULL CHECK (memory_mib >= 0),
    disk_gib bigint NOT NULL CHECK (disk_gib >= 0),
    min_kbps bigint NOT NULL CHECK (min_kbps >= 0),
    state text NOT NULL DEFAULT 'held' CHECK (state IN ('held','committed','released')),
    released_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, vm_id) REFERENCES vms(tenant_id, id),
    CHECK ((state = 'released') = (released_at IS NOT NULL))
);
CREATE INDEX reservations_vm ON resource_reservations(vm_id, state);

CREATE TABLE volumes (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    node_id uuid NOT NULL REFERENCES nodes(id),
    vm_id uuid,
    name varchar(128) NOT NULL,
    backend text NOT NULL CHECK (backend IN ('qcow2','rbd')),
    backend_ref text NOT NULL,
    role text NOT NULL DEFAULT 'data' CHECK (role IN ('root','data')),
    size_gib bigint NOT NULL CHECK (size_gib > 0),
    state text NOT NULL CHECK (state IN ('creating','available','attached','snapshotting','deleting','deleted','error')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (node_id, backend, backend_ref),
    FOREIGN KEY (tenant_id, vm_id) REFERENCES vms(tenant_id, id),
    CHECK (role <> 'root' OR vm_id IS NOT NULL)
);
CREATE UNIQUE INDEX volumes_root ON volumes(vm_id) WHERE role = 'root' AND state <> 'deleted';

CREATE TABLE volume_snapshots (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    volume_id uuid NOT NULL,
    name varchar(128) NOT NULL,
    consistency text NOT NULL CHECK (consistency IN ('crash','filesystem')),
    state text NOT NULL DEFAULT 'creating' CHECK (state IN ('creating','available','deleting','deleted','error')),
    backend_ref text,
    retained_bytes numeric(20,0) NOT NULL DEFAULT 0 CHECK (retained_bytes >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, volume_id) REFERENCES volumes(tenant_id, id)
);

CREATE TABLE public_ips (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    gateway_node_id uuid NOT NULL REFERENCES nodes(id),
    address inet NOT NULL UNIQUE CHECK (family(address) = 4 AND masklen(address) = 32),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','disabled')),
    UNIQUE (tenant_id, id)
);

CREATE TABLE port_forwardings (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    public_ip_id uuid NOT NULL,
    nic_id uuid NOT NULL,
    protocol text NOT NULL CHECK (protocol IN ('tcp','udp')),
    external_port integer NOT NULL CHECK (external_port BETWEEN 1 AND 65535),
    internal_port integer NOT NULL CHECK (internal_port BETWEEN 1 AND 65535),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','active','deleting','deleted','error')),
    desired_version bigint NOT NULL DEFAULT 1,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, public_ip_id) REFERENCES public_ips(tenant_id, id),
    FOREIGN KEY (tenant_id, nic_id) REFERENCES vm_nics(tenant_id, id)
);
CREATE UNIQUE INDEX port_forwardings_socket
    ON port_forwardings(public_ip_id, protocol, external_port) WHERE state <> 'deleted';

CREATE TABLE security_groups (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    name varchar(128) NOT NULL,
    version bigint NOT NULL DEFAULT 1,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name)
);

CREATE TABLE security_group_rules (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    group_id uuid NOT NULL,
    direction text NOT NULL CHECK (direction IN ('ingress','egress')),
    ethertype smallint NOT NULL CHECK (ethertype IN (4,6)),
    protocol text NOT NULL CHECK (protocol IN ('any','tcp','udp','icmp','icmpv6')),
    remote_cidr cidr NOT NULL,
    port_from integer,
    port_to integer,
    icmp_type smallint CHECK (icmp_type BETWEEN 0 AND 255),
    icmp_code smallint CHECK (icmp_code BETWEEN 0 AND 255),
    FOREIGN KEY (tenant_id, group_id) REFERENCES security_groups(tenant_id, id),
    CHECK (family(remote_cidr) = ethertype),
    CHECK ((protocol IN ('tcp','udp') AND port_from IS NOT NULL AND port_to IS NOT NULL
            AND port_from BETWEEN 1 AND 65535 AND port_to BETWEEN port_from AND 65535)
        OR (protocol NOT IN ('tcp','udp') AND port_from IS NULL AND port_to IS NULL)),
    CHECK (protocol NOT IN ('tcp','udp','any') OR (icmp_type IS NULL AND icmp_code IS NULL)),
    CHECK (protocol <> 'icmp' OR ethertype = 4),
    CHECK (protocol <> 'icmpv6' OR ethertype = 6)
);

CREATE TABLE vm_security_groups (
    tenant_id uuid NOT NULL,
    vm_id uuid NOT NULL,
    group_id uuid NOT NULL,
    PRIMARY KEY (tenant_id, vm_id, group_id),
    FOREIGN KEY (tenant_id, vm_id) REFERENCES vms(tenant_id, id),
    FOREIGN KEY (tenant_id, group_id) REFERENCES security_groups(tenant_id, id)
);

CREATE TABLE traffic_products (
    id uuid PRIMARY KEY,
    name varchar(128) NOT NULL,
    bytes numeric(20,0) NOT NULL CHECK (bytes > 0),
    price_minor bigint NOT NULL CHECK (price_minor >= 0),
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    validity_seconds bigint NOT NULL CHECK (validity_seconds BETWEEN 60 AND 315360000),
    active boolean NOT NULL DEFAULT true
);

CREATE TABLE tenant_wallets (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_minor bigint NOT NULL DEFAULT 0 CHECK (balance_minor >= 0),
    version bigint NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, currency)
);

CREATE TABLE traffic_orders (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    product_id uuid NOT NULL REFERENCES traffic_products(id),
    quantity integer NOT NULL CHECK (quantity BETWEEN 1 AND 1000),
    amount_minor bigint NOT NULL CHECK (amount_minor >= 0),
    currency char(3) NOT NULL,
    state text NOT NULL CHECK (state IN ('paid','refunded')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, currency) REFERENCES tenant_wallets(tenant_id, currency)
);

CREATE TABLE traffic_packages (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    order_id uuid NOT NULL,
    total_bytes numeric(20,0) NOT NULL CHECK (total_bytes > 0),
    remaining_bytes numeric(20,0) NOT NULL CHECK (remaining_bytes BETWEEN 0 AND total_bytes),
    purchased_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, order_id) REFERENCES traffic_orders(tenant_id, id),
    CHECK (expires_at > purchased_at)
);
CREATE INDEX traffic_packages_fifo ON traffic_packages(tenant_id, purchased_at, id)
    WHERE remaining_bytes > 0;

CREATE TABLE usage_reports (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    vm_id uuid NOT NULL,
    node_id uuid NOT NULL REFERENCES nodes(id),
    attachment_epoch uuid NOT NULL,
    sequence bigint NOT NULL CHECK (sequence > 0),
    egress_delta_bytes numeric(20,0) NOT NULL CHECK (egress_delta_bytes >= 0),
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    billed_at timestamptz,
    UNIQUE (vm_id, attachment_epoch, sequence),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, vm_id) REFERENCES vms(tenant_id, id),
    CHECK (period_end > period_start)
);

CREATE TABLE traffic_debits (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    report_id uuid NOT NULL,
    package_id uuid,
    bytes numeric(20,0) NOT NULL CHECK (bytes > 0),
    kind text NOT NULL CHECK (kind IN ('package','overage')),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, report_id) REFERENCES usage_reports(tenant_id, id),
    FOREIGN KEY (tenant_id, package_id) REFERENCES traffic_packages(tenant_id, id),
    CHECK ((kind = 'package') = (package_id IS NOT NULL))
);
CREATE UNIQUE INDEX traffic_debits_package ON traffic_debits(report_id, package_id)
    WHERE package_id IS NOT NULL;
CREATE UNIQUE INDEX traffic_debits_overage ON traffic_debits(report_id) WHERE kind = 'overage';

CREATE TABLE billing_ledger (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    currency char(3) NOT NULL,
    order_id uuid,
    report_id uuid,
    amount_minor bigint NOT NULL,
    kind text NOT NULL CHECK (kind IN ('purchase','overage','refund','topup')),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, order_id) REFERENCES traffic_orders(tenant_id, id),
    FOREIGN KEY (tenant_id, report_id) REFERENCES usage_reports(tenant_id, id)
);
CREATE UNIQUE INDEX billing_order_once ON billing_ledger(order_id, kind) WHERE order_id IS NOT NULL;
CREATE UNIQUE INDEX billing_usage_once ON billing_ledger(report_id, kind) WHERE report_id IS NOT NULL;

CREATE TABLE tasks (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    vm_id uuid,
    volume_id uuid,
    node_id uuid NOT NULL REFERENCES nodes(id),
    kind text NOT NULL CHECK (kind IN ('vm.create','vm.bandwidth','vm.nat','vm.security',
        'vm.resize','vm.adopt','vm.scan','vm.migrate','vm.recycle','vm.restore','vm.purge','volume.snapshot')),
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','running','succeeded','failed','unknown','cancelled')),
    progress smallint NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    resource_version bigint NOT NULL DEFAULT 1,
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    lease_until timestamptz,
    error_code varchar(64),
    safe_result jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, vm_id) REFERENCES vms(tenant_id, id),
    FOREIGN KEY (tenant_id, volume_id) REFERENCES volumes(tenant_id, id)
);
CREATE UNIQUE INDEX tasks_vm_mutation ON tasks(vm_id)
    WHERE vm_id IS NOT NULL AND state IN ('pending','running','unknown');
CREATE UNIQUE INDEX tasks_volume_mutation ON tasks(volume_id)
    WHERE volume_id IS NOT NULL AND state IN ('pending','running','unknown');
CREATE INDEX tasks_tenant_recent ON tasks(tenant_id, created_at DESC, id);

CREATE TABLE outbox_events (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    task_id uuid NOT NULL,
    node_id uuid NOT NULL REFERENCES nodes(id),
    subject text NOT NULL CHECK (subject ~ '^kvmcloud[.]commands[.][0-9a-f-]{36}$'),
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner uuid,
    lease_until timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    FOREIGN KEY (tenant_id, task_id) REFERENCES tasks(tenant_id, id)
);
CREATE INDEX outbox_pending ON outbox_events(next_attempt_at, created_at) WHERE published_at IS NULL;

CREATE TABLE idempotency_keys (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    principal_id uuid NOT NULL REFERENCES principals(id),
    operation varchar(160) NOT NULL,
    key varchar(128) NOT NULL,
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
    response_status integer CHECK (response_status BETWEEN 200 AND 299),
    response_body jsonb,
    location text,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    PRIMARY KEY (tenant_id, principal_id, operation, key),
    CHECK (expires_at > created_at)
);

CREATE TABLE audit_logs (
    id uuid PRIMARY KEY,
    tenant_id uuid REFERENCES tenants(id),
    actor_id uuid NOT NULL REFERENCES principals(id),
    request_id uuid NOT NULL,
    action varchar(128) NOT NULL,
    resource_type varchar(64) NOT NULL,
    resource_id uuid NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('accepted','denied','failed','completed')),
    safe_metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_tenant_time ON audit_logs(tenant_id, created_at DESC, id);

CREATE FUNCTION reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'append-only relation' USING ERRCODE = '42501';
END
$$;
CREATE TRIGGER audit_logs_no_mutation BEFORE UPDATE OR DELETE OR TRUNCATE ON audit_logs
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER billing_ledger_no_mutation BEFORE UPDATE OR DELETE OR TRUNCATE ON billing_ledger
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER traffic_debits_no_mutation BEFORE UPDATE OR DELETE OR TRUNCATE ON traffic_debits
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- RLS is defense in depth; handlers still bind tenant_id explicitly.
-- HTTP connections use a non-owner, non-superuser, NOBYPASSRLS role.
DO $$
DECLARE
    relation_name text;
BEGIN
    FOREACH relation_name IN ARRAY ARRAY[
        'tenants','tenant_memberships','api_keys','tenant_quotas','networks',
        'shared_bandwidth_pools','vms','vm_nics','bandwidth_policies',
        'bandwidth_leases','resource_reservations','volumes','volume_snapshots',
        'public_ips','port_forwardings','security_groups','security_group_rules',
        'vm_security_groups','tenant_wallets','traffic_orders','traffic_packages',
        'usage_reports','traffic_debits','billing_ledger','tasks','outbox_events',
        'idempotency_keys','audit_logs'
    ] LOOP
        EXECUTE format('ALTER TABLE kvmcloud_core.%I ENABLE ROW LEVEL SECURITY', relation_name);
        EXECUTE format('ALTER TABLE kvmcloud_core.%I FORCE ROW LEVEL SECURITY', relation_name);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON kvmcloud_core.%I USING (%I = nullif(current_setting(''app.tenant_id'', true), '''')::uuid) WITH CHECK (%I = nullif(current_setting(''app.tenant_id'', true), '''')::uuid)',
            relation_name,
            CASE WHEN relation_name = 'tenants' THEN 'id' ELSE 'tenant_id' END,
            CASE WHEN relation_name = 'tenants' THEN 'id' ELSE 'tenant_id' END
        );
    END LOOP;
END
$$;

ALTER TABLE images ENABLE ROW LEVEL SECURITY;
ALTER TABLE images FORCE ROW LEVEL SECURITY;
CREATE POLICY images_visible ON images FOR SELECT USING (
    tenant_id IS NULL OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
);
CREATE POLICY images_write ON images FOR ALL USING (
    tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
) WITH CHECK (
    tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
);

REVOKE ALL ON SCHEMA kvmcloud_core FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA kvmcloud_core FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA kvmcloud_core FROM PUBLIC;
COMMIT;

-- ================= 003_images.sql（镜像源/远端/本地 + vms 追溯） =================

-- 003_images.sql — 镜像源管理模块（第二部分）。
-- 前置：001_schema.sql 已应用。以 migration-owner 角色执行，禁止用 HTTP 服务角色。
-- 设计要点：
--   * 下载 URL 只存于 remote_images（控制面同步所得 + Agent 白名单校验后使用），
--     API 层绝不接收任意 URL/文件路径——杜绝 SSRF 与路径穿越。
--   * local_images.storage_ref 由 Agent 回报路径（受根目录前缀约束），HTTP 角色只读。
--   * 远端镜像缓存按 (source_id,name,version,arch,format) 唯一，重复同步走 upsert。
--   * 同一 local_images 名称同时只允许一个未终态（download_loop 活跃唯一索引）。
BEGIN;
SET LOCAL search_path = kvmcloud_core, pg_catalog;

-- 镜像源：国内源(domestic)/海外源(overseas)/自定义源(custom)。
CREATE TABLE image_sources (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid REFERENCES tenants(id),           -- NULL = 平台级源（所有租户可见）
    name varchar(64) NOT NULL,
    kind text NOT NULL CHECK (kind IN ('domestic','overseas','custom')),
    base_url text NOT NULL CHECK (base_url ~ '^https://[A-Za-z0-9.:\[\]-]+(/$|/$[A-Za-z0-9/_.-]*$)'),
    egress_bytes_limit bigint NOT NULL CHECK (egress_bytes_limit BETWEEN 1 AND 1125899906842624), -- 单文件下载上限
    domain_allowlist text[] NOT NULL DEFAULT '{}',   -- Agent 下载 DNS 白名单；custom 源必须非空
    priority integer NOT NULL DEFAULT 100 CHECK (priority BETWEEN 0 AND 1000),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','disabled')),
    last_synced_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name),
    CHECK (kind <> 'custom' OR COALESCE(array_length(domain_allowlist, 1), 0) >= 1)
);
CREATE INDEX image_sources_priority ON image_sources(state, priority) WHERE tenant_id IS NULL;

-- 远端镜像缓存：同步任务扫描远端目录后 upsert（SHA256 来自源的 CHECKSUM 文件）。
CREATE TABLE remote_images (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id uuid NOT NULL REFERENCES image_sources(id),
    name varchar(128) NOT NULL,                      -- 规范名：ubuntu-22.04-lts / win10-virtio 等
    version varchar(64) NOT NULL,
    arch text NOT NULL CHECK (arch IN ('x86_64','aarch64')),
    format text NOT NULL CHECK (format IN ('qcow2','raw','iso','vhd')),
    download_url text NOT NULL CHECK (download_url ~ '^https://[A-Za-z0-9.:\[\]-]+/[A-Za-z0-9/_.%+~-]+$'),
    sha256 text CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes bigint CHECK (size_bytes > 0),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_id, name, version, arch, format)
);
CREATE INDEX remote_images_name ON remote_images(name, arch);

-- 本地镜像库：Agent 按 remote_images 下载/转换/注入后的产物。
-- pipeline_state: pending → downloading → verifying → converting → injecting → available / failed / retired
CREATE TABLE local_images (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid REFERENCES tenants(id),           -- NULL = 平台公共镜像
    source_id uuid NOT NULL REFERENCES image_sources(id),
    remote_image_id uuid NOT NULL REFERENCES remote_images(id),
    name varchar(128) NOT NULL,
    os_type text NOT NULL CHECK (os_type IN ('linux','windows')),
    storage_ref text CHECK (storage_ref ~ '^images/[0-9a-f-]{36}\.qcow2$'), -- Agent 回报；相对池根目录
    size_bytes bigint CHECK (size_bytes > 0),
    observed_sha256 text CHECK (observed_sha256 ~ '^[0-9a-f]{64}$'),        -- Agent 下载后回传
    pull_state text NOT NULL DEFAULT 'pending' CHECK (pull_state IN
        ('pending','downloading','verifying','converting','injecting','available','failed','retired')),
    error_code varchar(64),
    desired_version bigint NOT NULL DEFAULT 1,
    observed_version bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, source_id, remote_image_id),
    CHECK (observed_version BETWEEN 0 AND desired_version)
);
CREATE UNIQUE INDEX local_images_active_name ON local_images(name, COALESCE(tenant_id,'00000000-0000-0000-0000-000000000000'::uuid))
    WHERE pull_state NOT IN ('failed','retired');
CREATE INDEX local_images_poll ON local_images(updated_at) WHERE pull_state NOT IN ('available','retired');

-- VM 追溯：创建时使用的镜像源规范名。
ALTER TABLE vms ADD COLUMN image_source_name varchar(128);

-- 任务种类扩展：镜像按需拉取走任务表 + outbox（与 VM 任务同一套幂等/租约模型）。
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check CHECK (kind IN ('vm.create','vm.bandwidth','vm.nat',
    'vm.security','vm.resize','vm.adopt','vm.scan','vm.migrate','vm.recycle','vm.restore','vm.purge',
    'volume.snapshot','image.pull','image.source.sync'));

-- ---------- 角色与最小授权 ----------
-- 镜像 worker：唯一可写 storage_ref/observed_sha256 的角色（HTTP 角色禁写，防伪造本地路径）。
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kvmcloud_core_image_worker') THEN
        CREATE ROLE kvmcloud_core_image_worker NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
    END IF;
END
$$;
GRANT USAGE ON SCHEMA kvmcloud_core TO kvmcloud_core_image_worker;
GRANT SELECT, INSERT, UPDATE (name, priority, state)
    ON kvmcloud_core.image_sources TO kvmcloud_core_api;
GRANT SELECT ON kvmcloud_core.remote_images TO kvmcloud_core_api;
GRANT SELECT, INSERT (id, tenant_id, source_id, remote_image_id, name, os_type, pull_state),
    UPDATE (pull_state, error_code, desired_version)
    ON kvmcloud_core.local_images TO kvmcloud_core_api;
GRANT SELECT, UPDATE (storage_ref, size_bytes, observed_sha256, observed_version)
    ON kvmcloud_core.local_images TO kvmcloud_core_image_worker;

-- ---------- RLS（租户隔离；公共源/公共镜像 tenant_id 为 NULL，所有租户可读） ----------
ALTER TABLE image_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE image_sources FORCE ROW LEVEL SECURITY;
CREATE POLICY image_sources_visible ON image_sources FOR SELECT USING (
    tenant_id IS NULL OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
);
CREATE POLICY image_sources_manage ON image_sources FOR ALL USING (
    tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
) WITH CHECK (
    tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
);

-- remote_images 跟随其源：源可见即可见（ tenant 列在源上，用 EXISTS 关联）。
ALTER TABLE remote_images ENABLE ROW LEVEL SECURITY;
ALTER TABLE remote_images FORCE ROW LEVEL SECURITY;
CREATE POLICY remote_images_visible ON remote_images FOR SELECT USING (
    EXISTS (SELECT 1 FROM image_sources s WHERE s.id = source_id
        AND (s.tenant_id IS NULL OR s.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid))
);

ALTER TABLE local_images ENABLE ROW LEVEL SECURITY;
ALTER TABLE local_images FORCE ROW LEVEL SECURITY;
CREATE POLICY local_images_visible ON local_images FOR SELECT USING (
    tenant_id IS NULL OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
);
CREATE POLICY local_images_write ON local_images FOR ALL USING (
    tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
) WITH CHECK (
    tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
);

COMMIT;
