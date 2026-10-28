<template>
  <h1>仪表盘</h1>
  <div class="cards">
    <div class="card"><div class="label">在线节点</div><div class="value">{{ online }}<span class="muted">/{{ total }}</span></div><div class="sub">status=online</div></div>
    <div class="card"><div class="label">虚拟机</div><div class="value">{{ vms.length }}</div><div class="sub">全集群</div></div>
    <div class="card"><div class="label">共享带宽池</div><div class="value">{{ pools.length }}</div><div class="sub">容量 {{ fmtKbps(totalPoolCap) }}</div></div>
    <div class="card"><div class="label">水位告警</div><div class="value">{{ alerts.length }}</div><div class="sub">阈值 90%</div></div>
  </div>

  <h1>节点四维水位</h1>
  <table v-if="nodes.length">
    <thead><tr><th>节点</th><th>vCPU</th><th>内存</th><th>磁盘</th><th>带宽</th></tr></thead>
    <tbody>
      <tr v-for="n in nodes" :key="n.id">
        <td>{{ n.hostname }} <span class="tag" :class="n.status === 'online' ? 'ok' : 'bad'">{{ n.status }}</span></td>
        <td><div class="wm-bar" :class="wmClass(n.watermarks.cpu)"><i :style="{ width: bar(n.watermarks.cpu) }"></i></div><span class="muted">{{ pct(n.watermarks.cpu) }}</span></td>
        <td><div class="wm-bar" :class="wmClass(n.watermarks.mem)"><i :style="{ width: bar(n.watermarks.mem) }"></i></div><span class="muted">{{ pct(n.watermarks.mem) }}</span></td>
        <td><div class="wm-bar" :class="wmClass(n.watermarks.disk)"><i :style="{ width: bar(n.watermarks.disk) }"></i></div><span class="muted">{{ pct(n.watermarks.disk) }}</span></td>
        <td><div class="wm-bar" :class="wmClass(n.watermarks.bw)"><i :style="{ width: bar(n.watermarks.bw) }"></i></div><span class="muted">{{ pct(n.watermarks.bw) }}</span></td>
      </tr>
    </tbody>
  </table>
  <p v-else class="muted">暂无节点：先运行 install.sh 并接入 Agent。</p>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { api, wmClass, pct, fmtKbps } from '../api.js'

const nodes = ref([]), vms = ref([]), pools = ref([]), alerts = ref([])
const online = computed(() => nodes.value.filter(n => n.status === 'online').length)
const total = computed(() => nodes.value.length)
const totalPoolCap = computed(() => pools.value.reduce((s, p) => s + p.total_capacity_kbps, 0))
const bar = (v) => Math.min(v * 100, 100) + '%'

onMounted(async () => {
  try {
    const [ns, vs, ps, al] = await Promise.all([
      api.get('/api/v1/nodes'), api.get('/api/v1/vms'),
      api.get('/api/v1/pools'), api.get('/api/v1/alerts'),
    ])
    nodes.value = ns || []; vms.value = vs || []; pools.value = ps || []; alerts.value = al || []
  } catch (e) { console.error(e) }
})
</script>
