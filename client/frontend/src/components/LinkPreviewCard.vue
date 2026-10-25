<script setup lang="ts">
// 链接预览卡片（#42 B4）：从消息正文提取首个链接，服务端抓取后展示站点/标题/描述/图片。
// 进入视口才请求（IntersectionObserver，长列表不一次打满服务端），无预览/加载中不占位。
// 整卡点击交给系统浏览器（OpenAttachment 只放行 http/https）；所有字段文本插值，无 v-html。
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { goApp } from '../api/bridge'
import { useNotifyStore } from '../stores/notify'
import { usePreviewStore } from '../stores/previews'
import { extractFirstURL } from '../utils/link'

const props = defineProps<{ content: string }>()

const previews = usePreviewStore()
const notify = useNotifyStore()

// root 是观察锚点：有 URL 就渲染（卡片为空时高度为 0，不占位）。
const root = ref<HTMLElement | null>(null)
const imageFailed = ref(false)

const url = computed(() => extractFirstURL(props.content))
const preview = computed(() => (url.value ? previews.get(url.value) : undefined))
const card = computed(() => (preview.value && preview.value !== 'none' ? preview.value : null))

let observer: IntersectionObserver | null = null

function disconnect() {
  if (observer) {
    observer.disconnect()
    observer = null
  }
}

function ensure() {
  if (url.value) previews.ensure(url.value)
}

// setupObserver 绑定当前 URL 的视口观察。已缓存（含负缓存）无需再观察；
// 环境没有 IntersectionObserver 时退回立即加载。
function setupObserver() {
  disconnect()
  imageFailed.value = false
  if (!url.value || previews.get(url.value) !== undefined) return
  if (typeof IntersectionObserver === 'undefined') {
    ensure()
    return
  }
  observer = new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting)) {
      disconnect()
      ensure()
    }
  }, { rootMargin: '120px 0px' })
  if (root.value) observer.observe(root.value)
}

onMounted(setupObserver)
onBeforeUnmount(disconnect)
// 消息被编辑后 content 变化：按新 URL 重新判断（旧观察必须解绑）。
watch(url, () => setupObserver())

function hostOf(raw: string): string {
  try {
    return new URL(raw).hostname
  } catch {
    return ''
  }
}

function openPreview(raw: string) {
  goApp().OpenAttachment(raw).catch((e) => notify.fromErr(e, '打开链接失败'))
}
</script>

<template>
  <div v-if="url" ref="root" class="lp-root">
    <button v-if="card" type="button" class="lp-card" :title="card.title"
            @click="openPreview(card.url)">
      <span class="lp-body">
        <span class="lp-site">{{ card.site_name || hostOf(card.url) }}</span>
        <span class="lp-title">{{ card.title }}</span>
        <span v-if="card.description" class="lp-desc">{{ card.description }}</span>
      </span>
      <img v-if="card.image_url && !imageFailed" class="lp-img" :src="card.image_url"
           alt="" loading="lazy" @error="imageFailed = true" />
    </button>
  </div>
</template>

<style scoped>
.lp-root {
  display: block;
  /* 零面积元素在部分浏览器不会触发 isIntersecting；1px 高度保证观察锚点可用，
     卡片未出时不渲染内容，视觉上不占位。 */
  min-height: 1px;
}
.lp-card {
  display: flex; align-items: stretch; gap: 10px;
  width: 100%; max-width: 420px;
  margin-top: 6px; padding: 8px 10px;
  text-align: left;
  background: var(--surface);
  border: 1px solid var(--line);
  border-radius: 8px;
}
.lp-card:hover { background: var(--brand-50); }
.lp-body {
  display: flex; flex-direction: column; gap: 2px;
  min-width: 0; flex: 1;
}
.lp-site {
  font-size: 11px; color: var(--ink-400);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.lp-title {
  font-size: 13px; font-weight: 600; color: var(--ink-900);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.lp-desc {
  font-size: 12px; line-height: 1.45; color: var(--ink-600);
  word-break: break-word;
  display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical;
  overflow: hidden;
}
.lp-img {
  width: 64px; height: 64px; flex: none;
  object-fit: cover; border-radius: 6px;
}
</style>
