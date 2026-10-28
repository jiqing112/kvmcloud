<template>
  <h1>超开水位告警</h1>
  <table v-if="alerts.length">
    <thead><tr><th>时间</th><th>节点</th><th>维度</th><th>水位</th><th>已分配 / 限额</th><th>级别</th></tr></thead>
    <tbody>
      <tr v-for="a in alerts" :key="a.id">
        <td class="muted">{{ new Date(a.created_at).toLocaleString() }}</td>
        <td class="mono">{{ short(a.node_id) }}</td>
        <td>{{ a.dimension }}</td>
        <td><span class="tag" :class="a.ratio >= 0.95 ? 'bad' : 'warn'">{{ pct(a.ratio) }}</span></td>
        <td class="mono">{{ a.allocated }} / {{ a.physical }}</td>
        <td><span class="tag" :class="a.severity === 'critical' ? 'bad' : 'warn'">{{ a.severity }}</span></td>
      </tr>
    </tbody>
  </table>
  <p v-else class="muted">当前无告警（任一维水位 ≥90% 时产生记录，10 分钟冷却）。</p>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { api, pct } from '../api.js'

const alerts = ref([])
const short = (id) => (id || '').slice(0, 8)
onMounted(async () => { alerts.value = await api.get('/api/v1/alerts') || [] })
</script>
