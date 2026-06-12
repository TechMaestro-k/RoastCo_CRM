import { useEffect, useState } from 'react'
import { api, inr } from '../api.js'
import Funnel from './Funnel.jsx'
import Threads from './Threads.jsx'

export default function Detail({ id, onBack }) {
  const [camp, setCamp] = useState(null)
  const [stats, setStats] = useState(null)
  const [events, setEvents] = useState([])
  const [narration, setNarration] = useState('')
  const [toast, setToast] = useState('')
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')

  useEffect(() => {
    let live = true
    api.campaign(id).then((c) => live && setCamp(c)).catch((e) => live && setErr(e.message))
    const poll = () =>
      Promise.all([api.stats(id), api.events(id)])
        .then(([s, e]) => {
          if (!live) return
          setStats(s)
          setEvents(e || [])
        })
        .catch(() => {})
    poll()
    const t = setInterval(poll, 6000)
    return () => {
      live = false
      clearInterval(t)
    }
  }, [id])

  const simulate = async () => {
    setBusy('sim')
    setToast('')
    try {
      const r = await api.simulateOrder(id)
      setToast(
        r.attributed_to === id
          ? `${r.customer} just ordered ${inr(r.total)} — attributed to this campaign ✓`
          : `${r.customer} ordered ${inr(r.total)} — no qualifying touch, not attributed (honest attribution).`
      )
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy('')
    }
  }

  const narrate = async () => {
    setBusy('narrate')
    try {
      const r = await api.narrate(id)
      setNarration(r.narration)
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy('')
    }
  }

  const status = stats?.derived_status || 'sending'

  return (
    <>
      <button className="btn ghost small" onClick={onBack} style={{ marginBottom: 18 }}>
        ← All campaigns
      </button>

      <div className="card">
        <div className="detail-head">
          <div>
            <h2>{camp ? camp.name : '…'}</h2>
            {camp?.source_intent && <p className="intent-quote">“{camp.source_intent}”</p>}
          </div>
          <div className="row">
            <span className="pill amber">{camp?.channel}</span>
            <span className="pill">
              <span className={'status-dot ' + status} />
              {status}
            </span>
          </div>
        </div>
      </div>

      <div className="stat-grid">
        <Tile label="Recipients" v={stats ? stats.recipients : '—'} />
        <Tile label="Delivery rate" v={rate(stats, 'delivery')} />
        <Tile label="Click rate" v={rate(stats, 'click')} />
        <Tile label="Failed" v={stats ? stats.failed : '—'} />
      </div>

      <div className="card">
        <h2>The funnel</h2>
        <p className="sub">
          Cumulative by stage — statuses only move forward, so late or duplicate callbacks can't
          bend these numbers.
        </p>
        {stats && <Funnel stats={stats} />}
      </div>

      <div className="card">
        <h2>Message threads</h2>
        <p className="sub">
          The channel at work: pick a shopper, see the exact message they got, and watch the
          ticks advance live as delivery callbacks arrive.
        </p>
        <Threads campaignId={id} />
      </div>

      <div className="card">
        <div className="row spread">
          <div>
            <h2>Attributed revenue</h2>
            <p className="sub">
              Last touch within {7} days — clicks first, honest about what it can't prove.
            </p>
            <div className="revenue-big">{stats ? inr(stats.revenue_attributed) : '—'}</div>
            <p className="muted mt">
              {stats?.orders_attributed ?? 0} orders ·{' '}
              {stats?.rates?.conversion ? stats.rates.conversion.toFixed(1) : '0'}% conversion
            </p>
          </div>
          <div style={{ textAlign: 'right' }}>
            <button className="btn" disabled={busy === 'sim'} onClick={simulate}>
              {busy === 'sim' ? <><span className="spin" />Placing order…</> : 'Simulate incoming order'}
            </button>
            <p className="muted mt" style={{ maxWidth: 240 }}>
              Sends a real order through the live ingest + attribution path.
            </p>
          </div>
        </div>
        {toast && <p className="toast">{toast}</p>}
      </div>

      <div className="card">
        <div className="row spread">
          <h2>What happened here?</h2>
          <button className="btn ghost small" disabled={busy === 'narrate'} onClick={narrate}>
            {busy === 'narrate' ? <><span className="spin" />Reading…</> : 'Ask the AI'}
          </button>
        </div>
        {narration && <div className="narration">{narration}</div>}
      </div>

      <div className="card">
        <h2>Live event feed</h2>
        <p className="sub">Raw receipts from the channel, newest first.</p>
        <ul className="event-feed">
          {events.length === 0 && <li className="muted">No events yet — the kettle's still on.</li>}
          {events.map((e, i) => (
            <li key={i}>
              <span className={'ev-dot ev-' + e.event_type} />
              <span className="ev-name">{e.customer}</span>
              <span>{e.event_type}</span>
              <span style={{ marginLeft: 'auto' }}>
                {new Date(e.occurred_at).toLocaleTimeString()}
              </span>
            </li>
          ))}
        </ul>
      </div>

      {err && <p className="error">{err}</p>}
    </>
  )
}

const rate = (s, k) => (s?.rates?.[k] != null ? s.rates[k].toFixed(1) + '%' : '—')

function Tile({ label, v }) {
  return (
    <div className="stat">
      <div className="label">{label}</div>
      <div className="value">{v}</div>
    </div>
  )
}
