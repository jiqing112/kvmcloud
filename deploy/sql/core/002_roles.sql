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
