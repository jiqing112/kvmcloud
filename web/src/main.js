import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import Dashboard from './views/Dashboard.vue'
import Nodes from './views/Nodes.vue'
import VMs from './views/VMs.vue'
import Bandwidth from './views/Bandwidth.vue'
import Pools from './views/Pools.vue'
import Alerts from './views/Alerts.vue'
import './style.css'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: Dashboard },
    { path: '/nodes', component: Nodes },
    { path: '/vms', component: VMs },
    { path: '/bandwidth', component: Bandwidth },
    { path: '/pools', component: Pools },
    { path: '/alerts', component: Alerts },
  ],
})

createApp(App).use(router).mount('#app')
