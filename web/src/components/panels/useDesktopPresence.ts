import { useEffect, useRef, useState } from 'react'

import { api } from '../../protocol/api'

/**
 * Whether the panel has a desktop, and when the agent last acted on it.
 *
 * A status poll every few seconds rather than the stream: the stream is
 * pictures, and the question here -- did an agent just start using the
 * computer, so the screen should open beside the terminal -- is one small
 * JSON answer. Asked only while the page is visible, and not at all once the
 * first answer says there is no desktop.
 */
export function useDesktopPresence(onAgent?: () => void): { enabled: boolean; agentAt: string | null } {
  const [enabled, setEnabled] = useState(false)
  const [agentAt, setAgentAt] = useState<string | null>(null)
  // Called when the agent acts after the first answer: a new action, rather
  // than whatever it last did before this page opened.
  const onAgentRef = useRef(onAgent)
  useEffect(() => {
    onAgentRef.current = onAgent
  })
  useEffect(() => {
    let seen: string | null | undefined
    let live = true
    let timer: ReturnType<typeof setTimeout> | null = null
    let first = true
    const ask = () => {
      // The first answer is always fetched, hidden or not: it is what decides
      // whether the header has a screen toggle at all, and a panel opened in
      // a background tab came up without one until somebody looked at it.
      // After that, a hidden page is not asked about.
      if (document.hidden && !first) {
        timer = setTimeout(ask, 3000)
        return
      }
      first = false
      void api.desktop().then(
        (d) => {
          if (!live) return
          setEnabled(d.enabled)
          if (!d.enabled) return
          const last = d.status?.last
          const at = last && last.by === 'agent' ? last.at : null
          if (seen !== undefined && at !== null && at !== seen) onAgentRef.current?.()
          seen = at
          setAgentAt(at)
          timer = setTimeout(ask, 3000)
        },
        () => {
          if (live) timer = setTimeout(ask, 10000)
        },
      )
    }
    ask()
    return () => {
      live = false
      if (timer) clearTimeout(timer)
    }
  }, [])
  return { enabled, agentAt }
}
