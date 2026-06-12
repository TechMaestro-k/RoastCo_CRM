import { useEffect, useState } from 'react'
import { api } from '../api.js'

// Threads makes the channel simulation visible: pick a shopper, see the exact
// rendered message they received as a chat bubble, and watch its ticks advance
// live (✓ sent → ✓✓ delivered → ✓✓ read → clicked) as callbacks arrive.
export default function Threads({ campaignId }) {
  const [recips, setRecips] = useState([])
  const [selId, setSelId] = useState(null)
  const [comm, setComm] = useState(null)

  // Recipient rail: refresh every 4s so statuses move while you watch.
  useEffect(() => {
    let live = true
    const load = () => api.recipients(campaignId).then((r) => live && setRecips(r || [])).catch(() => {})
    load()
    const t = setInterval(load, 8000)
    return () => { live = false; clearInterval(t) }
  }, [campaignId])

  // Selected thread: poll fast (2s) — this is the live part.
  useEffect(() => {
    if (!selId) return
    let live = true
    const load = () => api.comm(selId).then((c) => live && setComm(c)).catch(() => {})
    load()
    const t = setInterval(load, 4000)
    return () => { live = false; clearInterval(t) }
  }, [selId])

  return (
    <div className="thread-wrap">
      <ul className="recip-list">
        {recips.length === 0 && <li className="muted">No recipients yet.</li>}
        {recips.map((r) => (
          <li key={r.communication_id}>
            <button
              className={selId === r.communication_id ? 'sel' : ''}
              onClick={() => { setComm(null); setSelId(r.communication_id) }}
            >
              <span>{r.customer}</span>
              <span className={'recip-status ' + r.status}>{r.status}</span>
            </button>
          </li>
        ))}
      </ul>

      <div className="thread-pane">
        {!selId && <p className="muted">Select a shopper to watch their message's journey.</p>}
        {selId && !comm && <p className="muted"><span className="spin" />Loading…</p>}
        {comm && <Thread comm={comm} />}
      </div>
    </div>
  )
}

function Thread({ comm }) {
  return (
    <>
      <p className="muted" style={{ marginTop: 0 }}>
        {comm.customer} · {comm.channel} · {comm.recipient}
      </p>
      <div className="bubble-row">
        <div className="bubble">
          {comm.message}
          <div className="ticks">
            {comm.attempts > 1 && comm.status !== 'failed' && (
              <span title="send was retried">retried ×{comm.attempts - 1}</span>
            )}
            <Ticks status={comm.status} />
            {comm.status === 'clicked' && <span className="clicked-chip">clicked the link</span>}
          </div>
        </div>
      </div>
      <Journey comm={comm} />
    </>
  )
}

// WhatsApp-style ticks driven by the monotonic status:
// queued ⏳ · sent ✓ · delivered ✓✓ · read/opened ✓✓(amber) · failed ✗
function Ticks({ status }) {
  switch (status) {
    case 'queued':
      return <span>queued…</span>
    case 'sent':
      return <span className="tick sent" title="sent">✓</span>
    case 'delivered':
      return <span className="tick delivered" title="delivered">✓✓</span>
    case 'opened':
    case 'read':
    case 'clicked':
      return <span className="tick seen" title={status}>✓✓</span>
    case 'failed':
      return <span className="tick failed" title="failed">✗ failed</span>
    default:
      return null
  }
}

// The journey: every recorded event in order, then the next expected stage
// shown as pending — so "waiting" is visible, not just absence.
const STAGE_LABEL = {
  sent: 'Sent to channel', delivered: 'Delivered', opened: 'Opened',
  read: 'Read', clicked: 'Clicked', failed: 'Failed',
}

function Journey({ comm }) {
  const events = comm.events || []
  return (
    <ul className="journey">
      {events.map((e, i) => (
        <li key={i}>
          <span className={'ev-dot ev-' + e.event_type} />
          {STAGE_LABEL[e.event_type] || e.event_type}
          <span className="when">{new Date(e.occurred_at).toLocaleTimeString()}</span>
        </li>
      ))}
      {comm.status === 'queued' && (
        <li className="pending">
          <span className="ev-dot ev-sent" />
          waiting for dispatch{comm.attempts > 0 ? ` (attempt ${comm.attempts}, retrying with backoff)` : '…'}
        </li>
      )}
      {comm.status === 'failed' && comm.last_error && (
        <li className="pending" style={{ color: 'var(--fail)' }}>{comm.last_error}</li>
      )}
    </ul>
  )
}
