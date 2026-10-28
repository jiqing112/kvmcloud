<template>
  <h1>端口带宽策略</h1>
  <table v-if="policies.length">
    <thead><tr>
      <th>策略</th><th>VNIC</th><th>方向</th><th>保障 (min)</th><th>上限 (max)</th><th>当前 ceil</th><th>优先级</th><th>状态</th><th>共享池</th><th>操作</th>
    </tr></thead>
    <tbody>
      <tr v-for="p in policies" :key="p.id">
        <td class="mono">{{ short(p.id) }}</td>
        <td class="mono">{{ short(p.vnic_id) }}</td>
        <td>{{ p.direction }}</td>
        <td class="mono">{{ fmtKbps(p.min_kbps) }}</td>
        <td class="mono">{{ fmtKbps(p.max_kbps) }}</td>
        <td class="mono" :class="p.current_ceil_kbps > p.min_kbps ? '' : 'muted'">{{ fmtKbps(p.current_ceil_kbps ?? p.min_kbps) }}</td>
        <td class="mono">{{ p.priority }}</td>
        <td><span class="tag" :class="p.state === 'borrowing' ? 'warn' : 'ok'">{{ p.state }}</span></td>
        <td class="mono">{{ p.shared_pool_id ? short(p.shared_pool_id) : '—' }}</td>
        <td>
          <button class="btn ghost" @click="borrow(p)">借用</button>
          <button class="btn ghost" @click="release(p)" :disabled="p.state !== 'borrowing'">回收</button>
        </td>
      </tr>
    </tbody>
  </table>
  <p v-else class="muted">暂无策略：放置 VM 时带 min_kbps/shared_pool_id 自动创建。</p>

  <div v-if="borrowing" class="card" style="margin-top:20px; max-width:480px;">
    <div class="label">借用请求 — {{ short(borrowing.id) }}（池余量不足时自动抢占低优先级）</div>
    <div class="form-row">
      <input v-model.number="borrowKbps" type="number" min="1" placeholder="请求 Kbps" />
      <button class="btn" :disabled="busy" @click="doBorrow">提交</button>
    </div>
    <p class="err" v-if="error">{{ error }}</p>
  </div>

  <h1 style="margin-top:32px;">借用/抢占事件</h1>
  <table v-if="events.length">
    <thead><tr><th>时间</th><th>策略</th><th>请求</th><th>授予</th><th>结果</th></tr></thead>
    <tbody>
      <tr v-for="e in events" :key="e.id">
        <td class="muted">{{ new Date(e.created_at).toLocaleString() }}</td>
        <td class="mono">{{ short(e.policy_id) }}</td>
        <td class="mono">{{ fmtKbps(e.requested_kbps) }}</td>
        <td class="mono">{{ fmtKbps(e.granted_kbps) }}</td>
        <td><span class="tag" :class="e.reason === 'granted' ? 'ok' : e.reason === 'preempted' ? 'warn' : 'bad'">{{ e.reason }}</span></td>
      </tr>
    </tbody>
  </table>
  <p v-else class="muted">暂无事件。</p>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { api, fmtKbps } from '../api.js'

const policies = ref([]), events = ref([])
const borrowing = ref(null), borrowKbps = ref(0), busy = ref(false), error = ref('')

const short = (id) => (id || '').slice(0, 8)

async function load() {
  const [ps, es] = await Promise.all([api.get('/api/v1/policies'), api.get('/api/v1/events')])
  policies.value = ps || []; events.value = es || []
}
function borrow(p) { borrowing.value = p; borrowKbps.value = p.max_kbps - p.min_kbps; error.value = '' }
async function doBorrow() {
  busy.value = true; error.value = ''
  try {
    await api.post(`/api/v1/policies/${borrowing.value.id}/borrow`, { requested_kbps: borrowKbps.value })
    borrowing.value = null
    await load()
  } catch (e) {
    error.value = e.data && e.data.code === 'insufficient_bandwidth' ? `抢占后仍不足：${e.message}` : e.message
  } finally { busy.value = false }
}
async function release(p) {
  try { await api.post(`/api/v1/policies/${p.id}/release`); await load() } catch (e) { error.value = e.message }
}
onMounted(load)
</script>
