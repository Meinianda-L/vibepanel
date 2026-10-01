import { useCallback, useEffect, useRef, useState } from 'react'
import { Bot, Hand, Loader2, MousePointer2, Octagon, Play, WifiOff } from 'lucide-react'

import { api } from '../../protocol/api'
import type { DesktopInput, DesktopStatus } from '../../protocol/wire'
import { t, useLang } from '../../i18n'
import { safeText } from '../text'
import { desktopKey, readRecords, toScreen } from './desktop'

/**
 * The desktop, live: a picture of the screen an agent may be operating, what
 * the agent is allowed to do right now, and the two controls that matter --
 * Stop, and Take control.
 *
 * Three sizes from one component, as PanelDetail has them: a thumbnail at the
 * top of the dock (`card`), the side panel (`panel`) and the window (`full`).
 * The card is for the corner of your eye while an agent works; pressing it is
 * how you get the controls.
 *
 * Stop is never more than one press away in the larger two, and is drawn as a
 * shape and a word as well as a colour (red line 4): it is the control
 * somebody reaches for in a hurry.
 */
export function DesktopView({ mode }: { mode: 'card' | 'panel' | 'full' }) {
  useLang()
  // Two canvases: the screen, drawn tile by tile as changes arrive, and an
  // overlay with the pointer and the agent's last click, which moves far more
  // often than the screen changes and must not cost a redraw of it.
  const canvasRef = useRef<HTMLCanvasElement | null>(null)
  const overlayRef = useRef<HTMLCanvasElement | null>(null)
  const boxRef = useRef<HTMLDivElement | null>(null)
  const [status, setStatus] = useState<DesktopStatus | null>(null)
  const [link, setLink] = useState<'connecting' | 'live' | 'down'>('connecting')
  const [control, setControl] = useState(false)
  const [busy, setBusy] = useState(false)
  // The geometry of the stream, from its latest keyframe, and where the
  // pointer is, which the capture does not include.
  const geomRef = useRef<{ screenW: number; screenH: number; viewW: number; viewH: number } | null>(null)
  const pointerRef = useRef<{ x: number; y: number }>({ x: -1, y: -1 })
  const statusRef = useRef<DesktopStatus | null>(null)
  // The view's size from the keyframe, and the box it has to fit in: the two
  // canvases are sized to the largest rectangle of the view's shape inside
  // the box, in pixels, so the overlay lies exactly over the picture.
  const [view, setView] = useState<{ w: number; h: number } | null>(null)
  const [box, setBox] = useState<{ w: number; h: number } | null>(null)
  useEffect(() => {
    const el = boxRef.current
    if (!el) return
    const ro = new ResizeObserver(([e]) => setBox({ w: e.contentRect.width, h: e.contentRect.height }))
    ro.observe(el)
    return () => ro.disconnect()
  }, [mode])

  // The width asked of the server: what this box shows, in device pixels,
  // bounded. A card never needs 1080p; a window on a 4K screen does not get
  // more than the screen has.
  const width = mode === 'card' ? 480 : mode === 'panel' ? 960 : 1920

  const paint = useCallback(() => {
    const canvas = overlayRef.current
    const geom = geomRef.current
    if (!canvas || !geom) return
    if (canvas.width !== geom.viewW || canvas.height !== geom.viewH) {
      canvas.width = geom.viewW
      canvas.height = geom.viewH
    }
    const ctx = canvas.getContext('2d')
    if (!ctx) return
    ctx.clearRect(0, 0, canvas.width, canvas.height)
    const sx = geom.viewW / Math.max(1, geom.screenW)
    const sy = geom.viewH / Math.max(1, geom.screenH)
    // Where the agent just acted: a ring that fades over a second and a half,
    // so a click that changed nothing visible is still something you saw.
    const last = statusRef.current?.last
    if (last && last.by === 'agent' && last.x !== undefined && last.y !== undefined) {
      const age = Date.now() - Date.parse(last.at)
      if (age >= 0 && age < 1500) {
        ctx.save()
        ctx.globalAlpha = 1 - age / 1500
        ctx.strokeStyle = '#f43f5e'
        ctx.lineWidth = Math.max(2, 3 * sx)
        ctx.beginPath()
        ctx.arc(last.x * sx, last.y * sy, 10 + (age / 1500) * 18, 0, Math.PI * 2)
        ctx.stroke()
        ctx.restore()
      }
    }
    // The pointer, drawn here because X's GetImage never includes it.
    const p = pointerRef.current
    if (p.x < 0) return
    const k = Math.max(0.6, Math.min(1.4, geom.viewW / 1280))
    ctx.save()
    ctx.translate(p.x * sx, p.y * sy)
    ctx.scale(k, k)
    ctx.beginPath()
    ctx.moveTo(0, 0)
    ctx.lineTo(0, 17)
    ctx.lineTo(4.5, 13)
    ctx.lineTo(8, 20)
    ctx.lineTo(10.5, 19)
    ctx.lineTo(7, 12)
    ctx.lineTo(12.5, 12)
    ctx.closePath()
    ctx.fillStyle = '#fff'
    ctx.strokeStyle = '#000'
    ctx.lineWidth = 1.2
    ctx.fill()
    ctx.stroke()
    ctx.restore()
  }, [])

  // The stream: binary frames, JSON status. Reconnects with a backoff, and
  // says it is reconnecting rather than freezing on the last picture, which
  // on a live view is indistinguishable from an agent that stopped moving.
  useEffect(() => {
    let ws: WebSocket | null = null
    let chain: Promise<void> = Promise.resolve()
    let closed = false
    let retry = 500
    let timer: ReturnType<typeof setTimeout> | null = null
    const open = () => {
      const proto = location.protocol === 'https:' ? 'wss' : 'ws'
      ws = new WebSocket(`${proto}://${location.host}/api/desktop/stream?w=${width}`)
      ws.binaryType = 'arraybuffer'
      ws.onopen = () => {
        retry = 500
      }
      ws.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          try {
            const st = JSON.parse(ev.data) as DesktopStatus
            statusRef.current = st
            setStatus(st)
            setLink(st.problem ? 'down' : 'live')
          } catch {
            /* not ours */
          }
          return
        }
        // Tiles are decoded in the order they arrived and drawn in that
        // order: decoding is asynchronous, and an older tile finishing after
        // a newer one over the same place would put back what changed.
        for (const rec of readRecords(ev.data as ArrayBuffer)) {
          if (rec.kind === 'start') {
            geomRef.current = rec
            setView({ w: rec.viewW, h: rec.viewH })
            chain = chain.then(() => {
              const c = canvasRef.current
              if (c && (c.width !== rec.viewW || c.height !== rec.viewH)) {
                c.width = rec.viewW
                c.height = rec.viewH
              }
            })
          } else if (rec.kind === 'tile') {
            const blob = new Blob([rec.jpeg], { type: 'image/jpeg' })
            chain = chain
              .then(() => createImageBitmap(blob))
              .then((bmp) => {
                canvasRef.current?.getContext('2d')?.drawImage(bmp, rec.x, rec.y)
                bmp.close()
                setLink('live')
              })
              .catch(() => {
                /* a tile that will not decode is one tile; the next keyframe fixes it */
              })
          } else {
            pointerRef.current = { x: rec.x, y: rec.y }
            paint()
          }
        }
      }
      ws.onclose = () => {
        if (closed) return
        setLink((l) => (l === 'connecting' ? 'connecting' : 'down'))
        timer = setTimeout(open, retry)
        retry = Math.min(retry * 2, 8000)
      }
    }
    open()
    // The ring fades on its own clock, not the stream's: a click that changed
    // nothing sends no new frame.
    const fade = setInterval(paint, 120)
    return () => {
      closed = true
      if (timer) clearTimeout(timer)
      clearInterval(fade)
      ws?.close()
    }
  }, [width, paint])

  const send = useCallback((input: DesktopInput) => {
    void api.desktopInput(input).catch(() => {
      /* the next status says what is wrong */
    })
  }, [])

  const stopOrResume = useCallback(() => {
    setBusy(true)
    const p = status?.stopped ? api.desktopResume() : api.desktopStop()
    void p
      .then((st) => {
        statusRef.current = st
        setStatus(st)
      })
      .finally(() => setBusy(false))
  }, [status?.stopped])

  // The person's input, only while Take control is on: hovering over the view
  // must not count as using the screen, or merely watching would keep the
  // agent off it.
  const lastMove = useRef(0)
  const at = (e: { clientX: number; clientY: number }) => {
    const canvas = canvasRef.current
    const geom = geomRef.current
    if (!canvas || !geom) return null
    return toScreen(canvas.getBoundingClientRect(), e.clientX, e.clientY, geom.screenW, geom.screenH)
  }
  const button = (b: number): 'left' | 'middle' | 'right' => (b === 1 ? 'middle' : b === 2 ? 'right' : 'left')
  const wheel = useRef(0)

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas || !control) return
    // Wheel is registered by hand, not through React, to be able to call
    // preventDefault: React's wheel listener is passive, and a wheel the page
    // also scrolls with is a wheel that does two things.
    const onWheel = (e: WheelEvent) => {
      e.preventDefault()
      const p = at(e)
      if (!p) return
      wheel.current += e.deltaY
      const notches = Math.trunc(wheel.current / 60)
      if (notches !== 0) {
        wheel.current -= notches * 60
        send({ type: 'scroll', x: p.x, y: p.y, dy: notches })
      }
    }
    canvas.addEventListener('wheel', onWheel, { passive: false })
    return () => canvas.removeEventListener('wheel', onWheel)
  })

  const onKey = (e: React.KeyboardEvent, down: boolean) => {
    if (!control) return
    e.preventDefault()
    const k = desktopKey({ ...e, isComposing: e.nativeEvent.isComposing })
    if (k.kind === 'ignore') return
    if (k.kind === 'text') {
      if (down) send({ type: 'type', text: k.value })
    } else {
      send({ type: down ? 'keydown' : 'keyup', key: k.value })
    }
  }

  const agentLine = (() => {
    if (!status) return null
    if (status.stopped) return { icon: Octagon, label: t('desk.agentStopped'), color: 'var(--vp-state-crashed)' }
    if (status.person) return { icon: Hand, label: t('desk.youActive'), color: 'var(--vp-state-waiting)' }
    return { icon: Bot, label: t('desk.agentCan'), color: 'var(--vp-state-done)' }
  })()
  // What the agent did, in its own words where it typed something: free text
  // from outside the panel, so it is only ever drawn through safeText below.
  const last = status?.last?.by === 'agent' ? status.last : undefined
  const said = last?.text ?? ''
  const lastAgent = last ? [last.kind, said].filter(Boolean).join(' ') : ''

  const linkLine =
    link === 'live' ? null : link === 'connecting' ? (
      <span className="inline-flex items-center gap-1 text-vp-xs text-ink-2">
        <Loader2 size={11} className="animate-spin" />
        {t('desk.connecting')}
      </span>
    ) : (
      <span className="inline-flex items-center gap-1 text-vp-xs" style={{ color: 'var(--vp-state-waiting)' }}>
        <WifiOff size={11} />
        {status?.problem ? t('desk.unreachable', { why: safeText(status.problem) }) : t('desk.reconnecting')}
      </span>
    )

  if (mode === 'card') {
    // Not a button: the dock's header row is the one press target, and the
    // picture under it is for looking at (see DockHeader).
    return (
      <div data-testid="desktop-card" className="px-3 py-2">
        <div className="relative overflow-hidden rounded-md border border-hairline bg-surface-2">
          <canvas ref={canvasRef} className="block h-auto w-full" />
          <canvas ref={overlayRef} className="pointer-events-none absolute inset-0 h-full w-full" />
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5">
          {agentLine && (
            <span className="inline-flex items-center gap-1 text-vp-xs" style={{ color: agentLine.color }}>
              <agentLine.icon size={11} />
              {agentLine.label}
            </span>
          )}
          {linkLine}
        </div>
      </div>
    )
  }

  return (
    <div data-testid="desktop-view" className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b border-hairline px-3 py-1.5">
        {agentLine && (
          <span
            data-testid="desktop-agent-state"
            data-stopped={status?.stopped ? 'true' : 'false'}
            className="inline-flex items-center gap-1 text-vp-sm"
            style={{ color: agentLine.color }}
          >
            <agentLine.icon size={13} />
            {agentLine.label}
          </span>
        )}
        {link === 'live' && (
          <span className="inline-flex items-center gap-1 text-vp-xs text-ink-2">
            <span className="inline-block h-1.5 w-1.5 rounded-full" style={{ background: 'var(--vp-state-done)' }} />
            {t('desk.live')} · {status?.width}×{status?.height}
          </span>
        )}
        {linkLine}
        <span className="min-w-0 flex-1" />
        <button
          type="button"
          data-testid="desktop-control"
          onClick={() => setControl((c) => !c)}
          aria-pressed={control}
          className="vp-control text-vp-xs"
        >
          <MousePointer2 size={12} />
          {control ? t('desk.release') : t('desk.take')}
        </button>
        <button
          type="button"
          data-testid="desktop-stop"
          onClick={stopOrResume}
          disabled={busy || !status}
          className="vp-control text-vp-xs disabled:opacity-50"
          style={status?.stopped ? undefined : { color: 'var(--vp-state-crashed)' }}
        >
          {status?.stopped ? <Play size={12} /> : <Octagon size={12} />}
          {status?.stopped ? t('desk.resume') : t('desk.stop')}
        </button>
      </div>
      {control && (
        <p className="border-b border-hairline px-3 py-1 text-vp-xs" style={{ color: 'var(--vp-state-waiting)' }}>
          {t('desk.takeHint')}
        </p>
      )}
      <div ref={boxRef} className="flex min-h-0 flex-1 items-center justify-center overflow-hidden bg-surface-2 p-2">
        <div
          className="relative"
          style={(() => {
            if (!view || !box) return { width: '100%', aspectRatio: '16 / 9' }
            const k = Math.min(box.w / view.w, box.h / view.h)
            return { width: Math.floor(view.w * k), height: Math.floor(view.h * k) }
          })()}
        >
          <canvas
            ref={canvasRef}
            data-testid="desktop-canvas"
            tabIndex={control ? 0 : -1}
            className="block h-full w-full rounded-md shadow-sm outline-none"
            style={{
              cursor: control ? 'none' : 'default',
              boxShadow: control ? '0 0 0 2px var(--vp-state-waiting)' : undefined,
            }}
            onContextMenu={(e) => control && e.preventDefault()}
            onPointerDown={(e) => {
              if (!control) return
              e.currentTarget.focus()
              e.currentTarget.setPointerCapture(e.pointerId)
              const p = at(e)
              if (p) send({ type: 'down', x: p.x, y: p.y, button: button(e.button) })
            }}
            onPointerUp={(e) => {
              if (!control) return
              const p = at(e)
              if (p) send({ type: 'up', x: p.x, y: p.y, button: button(e.button) })
            }}
            onPointerMove={(e) => {
              if (!control) return
              const now = performance.now()
              if (now - lastMove.current < 40) return
              lastMove.current = now
              const p = at(e)
              if (p) send({ type: 'move', x: p.x, y: p.y })
            }}
            onKeyDown={(e) => onKey(e, true)}
            onKeyUp={(e) => onKey(e, false)}
          />
          <canvas ref={overlayRef} className="pointer-events-none absolute inset-0 h-full w-full" />
        </div>
      </div>
      {lastAgent && (
        <p data-testid="desktop-last" className="truncate border-t border-hairline px-3 py-1 text-vp-xs text-ink-2">
          {t('desk.lastAgent', { what: safeText(lastAgent) })}
        </p>
      )}
    </div>
  )
}
