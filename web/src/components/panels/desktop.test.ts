import { describe, expect, it } from 'vitest'

import { desktopKey, readRecords, toScreen } from './desktop'

/** Builds a message the way internal/desktop/stream.go does. */
function message(records: { kind: number; payload: number[] }[]): ArrayBuffer {
  const bytes: number[] = []
  for (const r of records) {
    const n = r.payload.length
    bytes.push(r.kind, (n >>> 24) & 0xff, (n >>> 16) & 0xff, (n >>> 8) & 0xff, n & 0xff, ...r.payload)
  }
  return new Uint8Array(bytes).buffer
}
const u16 = (...vs: number[]) => vs.flatMap((v) => [(v >> 8) & 0xff, v & 0xff])

describe('readRecords', () => {
  it('reads a keyframe, its tiles and the pointer from one message', () => {
    const recs = readRecords(
      message([
        { kind: 1, payload: [2, ...u16(1920, 1080, 960, 540)] },
        { kind: 2, payload: [...u16(32, 64, 32, 16), 0xff, 0xd8] },
        { kind: 3, payload: u16(500, 300) },
      ]),
    )
    expect(recs).toHaveLength(3)
    expect(recs[0]).toEqual({ kind: 'start', scale: 2, screenW: 1920, screenH: 1080, viewW: 960, viewH: 540 })
    const tile = recs[1] as Extract<(typeof recs)[number], { kind: 'tile' }>
    expect([tile.x, tile.y, tile.w, tile.h, [...tile.jpeg]]).toEqual([32, 64, 32, 16, [0xff, 0xd8]])
    expect(recs[2]).toEqual({ kind: 'pointer', x: 500, y: 300 })
  })

  it('skips a kind it does not know and stops at a truncated record', () => {
    const recs = readRecords(message([{ kind: 9, payload: [1, 2, 3] }, { kind: 3, payload: u16(1, 2) }]))
    expect(recs).toEqual([{ kind: 'pointer', x: 1, y: 2 }])
    const cut = message([{ kind: 3, payload: u16(1, 2) }]).slice(0, 7)
    expect(readRecords(cut)).toEqual([])
  })
})

describe('toScreen', () => {
  // A 1920x1080 screen drawn 960x540 at (100, 50).
  const rect = { left: 100, top: 50, width: 960, height: 540 }

  it('maps what the person clicked on to the screen pixel under it', () => {
    expect(toScreen(rect, 100, 50, 1920, 1080)).toEqual({ x: 0, y: 0 })
    expect(toScreen(rect, 580, 320, 1920, 1080)).toEqual({ x: 960, y: 540 })
    expect(toScreen(rect, 1060, 590, 1920, 1080)).toEqual({ x: 1919, y: 1079 })
  })

  it('ignores a point outside the picture', () => {
    expect(toScreen(rect, 99, 60, 1920, 1080)).toBe(null)
    expect(toScreen(rect, 500, 700, 1920, 1080)).toBe(null)
  })
})

describe('desktopKey', () => {
  const k = (key: string, mods: Partial<{ ctrlKey: boolean; altKey: boolean; metaKey: boolean; isComposing: boolean }> = {}) =>
    desktopKey({ key, ctrlKey: false, altKey: false, metaKey: false, ...mods })

  it('types plain characters, in any language, as text', () => {
    expect(k('a')).toEqual({ kind: 'text', value: 'a' })
    expect(k('A')).toEqual({ kind: 'text', value: 'A' })
    expect(k('好')).toEqual({ kind: 'text', value: '好' })
    expect(k(' ')).toEqual({ kind: 'text', value: ' ' })
  })

  it('sends shortcuts as the letter key under the modifier', () => {
    expect(k('L', { ctrlKey: true })).toEqual({ kind: 'key', value: 'l' })
    expect(k(' ', { ctrlKey: true })).toEqual({ kind: 'key', value: 'space' })
  })

  it('sends named keys by name', () => {
    expect(k('Enter')).toEqual({ kind: 'key', value: 'Enter' })
    expect(k('ArrowLeft')).toEqual({ kind: 'key', value: 'ArrowLeft' })
    expect(k('Control')).toEqual({ kind: 'key', value: 'Control' })
  })

  it('leaves an input method alone until it commits', () => {
    expect(k('Process').kind).toBe('ignore')
    expect(k('a', { isComposing: true }).kind).toBe('ignore')
  })
})
