<template>
  <h1>节点</h1>
  <table v-if="nodes.length">
    <thead><tr>
      <th>主机名</th><th>状态</th><th>物理 (vCPU/内存/磁盘/带宽)</th><th>已分配</th><th>超开比</th><th>四维水位</th><th>调优</th>
    </tr></thead>
    <tbody>
      <tr v-for="n in nodes" :key="n.id">
        <td class="mono">{{ n.hostname }}</td>
        <td><span class="tag" :class="n.status === 'online' ? 'ok' : 'bad'">{{ n.status }}</span></td>
        <td class="mono">{{ n.physical_vcpus }}c / {{ gb(n.physical_mem_mb) }}G / {{ n.physical_disk_gb }}G / {{ fmtKbps(n.physical_nic_bandwidth_kbps) }}</td>
        <td class="mono">{{ n.allocated_vcpus }}c / {{ gb(n.allocated_mem_mb) }}G / {{ n.allocated_disk_gb }}G / {{ fmtKbps(n.allocated_min_kbps) }}</td>
        <td class="mono">{{ n.cpu_overcommit_ratio }}/{{ n.mem_overcommit_ratio }}/{{ n.disk_overcommit_ratio }}/{{ n.bw_overcommit_ratio }}</td>
        <td><span class="mono">{{ pct(n.watermarks.cpu) }} · {{ pct(n.watermarks.mem) }} · {{ pct(n.watermarks.disk) }} · {{ pct(n.watermarks.bw) }}</span></td>
        <td><button class="btn ghost" @click="edit(n)">调优</button></td>
      </tr>
    </tbody>
  </table>
  <p v-else class="muted">加载中或无节点。</p>

  <div v-if="editing" class="card" style="margin-top:20px; max-width:560px;">
    <div class="label">超开比调优 — {{ editing.hostname }}</div>
    <div class="form-row">
      <input v-model.number="form.cpu" type="number" step="0.1" min="1" placeholder="CPU 比" />
      <input v-model.number="form.mem" type="number" step="0.1" min="1" placeholder="内存比" />
      <input v-model.number="form.disk" type="number" step="0.1" min="1" placeholder="磁盘比" />
      <input v-model.number="form.bw" type="number" step="0.1" min="1" placeholder="带宽比" />
      <button class="btn" :disabled="saving" @click="save">保存</button>
    </div>
    <p class="err" v-if="error">{{ error }}</p>
    <p class="muted">调整后立即影响新放置校验；已分配量不回溯。</p>
  </div>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { api, pct, fmtKbps } from '../api.js'

const nodes = ref([])
const editing = ref(null)
const form = ref({ cpu: 4, mem: 1.5, disk: 2, bw: 1 })
const saving = ref(false)
const error = ref('')

const gb = (mb) => (mb / 1024).toFixed(0)

async function load() {
  nodes.value = await api.get('/api/v1/nodes') || []
}
function edit(n) {
  editing.value = n
  form.value = { cpu: n.cpu_overcommit_ratio, mem: n.mem_overcommit_ratio, disk: n.disk_overcommit_ratio, bw: n.bw_overcommit_ratio }
}
async function save() {
  saving.value = true; error.value = ''
  try {
    await api.put(`/api/v1/nodes/${editing.value.id}/ratios`, {
      cpu_overcommit_ratio: form.value.cpu,
      mem_overcommit_ratio: form.value.mem,
      disk_overcommit_ratio: form.value.disk,
      bw_overcommit_ratio: form.value.bw,
    })
    editing.value = null
    await load()
  } catch (e) { error.value = e.message } finally { saving.value = false }
}
onMounted(load)
</script>
