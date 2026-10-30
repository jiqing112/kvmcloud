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
