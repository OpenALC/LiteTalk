import { describe, expect, it, vi } from 'vitest'
import { bindAuthWaveMotion } from './authWaveMotion'

class VisibilityTarget extends EventTarget {
  hidden = false
  setHidden(hidden: boolean) {
    this.hidden = hidden
    this.dispatchEvent(new Event('visibilitychange'))
  }
}

describe('认证页装饰波形可见性', () => {
  it('初始可见时保持动画，实际失焦后暂停，恢复焦点后继续', () => {
    const win = new EventTarget()
    const doc = new VisibilityTarget()
    const setPaused = vi.fn()
    const dispose = bindAuthWaveMotion(win, doc, setPaused)

    expect(setPaused.mock.calls).toEqual([[false]])
    win.dispatchEvent(new Event('blur'))
    win.dispatchEvent(new Event('blur'))
    expect(setPaused.mock.calls).toEqual([[false], [true]])
    win.dispatchEvent(new Event('focus'))
    expect(setPaused.mock.calls).toEqual([[false], [true], [false]])
    dispose()
  })

  it('初始隐藏与焦点、可见性事件交错时，两个暂停条件都清除才继续', () => {
    const win = new EventTarget()
    const doc = new VisibilityTarget()
    doc.hidden = true
    const setPaused = vi.fn()
    const dispose = bindAuthWaveMotion(win, doc, setPaused)

    expect(setPaused.mock.calls).toEqual([[true]])
    win.dispatchEvent(new Event('focus'))
    expect(setPaused).toHaveBeenCalledTimes(1)
    doc.setHidden(false)
    expect(setPaused.mock.calls).toEqual([[true], [false]])
    win.dispatchEvent(new Event('blur'))
    doc.setHidden(true)
    doc.setHidden(false)
    expect(setPaused.mock.calls).toEqual([[true], [false], [true]])
    win.dispatchEvent(new Event('focus'))
    expect(setPaused.mock.calls).toEqual([[true], [false], [true], [false]])
    dispose()
  })

  it('卸载时逐个注销监听，后续事件不能再改状态', () => {
    const win = new EventTarget()
    const doc = new VisibilityTarget()
    const setPaused = vi.fn()
    const removeWin = vi.spyOn(win, 'removeEventListener')
    const removeDoc = vi.spyOn(doc, 'removeEventListener')
    const dispose = bindAuthWaveMotion(win, doc, setPaused)

    dispose()
    dispose()
    expect(removeWin).toHaveBeenCalledTimes(2)
    expect(removeDoc).toHaveBeenCalledTimes(1)
    win.dispatchEvent(new Event('blur'))
    win.dispatchEvent(new Event('focus'))
    doc.setHidden(true)
    expect(setPaused.mock.calls).toEqual([[false]])
  })
})
