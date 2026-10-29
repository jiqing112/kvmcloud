<template>
  <h1>虚拟机</h1>
  <div class="card" style="max-width:860px;">
    <div class="label">放置新 VM（四维超开校验）</div>
    <div class="form-row">
      <input v-model="form.tenant_name" placeholder="租户" style="width:110px" />
      <input v-model="form.name" placeholder="名称" style="width:130px" />
      <input v-model.number="form.vcpus" type="number" min="1" placeholder="vCPU" style="width:80px" />
      <input v-model.number="form.mem_mb" type="number" min="1" placeholder="内存MB" style="width:100px" />
      <input v-model.number="form.disk_gb" type="number" min="1" placeholder="磁盘GB" style="width:100px" />
      <input v-model.number="form.min_kbps" type="number" min="0" placeholder="保障Kbps" style="width:110px" />
      <input v-model.number="form.max_kbps" type="number" min="0" placeholder="上限Kbps" style="width:110px" />
      <select v-model="form.shared_pool_id" style="width:170px">
        <option value="">不入共享池</option>
        <option v-for="p in pools" :key="p.id" :value="p.id">{{ p.name }}</option>
      </select>
      <button class="btn" :disabled="placing" @click="place">放置</button>
    </div>
    <p class="err" v-if="error">{{ error }}</p>
  </div>

  <table v-if="vms.length" style="margin-top:20px;">
    <thead><tr><th>名称</th><th>规格</th><th>节点</th><th>状态</th><th>创建时间</th></tr></thead>
    <tbody>
      <tr v-for="v in vms" :key="v.id">
        <td>{{ v.name }}</td>
        <td class="mono">{{ v.vcpus }}c / {{ (v.mem_mb/1024).toFixed(0) }}G / {{ v.disk_gb }}G</td>
        <td class="mono">{{ short(v.node_id) }}</td>
        <td>
          <span class="tag" :class="statusClass(v.status)" :title="v.error || ''">{{ v.status }}</span>
          <div class="muted" v-if="v.error" style="max-width:260px;white-space:normal;">{{ v.error }}</div>
        </td>
        <td class="muted">{{ new Date(v.created_at).toLocaleString() }}</td>
      </tr>
    </tbody>
  </table>
  <p v-else class="muted">暂无 VM。</p>
</template>

<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import { api } from '../api.js'

const vms = ref([]), pools = ref([])
const placing = ref(false), error = ref('')
const form = ref({ tenant_name: 'default', name: '', vcpus: 2, mem_mb: 4096, disk_gb: 40, min_kbps: 0, max_kbps: 0, shared_pool_id: '' })

const short = (id) => (id || '').slice(0, 8)
const statusClass = (s) => (s === 'running' ? 'ok' : s === 'creating' ? 'warn' : s === 'error' ? 'bad' : '')

let timer = null
async function load() {
  const [vs, ps] = await Promise.all([api.get('/api/v1/vms'), api.get('/api/v1/pools')])
  vms.value = vs || []; pools.value = ps || []
}
async function place() {
  placing.value = true; error.value = ''
  try {
    await api.post('/api/v1/vms', form.value)
    form.value.name = ''
    await load()
  } catch (e) {
    error.value = e.data && e.data.code === 'overcommit_exceeded' ? `超开超限：${e.message}` : e.message
  } finally { placing.value = false }
}
onMounted(() => {
  load()
  timer = setInterval(load, 5000) // 建机/变配状态轮询
})
onUnmounted(() => { if (timer) clearInterval(timer) })
</script>
