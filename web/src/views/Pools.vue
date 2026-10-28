<template>
  <h1>共享带宽池</h1>
  <div class="card" style="max-width:640px;">
    <div class="label">新建共享池</div>
    <div class="form-row">
      <input v-model="form.tenant_name" placeholder="租户" style="width:110px" />
      <input v-model="form.name" placeholder="池名称" style="width:150px" />
      <input v-model.number="form.total_capacity_kbps" type="number" min="1" placeholder="容量 Kbps" style="width:140px" />
      <button class="btn" :disabled="creating" @click="create">创建</button>
    </div>
    <p class="err" v-if="error">{{ error }}</p>
  </div>

  <table v-if="pools.length" style="margin-top:20px;">
    <thead><tr><th>名称</th><th>租户</th><th>总容量</th><th>保障已分配 (Σmin)</th><th>当前借用</th><th>可借余量</th></tr></thead>
    <tbody>
      <tr v-for="p in pools" :key="p.id">
        <td>{{ p.name }}</td>
        <td class="mono">{{ short(p.tenant_id) }}</td>
        <td class="mono">{{ fmtKbps(p.total_capacity_kbps) }}</td>
        <td class="mono">{{ fmtKbps(p.allocated_min_kbps) }}</td>
        <td class="mono">{{ fmtKbps(p.current_borrowed_kbps) }}</td>
        <td class="mono">{{ fmtKbps(avail(p)) }}</td>
      </tr>
    </tbody>
  </table>
  <p v-else class="muted">暂无共享池。</p>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { api, fmtKbps } from '../api.js'

const pools = ref([]), creating = ref(false), error = ref('')
const form = ref({ tenant_name: 'default', name: '', total_capacity_kbps: 100000 })

const short = (id) => (id || '').slice(0, 8)
const avail = (p) => p.total_capacity_kbps - p.allocated_min_kbps - p.current_borrowed_kbps

async function load() { pools.value = await api.get('/api/v1/pools') || [] }
async function create() {
  creating.value = true; error.value = ''
  try {
    await api.post('/api/v1/pools', form.value)
    form.value.name = ''
    await load()
  } catch (e) { error.value = e.message } finally { creating.value = false }
}
onMounted(load)
</script>
