<template>
  <div class="layout">
    <aside class="sidebar">
      <div class="logo">kvmcloud</div>
      <nav>
        <RouterLink to="/">仪表盘</RouterLink>
        <RouterLink to="/nodes">节点</RouterLink>
        <RouterLink to="/vms">虚拟机</RouterLink>
        <RouterLink to="/bandwidth">带宽策略</RouterLink>
        <RouterLink to="/pools">共享池</RouterLink>
        <RouterLink to="/alerts">水位告警</RouterLink>
      </nav>
    </aside>
    <main class="main">
      <RouterView />
    </main>
  </div>
</template>
