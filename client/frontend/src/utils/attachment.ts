// 附件展示与发送判定的纯函数（#41）：从 ChatPanel 抽出便于 vitest 覆盖。
import type { AttachmentView } from '../api/types'

// formatFileSize 附件大小的人类可读文本：B 取整，其余保留一位小数（如 1.5 MB）。
export function formatFileSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  let value = bytes
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i++
  }
  return i === 0 ? `${Math.round(value)} B` : `${value.toFixed(1)} ${units[i]}`
}

// isImageAttachment 图片按服务端嗅探的 content_type 判定；缩略图存在也视为图片
// （缩略图只对图片生成，兼容个别类型字段缺失的历史数据）。
export function isImageAttachment(a: Pick<AttachmentView, 'content_type' | 'thumbnail_url'>): boolean {
  return (a.content_type || '').toLowerCase().startsWith('image/') || !!a.thumbnail_url
}

// canSendMessage 正文非空或已选附件即可发送（纯附件消息 content 允许空串）。
export function canSendMessage(text: string, attachmentCount: number): boolean {
  return text.trim() !== '' || attachmentCount > 0
}
