<script setup lang="ts">
// IntroView 介绍页（v0.1.2）：服务器栏顶部的「首页」，进入应用默认落在本页。
// 无频道栏（MainView 条件渲染）；内容：LiteTalk 简介、OpenALC 出品、仓库地址。
import { AudioWaveform, Github } from 'lucide-vue-next'

const REPO = 'https://github.com/OpenALC/LiteTalk'
</script>

<template>
  <div class="intro">
    <div class="brand-card">
      <div class="logo-mark"><AudioWaveform :size="30" /></div>
      <h1>轻音<small>LITETALK</small></h1>
      <p class="tag">轻量、清晰的桌面语音。打开频道，即刻开聊。</p>

      <ul class="points">
        <li>自研 Pion SFU 转发架构，服务器纯转发不混音，低延迟多人通话</li>
        <li>RNNoise 智能降噪 + Opus 语音编码，弱网带内纠错</li>
        <li>开源友好：Auth 模式开关、发信通道插拔、自托管一键部署</li>
      </ul>

      <div class="meta">
        <span class="by">OpenALC 开源项目 · MIT License</span>
        <span class="repo"><Github :size="13" />{{ REPO }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* 介绍页占满舞台区：品牌卡垂直居中，风格对齐 AuthLayout 品牌区 */
.intro {
  flex: 1; min-height: 0;
  display: flex; align-items: center; justify-content: center;
  padding: 32px; overflow-y: auto;
  background: linear-gradient(160deg, var(--brand-50), var(--surface) 70%);
}
.brand-card {
  max-width: 460px; width: 100%;
  background: var(--surface); border: 1px solid var(--line);
  border-radius: 18px; box-shadow: var(--shadow-2);
  padding: 34px 36px;
  display: flex; flex-direction: column; gap: 14px;
}
.logo-mark {
  width: 54px; height: 54px; border-radius: 15px;
  background: var(--brand-500);
  display: flex; align-items: center; justify-content: center;
  box-shadow: 0 6px 16px rgba(79, 164, 243, 0.35);
}
.logo-mark :deep(svg) { color: #fff; }
h1 { font-size: 22px; font-weight: 700; letter-spacing: 0.5px; }
h1 small {
  display: block; font-size: 11px; font-weight: 500;
  color: var(--ink-400); letter-spacing: 2.5px; margin-top: 2px;
}
.tag { font-size: 14px; color: var(--ink-600); line-height: 1.7; }
.points {
  display: flex; flex-direction: column; gap: 8px;
  padding: 14px 0; border-top: 1px solid var(--line);
  list-style: none;
}
.points li {
  font-size: 12.5px; color: var(--ink-600); line-height: 1.6;
  padding-left: 16px; position: relative;
}
.points li::before {
  content: ""; position: absolute; left: 2px; top: 7px;
  width: 5px; height: 5px; border-radius: 50%;
  background: var(--brand-500);
}
.meta {
  display: flex; flex-direction: column; gap: 6px;
  padding-top: 12px; border-top: 1px solid var(--line);
  font-size: 12px; color: var(--ink-400);
}
.repo { display: flex; align-items: center; gap: 6px; color: var(--brand-600); }
</style>
