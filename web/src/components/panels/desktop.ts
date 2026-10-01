/**
 * The desktop view's arithmetic, apart from the component so it can be
 * tested without a canvas or a socket.
 */

/**
 * One record of a binary stream message; see internal/desktop/stream.go.
 * A message is records back to back: kind (1 byte), length (4, big-endian),
 * payload.
 */
export type StreamRecord =
  | { kind: 'start'; scale: number; screenW: number; screenH: number; viewW: number; viewH: number }
  | { kind: 'tile'; x: number; y: number; w: number; h: number; jpeg: Uint8Array<ArrayBuffer> }
  | { kind: 'pointer'; x: number; y: number }

export function readRecords(buf: ArrayBuffer): StreamRecord[] {
  const out: StreamRecord[] = []
  const v = new DataView(buf)
  let at = 0
  while (at + 5 <= buf.byteLength) {
    const kind = v.getUint8(at)
    const len = v.getUint32(at + 1)
    const p = at + 5
    if (p + len > buf.byteLength) break
    if (kind === 1 && len >= 9) {
      out.push({
        kind: 'start',
        scale: v.getUint8(p),
        screenW: v.getUint16(p + 1),
        screenH: v.getUint16(p + 3),
        viewW: v.getUint16(p + 5),
        viewH: v.getUint16(p + 7),
      })
    } else if (kind === 2 && len >= 8) {
      out.push({
        kind: 'tile',
        x: v.getUint16(p),
        y: v.getUint16(p + 2),
        w: v.getUint16(p + 4),
        h: v.getUint16(p + 6),
        jpeg: new Uint8Array(buf, p + 8, len - 8),
      })
    } else if (kind === 3 && len >= 4) {
      out.push({ kind: 'pointer', x: v.getUint16(p), y: v.getUint16(p + 2) })
    }
    // An unknown kind is skipped by its length: a newer server's record is
    // not a reason to drop the rest of the message.
    at = p + len
  }
  return out
}

/**
 * A point on the drawn picture, in screen pixels.
 *
 * From the element's box as drawn, not the canvas's own size: the picture is
 * scaled to fit, and the person clicks on what they see.
 */
export function toScreen(
  rect: { left: number; top: number; width: number; height: number },
  clientX: number,
  clientY: number,
  screenW: number,
  screenH: number,
): { x: number; y: number } | null {
  if (rect.width <= 0 || rect.height <= 0) return null
  const fx = (clientX - rect.left) / rect.width
  const fy = (clientY - rect.top) / rect.height
  if (fx < 0 || fy < 0 || fx > 1 || fy > 1) return null
  return {
    x: Math.min(screenW - 1, Math.max(0, Math.floor(fx * screenW))),
    y: Math.min(screenH - 1, Math.max(0, Math.floor(fy * screenH))),
  }
}

/**
 * What a browser key event becomes on the desktop.
 *
 * A plain character -- no Ctrl, Alt or Meta -- is text, typed as a character
 * rather than as a key, so Shift, the keyboard layout and anything outside the
 * desktop's keyboard map (Chinese, accents) come out as the person meant them.
 * Everything else is a key by the name the browser gives it, which the server
 * reads (Enter, ArrowLeft, Control, F5 …); a letter under a modifier is the
 * letter's key, lower case, so Ctrl+L is ctrl and l.
 *
 * What an input method is in the middle of composing is not a key at all:
 * the composed text arrives when it is committed.
 */
export function desktopKey(e: {
  key: string
  ctrlKey: boolean
  altKey: boolean
  metaKey: boolean
  isComposing?: boolean
}): { kind: 'text' | 'key' | 'ignore'; value: string } {
  if (e.isComposing || e.key === 'Process' || e.key === 'Dead' || e.key === 'Unidentified') {
    return { kind: 'ignore', value: '' }
  }
  const single = [...e.key].length === 1
  if (single && !e.ctrlKey && !e.altKey && !e.metaKey) return { kind: 'text', value: e.key }
  if (single) return { kind: 'key', value: e.key === ' ' ? 'space' : e.key.toLowerCase() }
  return { kind: 'key', value: e.key }
}
