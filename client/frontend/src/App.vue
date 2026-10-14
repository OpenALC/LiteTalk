<script setup lang="ts">
// App 根组件：路由出口 + 全局轻提示（notify store 驱动，覆盖全部页面）。
import GlobalToast from './components/GlobalToast.vue'
</script>

<template>
  <router-view />
  <GlobalToast />
</template>
