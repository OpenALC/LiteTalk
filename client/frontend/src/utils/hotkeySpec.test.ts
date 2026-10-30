// hotkeySpec 单测：归一化纯函数必须与 Go 侧 hotkey.ParseBinding 语义逐条对齐
// （任务书 §0.2）。这里覆盖合法/非法/规范化/round-trip、DOM 事件映射与冲突检测。
import { describe, expect, it } from 'vitest'
import {
  HOTKEY_ACTIONS,
  IN_APP_COMBOS,
  bindingFromKeyEvent,
  bindingFromMouseEvent,
  bindingLabel,
  findHotkeyConflicts,
  findInAppClashes,
  modifierPrefix,
  normalizeBinding,
} from './hotkeySpec'

// 合法样本表：契约里出现的每一类主键至少一个，同时用于 round-trip。
const VALID: Array<[string, string]> = [
  ['ctrl+m', 'ctrl+m'],
  ['Ctrl+M', 'ctrl+m'],
  ['CTRL+SHIFT+M', 'ctrl+shift+m'],
  ['Shift+Ctrl+M', 'ctrl+shift+m'], // 契约示例：修饰键顺序不敏感
  ['alt+shift+ctrl+win+f13', 'ctrl+alt+shift+win+f13'],
  ['a', 'a'],
  ['z', 'z'],
  ['0', '0'],
  ['9', '9'],
  ['f1', 'f1'],
  ['f9', 'f9'],
  ['f10', 'f10'],
  ['f24', 'f24'],
  ['space', 'space'],
  ['tab', 'tab'],
  ['esc', 'esc'],
  ['enter', 'enter'],
  ['up', 'up'],
  ['down', 'down'],
  ['left', 'left'],
  ['right', 'right'],
  ['home', 'home'],
  ['end', 'end'],
  ['pgup', 'pgup'],
  ['pgdn', 'pgdn'],
  ['ins', 'ins'],
  ['del', 'del'],
  ['`', '`'],
  ['-', '-'],
  ['=', '='],
  ['[', '['],
  [']', ']'],
  ['\\', '\\'],
  [';', ';'],
  ["'", "'"],
  [',', ','],
  ['.', '.'],
  ['/', '/'],
  ['ctrl+/', 'ctrl+/'],
  ['mouse1', 'mouse1'],
  ['mouse2', 'mouse2'],
  ['mouse3', 'mouse3'],
  ['mouse4', 'mouse4'],
  ['mouse5', 'mouse5'],
  ['ctrl+alt+mouse5', 'ctrl+alt+mouse5'], // 鼠标键可带修饰键
]

describe('normalizeBinding 合法输入', () => {
  it.each(VALID)('%s → %s', (input, want) => {
    expect(normalizeBinding(input)).toBe(want)
  })

  it('规范化后全小写、修饰键固定序', () => {
    expect(normalizeBinding('WIN+ALT+SHIFT+CTRL+F13')).toBe('ctrl+alt+shift+win+f13')
    expect(normalizeBinding('Shift+Alt+Ctrl+Win+a')).toBe('ctrl+alt+shift+win+a')
  })

  it('round-trip：规范化产物再解析不变', () => {
    for (const [, norm] of VALID) {
      expect(normalizeBinding(norm)).toBe(norm)
    }
  })

  it('空白容忍：整串与 token 两侧空白被去掉', () => {
    expect(normalizeBinding('  ctrl + m ')).toBe('ctrl+m')
  })
})

describe('normalizeBinding 非法输入', () => {
  it.each([
    ['空串', ''],
    ['纯空白', '   '],
    ['只有修饰键', 'ctrl'],
    ['多个修饰键无主键', 'ctrl+shift'],
    ['修饰键重复', 'ctrl+ctrl+m'],
    ['修饰键重复（非相邻）', 'ctrl+alt+ctrl+m'],
    ['两个主键', 'ctrl+m+k'],
    ['两个主键（无修饰键）', 'm+k'],
    ['空 token', 'ctrl++m'],
    ['尾部多余分隔符', 'ctrl+m+'],
    ['首部分隔符', '+m'],
    ['未知 token', 'foo'],
    ['修饰键加未知 token', 'ctrl+foo'],
    ['f0 越界', 'f0'],
    ['f25 越界', 'f25'],
    ['mouse0 越界', 'mouse0'],
    ['mouse6 越界', 'mouse6'],
    ['meta 不是契约修饰键', 'meta+m'],
    ['cmd 不是契约修饰键', 'cmd+m'],
    ['标点表外符号', '@'],
    ['修饰键加表外符号', 'ctrl+@'],
    ['鼠标加第二个主键', 'mouse4+m'],
  ])('%s → null', (_name, input) => {
    expect(normalizeBinding(input)).toBeNull()
  })
})

describe('bindingFromKeyEvent 键盘事件映射', () => {
  const mods = { ctrlKey: false, altKey: false, shiftKey: false, metaKey: false }

  it('字母键带修饰键', () => {
    expect(bindingFromKeyEvent({ ...mods, ctrlKey: true, key: 'm' })).toBe('ctrl+m')
    expect(bindingFromKeyEvent({ ...mods, ctrlKey: true, shiftKey: true, key: 'M' })).toBe('ctrl+shift+m')
  })

  it('命名键按 alias 翻成契约 token', () => {
    expect(bindingFromKeyEvent({ ...mods, key: ' ' })).toBe('space')
    expect(bindingFromKeyEvent({ ...mods, key: 'Escape' })).toBe('esc')
    expect(bindingFromKeyEvent({ ...mods, key: 'Enter' })).toBe('enter')
    expect(bindingFromKeyEvent({ ...mods, key: 'Tab' })).toBe('tab')
    expect(bindingFromKeyEvent({ ...mods, key: 'ArrowUp' })).toBe('up')
    expect(bindingFromKeyEvent({ ...mods, key: 'ArrowDown' })).toBe('down')
    expect(bindingFromKeyEvent({ ...mods, key: 'ArrowLeft' })).toBe('left')
    expect(bindingFromKeyEvent({ ...mods, key: 'ArrowRight' })).toBe('right')
    expect(bindingFromKeyEvent({ ...mods, key: 'PageUp' })).toBe('pgup')
    expect(bindingFromKeyEvent({ ...mods, key: 'PageDown' })).toBe('pgdn')
    expect(bindingFromKeyEvent({ ...mods, key: 'Insert' })).toBe('ins')
    expect(bindingFromKeyEvent({ ...mods, key: 'Delete' })).toBe('del')
    expect(bindingFromKeyEvent({ ...mods, key: 'Home' })).toBe('home')
    expect(bindingFromKeyEvent({ ...mods, key: 'End' })).toBe('end')
    expect(bindingFromKeyEvent({ ...mods, key: 'F13' })).toBe('f13')
  })

  it('metaKey 映射为 win 修饰键', () => {
    expect(bindingFromKeyEvent({ ...mods, metaKey: true, key: 'm' })).toBe('win+m')
  })

  it('不可绑定的按键返回空串（录制态继续等待）', () => {
    expect(bindingFromKeyEvent({ ...mods, ctrlKey: true, key: 'Control' })).toBe('')
    expect(bindingFromKeyEvent({ ...mods, key: 'Meta' })).toBe('')
    expect(bindingFromKeyEvent({ ...mods, key: 'Shift' })).toBe('')
    expect(bindingFromKeyEvent({ ...mods, key: 'CapsLock' })).toBe('')
    expect(bindingFromKeyEvent({ ...mods, key: 'Dead' })).toBe('')
    expect(bindingFromKeyEvent({ ...mods, key: 'F25' })).toBe('')
    expect(bindingFromKeyEvent({ ...mods, shiftKey: true, key: '@' })).toBe('')
  })
})

describe('bindingFromMouseEvent 鼠标事件映射', () => {
  const mods = { ctrlKey: false, altKey: false, shiftKey: false, metaKey: false }

  it('button 0-4 映射 mouse1-mouse5', () => {
    expect(bindingFromMouseEvent({ ...mods, button: 0 })).toBe('mouse1')
    expect(bindingFromMouseEvent({ ...mods, button: 1 })).toBe('mouse2')
    expect(bindingFromMouseEvent({ ...mods, button: 2 })).toBe('mouse3')
    expect(bindingFromMouseEvent({ ...mods, button: 3 })).toBe('mouse4')
    expect(bindingFromMouseEvent({ ...mods, button: 4 })).toBe('mouse5')
  })

  it('侧键可带修饰键', () => {
    expect(bindingFromMouseEvent({ ...mods, ctrlKey: true, altKey: true, button: 4 })).toBe('ctrl+alt+mouse5')
  })

  it('越界按键返回空串', () => {
    expect(bindingFromMouseEvent({ ...mods, button: 5 })).toBe('')
    expect(bindingFromMouseEvent({ ...mods, button: -1 })).toBe('')
  })
})

describe('modifierPrefix', () => {
  it('按固定序拼接，无修饰键返回空串', () => {
    expect(modifierPrefix({ ctrlKey: false, altKey: false, shiftKey: false, metaKey: false })).toBe('')
    expect(modifierPrefix({ ctrlKey: false, altKey: true, shiftKey: true, metaKey: false })).toBe('alt+shift')
    expect(modifierPrefix({ ctrlKey: true, altKey: true, shiftKey: true, metaKey: true })).toBe('ctrl+alt+shift+win')
  })
})

describe('bindingLabel 展示文本', () => {
  it('规范化后逐 token 首字母大写', () => {
    expect(bindingLabel('ctrl+shift+m')).toBe('Ctrl + Shift + M')
    expect(bindingLabel('ctrl+alt+mouse5')).toBe('Ctrl + Alt + Mouse5')
    expect(bindingLabel('Shift+Ctrl+M')).toBe('Ctrl + Shift + M')
  })

  it('未绑定或非法返回空串', () => {
    expect(bindingLabel('')).toBe('')
    expect(bindingLabel(undefined)).toBe('')
    expect(bindingLabel('foo')).toBe('')
  })
})

describe('findHotkeyConflicts 冲突检测', () => {
  it('全未绑定无冲突', () => {
    expect(findHotkeyConflicts({})).toEqual([])
    expect(findHotkeyConflicts({ ptt: '', mute: 'ctrl+m' })).toEqual([])
  })

  it('四个动作各占不同组合无冲突', () => {
    expect(
      findHotkeyConflicts({ ptt: 'mouse4', mute: 'ctrl+m', deafen: 'ctrl+d', dual: 'ctrl+shift+m' }),
    ).toEqual([])
  })

  it('同一组合被两个动作占用即冲突', () => {
    expect(findHotkeyConflicts({ ptt: 'ctrl+m', mute: 'ctrl+m' })).toEqual(['ctrl+m'])
  })

  it('三个动作撞同一组合只报一次', () => {
    expect(findHotkeyConflicts({ ptt: 'f13', mute: 'f13', dual: 'f13' })).toEqual(['f13'])
  })

  it('多组冲突按首次出现序全部列出', () => {
    expect(findHotkeyConflicts({ ptt: 'mouse4', mute: 'mouse4', deafen: 'ctrl+m', dual: 'ctrl+m' })).toEqual([
      'mouse4',
      'ctrl+m',
    ])
  })

  it('按规范化结果比较，大小写与修饰键顺序不同也算冲突', () => {
    expect(findHotkeyConflicts({ ptt: 'Ctrl+M', mute: 'ctrl+m' })).toEqual(['ctrl+m'])
    expect(findHotkeyConflicts({ ptt: 'Shift+Ctrl+M', mute: 'ctrl+shift+m' })).toEqual(['ctrl+shift+m'])
  })

  it('非法绑定串不参与冲突判定', () => {
    expect(findHotkeyConflicts({ ptt: 'foo', mute: 'foo' })).toEqual([])
  })
})

describe('HOTKEY_ACTIONS 动作表', () => {
  it('与 Go 侧 hotkey.Action 的五个动作一一对应', () => {
    expect(HOTKEY_ACTIONS.map((a) => a.id)).toEqual(['ptt', 'whisper', 'mute', 'deafen', 'dual'])
  })
})

describe('IN_APP_COMBOS 应用内快捷键组合', () => {
  it('由 useHotkeys 的 HOTKEYS 归一化而来，覆盖闭麦/静音/全关/搜索/设置', () => {
    // 规范化后应含 ctrl+m、ctrl+d、ctrl+shift+m、ctrl+k、ctrl+,
    expect(IN_APP_COMBOS).toContain('ctrl+m')
    expect(IN_APP_COMBOS).toContain('ctrl+d')
    expect(IN_APP_COMBOS).toContain('ctrl+shift+m')
    expect(IN_APP_COMBOS).toContain('ctrl+k')
    expect(IN_APP_COMBOS).toContain('ctrl+,')
  })

  it('不含非法组合（HOTKEYS 若写出非法串应被滤掉而不是抛错）', () => {
    for (const c of IN_APP_COMBOS) {
      expect(normalizeBinding(c)).toBe(c)
    }
  })
})

describe('findInAppClashes 与应用内快捷键撞车', () => {
  it('全局绑定撞上应用内组合时报告，未绑定不报告', () => {
    expect(findInAppClashes({ ptt: 'ctrl+m' })).toEqual(['ctrl+m'])
    expect(findInAppClashes({ mute: 'ctrl+shift+m' })).toEqual(['ctrl+shift+m'])
    expect(findInAppClashes({ ptt: 'ctrl+alt+mouse5' })).toEqual([])
    expect(findInAppClashes({})).toEqual([])
  })

  it('规范化后比较：大小写与修饰键顺序不同也算撞车', () => {
    expect(findInAppClashes({ ptt: 'Ctrl+M' })).toEqual(['ctrl+m'])
    expect(findInAppClashes({ ptt: 'Shift+Ctrl+M' })).toEqual(['ctrl+shift+m'])
  })

  it('非法绑定串不参与判定', () => {
    expect(findInAppClashes({ ptt: 'foo' })).toEqual([])
  })

  it('多个动作撞车时按动作序全部报告', () => {
    expect(findInAppClashes({ ptt: 'ctrl+m', mute: 'ctrl+d' })).toEqual(['ctrl+m', 'ctrl+d'])
  })
})
