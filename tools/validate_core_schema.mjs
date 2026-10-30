// Optional validation runtime: @electric-sql/pglite, outside app dependencies.
// node tools/validate_core_schema.mjs <path-to-pglite-dist-index.js>
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const modulePath = process.argv[2];
if (!modulePath) throw new Error('Pass the path to the PGlite module');
const { PGlite } = await import(pathToFileURL(resolve(modulePath)).href);
const db = new PGlite();
const tenantA = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const tenantB = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const actor = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
const cluster = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd';
const node = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee';
const image = 'ffffffff-ffff-4fff-8fff-ffffffffffff';
const networkA = '11111111-1111-4111-8111-111111111111';
const networkB = '22222222-2222-4222-8222-222222222222';
const vmA = '33333333-3333-4333-8333-333333333333';
const vmB = '44444444-4444-4444-8444-444444444444';
let passed = 0;

async function check(name, fn) {
  await fn();
  passed++;
  console.log(`PASS ${name}`);
}

async function asTenant(tenant, fn) {
  await db.exec('BEGIN; SET LOCAL ROLE kvmcloud_core_api;');
  try {
    if (tenant) await db.query("SELECT set_config('app.tenant_id', $1, true)", [tenant]);
    return await fn();
  } finally {
    await db.exec('ROLLBACK;');
  }
}

async function reject(sql, code) {
  await assert.rejects(db.exec(sql), error => error.code === code);
}

try {
  await check('baseline and least-privilege roles apply', async () => {
    await db.exec(readFileSync('deploy/sql/core/001_schema.sql', 'utf8'));
    await db.exec(readFileSync('deploy/sql/core/002_roles.sql', 'utf8'));
    await db.exec(readFileSync('deploy/sql/core/003_images.sql', 'utf8'));
  });
  await db.exec(`
    INSERT INTO kvmcloud_core.tenants(id,name) VALUES ('${tenantA}','a'),('${tenantB}','b');
    INSERT INTO kvmcloud_core.principals(id,subject) VALUES ('${actor}','oidc:test');
    INSERT INTO kvmcloud_core.clusters(id,name,region) VALUES ('${cluster}','test','test');
    INSERT INTO kvmcloud_core.networks(id,tenant_id,cluster_id,name)
      VALUES ('${networkA}','${tenantA}','${cluster}','a'),('${networkB}','${tenantB}','${cluster}','b');
    INSERT INTO kvmcloud_core.images(id,name,artifact_sha256,storage_ref,state,min_disk_gib)
      VALUES ('${image}','test',decode(repeat('00',32),'hex'),'image:test','ready',1);
    INSERT INTO kvmcloud_core.nodes(id,cluster_id,hostname,agent_spki_sha256,state,schedulable,
      physical_vcpus,physical_memory_mib,physical_disk_gib,physical_nic_kbps)
      VALUES ('${node}','${cluster}','test',decode(repeat('01',32),'hex'),'online',true,8,16384,500,1000000);
    INSERT INTO kvmcloud_core.tenant_quotas(tenant_id,max_vms,max_vcpus,max_memory_mib,max_disk_gib,max_min_kbps)
      VALUES ('${tenantA}',10,32,65536,1000,1000000),('${tenantB}',10,32,65536,1000,1000000);
    INSERT INTO kvmcloud_core.vms(id,tenant_id,node_id,image_id,network_id,name,vcpus,memory_mib,root_disk_gib)
      VALUES ('${vmA}','${tenantA}','${node}','${image}','${networkA}','a',1,128,1),
             ('${vmB}','${tenantB}','${node}','${image}','${networkB}','b',1,128,1);
  `);
  await check('RLS exposes only the active tenant', async () => {
    await asTenant(tenantA, async () => {
      const result = await db.query('SELECT id FROM kvmcloud_core.vms');
      assert.deepEqual(result.rows, [{ id: vmA }]);
    });
    await asTenant(null, async () => {
      assert.equal((await db.query('SELECT id FROM kvmcloud_core.vms')).rows.length, 0);
    });
  });
  await check('public images are readable; tenant network metadata can be locked', async () => {
    await asTenant(tenantA, async () => {
      assert.equal((await db.query('SELECT id FROM kvmcloud_core.images')).rows.length, 1);
      assert.equal((await db.query('SELECT id FROM kvmcloud_core.networks FOR SHARE')).rows.length, 1);
      assert.equal((await db.query('SELECT id FROM kvmcloud_core.tenants FOR SHARE')).rows.length, 1);
    });
  });
  await check('HTTP role can atomically reserve four dimensions', async () => {
    await asTenant(tenantA, async () => {
      const result = await db.query(`UPDATE kvmcloud_core.nodes SET
        used_vcpus=used_vcpus+1, used_memory_mib=used_memory_mib+128,
        used_disk_gib=used_disk_gib+1, used_min_kbps=used_min_kbps+10
        WHERE id=$1 AND used_vcpus::numeric+1 <= floor(physical_vcpus::numeric*cpu_ratio)
        RETURNING used_vcpus,used_memory_mib,used_disk_gib,used_min_kbps`, [node]);
      assert.equal(result.rows[0].used_vcpus, 1);
      assert.equal(result.rows[0].used_memory_mib, 128);
      assert.equal(result.rows[0].used_disk_gib, 1);
      assert.equal(result.rows[0].used_min_kbps, 10);
    });
    const result = await db.query('SELECT used_vcpus FROM kvmcloud_core.nodes WHERE id=$1', [node]);
    assert.equal(result.rows[0].used_vcpus, 0);
  });
  await check('node over-allocation and negative quotas are rejected', async () => {
    await reject(`UPDATE kvmcloud_core.nodes SET used_vcpus=33 WHERE id='${node}'`, '23514');
    await reject(`UPDATE kvmcloud_core.tenant_quotas SET used_disk_gib=-1 WHERE tenant_id='${tenantA}'`, '23514');
  });
  await check('composite foreign key rejects cross-tenant network reference', async () => {
    await reject(`INSERT INTO kvmcloud_core.vms(id,tenant_id,node_id,network_id,name,vcpus,memory_mib,root_disk_gib)
      VALUES (gen_random_uuid(),'${tenantA}','${node}','${networkB}','cross',1,128,1)`, '23503');
  });
  await check('TCP rules cannot exploit SQL NULL checks', async () => {
    await db.exec(`INSERT INTO kvmcloud_core.security_groups(id,tenant_id,name)
      VALUES ('55555555-5555-4555-8555-555555555555','${tenantA}','default')`);
    await reject(`INSERT INTO kvmcloud_core.security_group_rules(id,tenant_id,group_id,direction,ethertype,protocol,remote_cidr)
      VALUES (gen_random_uuid(),'${tenantA}','55555555-5555-4555-8555-555555555555','ingress',4,'tcp','0.0.0.0/0')`, '23514');
  });
  await check('only one unresolved mutation per VM', async () => {
    await db.exec(`INSERT INTO kvmcloud_core.tasks(id,tenant_id,vm_id,node_id,kind,state)
      VALUES (gen_random_uuid(),'${tenantA}','${vmA}','${node}','vm.create','unknown')`);
    await reject(`INSERT INTO kvmcloud_core.tasks(id,tenant_id,vm_id,node_id,kind)
      VALUES (gen_random_uuid(),'${tenantA}','${vmA}','${node}','vm.resize')`, '23505');
  });
  await check('audit rows cannot be rewritten or truncated', async () => {
    await asTenant(tenantA, async () => {
      await db.query(`INSERT INTO kvmcloud_core.audit_logs
        (id,tenant_id,actor_id,request_id,action,resource_type,resource_id,outcome)
        VALUES (gen_random_uuid(),$1,$2,gen_random_uuid(),'test','vm',$3,'accepted')`, [tenantA, actor, vmA]);
      assert.equal((await db.query('SELECT id FROM kvmcloud_core.audit_logs')).rows.length, 1);
    });
    await reject('UPDATE kvmcloud_core.audit_logs SET action=action', '42501');
    await reject('TRUNCATE kvmcloud_core.audit_logs', '42501');
  });
  await check('HTTP role cannot alter node state or schema', async () => {
    await assert.rejects(asTenant(tenantA, () => db.exec(`UPDATE kvmcloud_core.nodes SET state='online'`)),
      error => error.code === '42501');
    await assert.rejects(asTenant(tenantA, () => db.exec('ALTER TABLE kvmcloud_core.audit_logs DISABLE TRIGGER ALL')),
      error => error.code === '42501');
  });
  await check('enrollment role can append only platform enrollment audit', async () => {
    await db.exec('BEGIN; SET LOCAL ROLE kvmcloud_core_enrollment;');
    try {
      await db.query(`INSERT INTO kvmcloud_core.audit_logs
        (id,actor_id,request_id,action,resource_type,resource_id,outcome)
        VALUES (gen_random_uuid(),$1,gen_random_uuid(),'nodes.install-token','node',$2,'accepted')`, [actor, node]);
    } finally {
      await db.exec('ROLLBACK;');
    }
  });
  // --- 镜像模块（003_images.sql）---
  const platformSource = '66666666-6666-4666-8666-666666666666';
  const remoteA = '77777777-7777-4777-8777-777777777777';
  const localA = '88888888-8888-4888-8888-888888888888';
  await check('image module tables apply with allowlist + version constraints', async () => {
    await db.exec(`INSERT INTO kvmcloud_core.image_sources(id,name,kind,base_url,domain_allowlist,egress_bytes_limit)
      VALUES ('${platformSource}','mirrors','domestic','https://mirrors.example.com/',
      ARRAY['mirrors.example.com'],68719476736)`);
    await reject(`INSERT INTO kvmcloud_core.image_sources(id,tenant_id,name,kind,base_url,domain_allowlist,egress_bytes_limit)
      VALUES (gen_random_uuid(),'${tenantA}','bad','custom','https://c.example.com/','{}',68719476736)`,
      '23514');
    await db.exec(`INSERT INTO kvmcloud_core.remote_images(id,source_id,name,version,arch,format,download_url)
      VALUES ('${remoteA}','${platformSource}','ubuntu-24.04-lts','v1','x86_64','qcow2',
      'https://mirrors.example.com/ubuntu.qcow2')`);
  });
  await check('tenant sees platform source but cannot modify it', async () => {
    await asTenant(tenantA, async () => {
      assert.equal((await db.query('SELECT id FROM kvmcloud_core.image_sources')).rows.length, 1);
      // RLS 对 UPDATE 静默过滤：平台级行对本租户 manage policy 不可见 → 0 行受影响（API 层呈现为 404）。
      const r = await db.query(`UPDATE kvmcloud_core.image_sources SET priority=1 WHERE id='${platformSource}'`);
      assert.equal(r.rowCount ?? r.affectedRows, 0);
    });
  });
  await check('local image RLS and HTTP role column grants', async () => {
    await asTenant(tenantA, async () => {
      await db.query(`INSERT INTO kvmcloud_core.local_images(id,tenant_id,source_id,remote_image_id,name,os_type)
        VALUES ('${localA}','${tenantA}','${platformSource}','${remoteA}','ubuntu-24.04-lts','linux')`);
      await db.query(`UPDATE kvmcloud_core.local_images SET pull_state='downloading' WHERE id='${localA}'`);
    });
    await asTenant(tenantB, async () => {
      assert.equal((await db.query('SELECT id FROM kvmcloud_core.local_images')).rows.length, 0);
    });
    // 被拒语句会中止事务，放到独立事务里验证（42501：HTTP 角色缺 storage_ref 列授权）。
    await asTenant(tenantA, async () => {
      await assert.rejects(() => db.query(
        `UPDATE kvmcloud_core.local_images SET storage_ref='images/x.qcow2' WHERE id='${localA}'`),
        error => error.code === '42501');
    });
  });
  await check('vms.image_source_name and extended task kinds accept image pipeline', async () => {
    await asTenant(tenantA, async () => {
      await db.query(`INSERT INTO kvmcloud_core.tasks(id,tenant_id,node_id,kind)
        VALUES (gen_random_uuid(),'${tenantA}','${node}','image.pull')`);
      // HTTP 角色走 INSERT 建 VM（含镜像源规范名）；校验替代路径：整行插入后按 RLS 读回。
      await db.query(`INSERT INTO kvmcloud_core.vms(id,tenant_id,node_id,image_id,image_source_name,
        domain_uuid,network_id,name,vcpus,memory_mib,root_disk_gib,state)
        VALUES (gen_random_uuid(),'${tenantA}','${node}','${image}','ubuntu-24.04-lts',
        gen_random_uuid(),'${networkA}','img-src-vm',1,512,10,'creating')`);
      const v = await db.query(`SELECT image_source_name FROM kvmcloud_core.vms WHERE name='img-src-vm'`);
      assert.equal(v.rows[0].image_source_name, 'ubuntu-24.04-lts');
    });
    await reject(`INSERT INTO kvmcloud_core.tasks(id,tenant_id,node_id,kind)
      VALUES (gen_random_uuid(),'${tenantA}','${node}','image.totally-fake')`, '23514');
  });
  console.log(`${passed} PostgreSQL schema checks passed (PGlite; not multi-connection concurrency tests).`);
} finally {
  await db.close();
}
