<script setup lang="ts">
// TextChat 文字频道视图：消息列表与输入框的公共壳见 ChatPanel。
// 消息只渲染当前频道（store 按 channel_id 分桶），跨频道不串消息。
import { computed } from 'vue'
import { Hash } from 'lucide-vue-next'
import { useServersStore } from '../stores/data'
import ChatPanel from './ChatPanel.vue'

const servers = useServersStore()
const channelId = computed(() => servers.currentChannelId)
const title = computed(() => servers.currentChannel?.name ?? '闲聊广场')
</script>

<template>
  <ChatPanel :channel-id="channelId" :title="title" :placeholder="`发送到 #${title}`">
    <template #icon><Hash :size="17" /></template>
  </ChatPanel>
</template>
