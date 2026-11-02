// 只管登录壳左侧装饰波形；初始不读 hasFocus，原生窗口首次挂载时焦点可能尚未建立。
export function bindAuthWaveMotion(
  win: EventTarget,
  doc: EventTarget & { readonly hidden: boolean },
  setPaused: (paused: boolean) => void,
): () => void {
  let blurred = false
  let paused: boolean | null = null
  let disposed = false

  const update = () => {
    if (disposed) return
    const next = blurred || doc.hidden
    if (next === paused) return
    paused = next
    setPaused(next)
  }
  const onBlur = () => { blurred = true; update() }
  const onFocus = () => { blurred = false; update() }

  win.addEventListener('blur', onBlur)
  win.addEventListener('focus', onFocus)
  doc.addEventListener('visibilitychange', update)
  update()

  return () => {
    if (disposed) return
    disposed = true
    win.removeEventListener('blur', onBlur)
    win.removeEventListener('focus', onFocus)
    doc.removeEventListener('visibilitychange', update)
  }
}
