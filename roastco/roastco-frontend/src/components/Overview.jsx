import { useEffect, useState } from 'react'
import { api, inr } from '../api.js'

export default function Overview({ onOpen, onNew }) {
  const [data, setData] = useState(null)
  const [campaigns, setCampaigns] = useState([])
  const [err, setErr] = useState('')

  useEffect(() => {
    let live = true
    const load = () =>
      Promise.all([api.overview(), api.campaigns()])
        .then(([o, c]) => {
          if (!live) return
          setData(o)
          setCampaigns(c || [])
          setErr('')
        })
        .catch((e) => live && setErr(e.message))
    load()
    const t = setInterval(load, 5000)
    return () => {
      live = false
      clearInterval(t)
    }
  }, [])

  return (
    <>
      <div className="stat-grid">
        <Stat label="Shoppers" value={data ? data.customers.toLocaleString('en-IN') : '—'} />
        <Stat label="Orders" value={data ? data.orders.toLocaleString('en-IN') : '—'} />
        <Stat label="Lifetime revenue" value={data ? inr(data.revenue) : '—'} amber />
        <Stat label="Campaigns" value={data ? data.campaigns : '—'} />
      </div>

      <div className="card">
        <div className="row spread">
          <div>
            <h2>Campaigns</h2>
            <p className="sub">Most recent first. Open one for the live funnel and attributed revenue.</p>
          </div>
          <button className="btn" onClick={onNew}>
            Brew a campaign
          </button>
        </div>

        {err && <p className="error">{err}</p>}

        {campaigns.length === 0 && !err && (
          <div className="empty">
            <div className="big">Nothing brewing yet</div>
            Describe an audience in plain English and launch your first campaign.
          </div>
        )}

        {campaigns.map((c) => (
          <button key={c.id} className="camp-row" onClick={() => onOpen(c.id)}>
            <div>
              <div className="camp-name">{c.name}</div>
              <div className="camp-meta">
                <span className="pill amber">{c.channel}</span>
                <span>{c.recipients} recipients</span>
                <span>{new Date(c.launched_at).toLocaleString()}</span>
              </div>
            </div>
            <span className="muted">View →</span>
          </button>
        ))}
      </div>
    </>
  )
}

function Stat({ label, value, amber }) {
  return (
    <div className="stat">
      <div className="label">{label}</div>
      <div className={'value' + (amber ? ' amber' : '')}>{value}</div>
    </div>
  )
}
