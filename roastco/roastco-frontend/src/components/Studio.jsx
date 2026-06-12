import { useEffect, useRef, useState } from 'react'
import { api } from '../api.js'

const EXAMPLES = [
  'Win back customers who bought beans in the last 6 months but never bought a grinder, and have not ordered in 60 days — give them 20% off',
  'VIP customers in Gurgaon who love equipment',
  'Everyone who bought ground coffee in the last 3 months',
]

const NUMERIC = ['total_spend', 'order_count', 'days_since_last_order', 'days_since_signup']
const PURCHASE = ['purchased_category', 'purchased_product']

export default function Studio({ onLaunched }) {
  const [meta, setMeta] = useState(null)
  const [intent, setIntent] = useState('')
  const [plan, setPlan] = useState(null) // {spec, segment_name, interpretation, count, sample}
  const [rules, setRules] = useState([])
  const [match, setMatch] = useState('all')
  const [draft, setDraft] = useState(null) // {message, channel, channel_reason, campaign_name}
  const [message, setMessage] = useState('')
  const [channel, setChannel] = useState('email')
  const [name, setName] = useState('')
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')
  const idemKey = useRef(crypto.randomUUID())
  const msgRef = useRef(null)

  useEffect(() => {
    api.meta().then(setMeta).catch(() => {})
  }, [])

  const runPreview = async () => {
    setBusy('preview')
    setErr('')
    try {
      const p = await api.preview(intent)
      setPlan(p)
      setRules(p.spec.rules)
      setMatch(p.spec.match)
      setName(p.segment_name || '')
      setDraft(null)
      setMessage('')
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy('')
    }
  }

  const rerunSpec = async (nextRules, nextMatch) => {
    setBusy('rerun')
    setErr('')
    try {
      const definition = { match: nextMatch, rules: nextRules }
      const p = await api.previewSpec(definition)
      setPlan((old) => ({ ...old, spec: definition, count: p.count, sample: p.sample }))
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy('')
    }
  }

  const runDraft = async () => {
    setBusy('draft')
    setErr('')
    try {
      const d = await api.draft(intent, plan.spec)
      setDraft(d)
      setMessage(d.message)
      setChannel(d.channel)
      if (d.campaign_name) setName(d.campaign_name)
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy('')
    }
  }

  const launch = async () => {
    setBusy('launch')
    setErr('')
    try {
      const res = await api.launch(
        {
          name: name || 'Untitled campaign',
          channel,
          message,
          definition: plan.spec,
          source_intent: intent,
          segment_name: plan.segment_name || name,
        },
        idemKey.current
      )
      onLaunched(res.campaign_id)
    } catch (e) {
      setErr(e.message)
      setBusy('')
    }
  }

  const insertToken = (t) => {
    setMessage((m) => (m ? m.trimEnd() + ' ' + t : t))
    msgRef.current?.focus()
  }

  return (
    <>
      <Step n="01" title="Describe the audience" last={!plan}>
        <p className="sub">
          Plain English in — the AI proposes the audience rules, you stay in charge.
        </p>
        <textarea
          className="intent"
          value={intent}
          placeholder="e.g. win back customers who bought beans but haven't ordered in 60 days…"
          onChange={(e) => setIntent(e.target.value)}
        />
        <div className="row mt">
          <button className="btn" disabled={!intent.trim() || busy === 'preview'} onClick={runPreview}>
            {busy === 'preview' ? <><span className="spin" />Reading intent…</> : 'Preview audience'}
          </button>
        </div>
        <div className="chip-row">
          {EXAMPLES.map((ex) => (
            <button key={ex} className="chip" onClick={() => setIntent(ex)}>
              {ex.length > 64 ? ex.slice(0, 61) + '…' : ex}
            </button>
          ))}
        </div>
      </Step>

      {plan && (
        <Step n="02" title="Review the audience">
          <p className="interp">{plan.interpretation}</p>
          <div className="count-line">
            <span className="count-big">{plan.count.toLocaleString('en-IN')}</span>
            <span className="count-cap">shoppers match right now</span>
            {busy === 'rerun' && <span className="spin" />}
          </div>

          <RuleEditor
            meta={meta}
            rules={rules}
            match={match}
            onChange={(r, m) => {
              setRules(r)
              setMatch(m)
            }}
            onApply={() => rerunSpec(rules, match)}
            busy={busy === 'rerun'}
          />

          {plan.sample?.length > 0 && (
            <div className="table-scroll">
              <table className="sample">
                <thead>
                  <tr>
                    <th>Sample shopper</th>
                    <th>City</th>
                    <th>Spend</th>
                    <th>Orders</th>
                    <th>Last order</th>
                  </tr>
                </thead>
                <tbody>
                  {plan.sample.map((s) => (
                    <tr key={s.id}>
                      <td>{s.name}</td>
                      <td>{s.city}</td>
                      <td>₹{Math.round(s.total_spend).toLocaleString('en-IN')}</td>
                      <td>{s.order_count}</td>
                      <td>{s.days_since_last_order == null ? '—' : `${s.days_since_last_order}d ago`}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Step>
      )}

      {plan && (
        <Step n="03" title="Write the message">
          <p className="sub">
            One template for everyone — tokens fill per shopper at send time, with safe fallbacks.
          </p>
          <div className="row" style={{ marginBottom: 12 }}>
            <button className="btn ghost" disabled={busy === 'draft'} onClick={runDraft}>
              {busy === 'draft' ? <><span className="spin" />Drafting…</> : draft ? 'Redraft with AI' : 'Draft with AI'}
            </button>
          </div>
          <textarea ref={msgRef} value={message} onChange={(e) => setMessage(e.target.value)} placeholder="Hi {{first_name}}, …" />
          <div className="token-row">
            {(meta?.tokens || []).map((t) => (
              <button key={t} className="chip mono" onClick={() => insertToken(t)}>
                {t}
              </button>
            ))}
          </div>
          <div className="channel-row">
            {(meta?.channels || ['email', 'sms', 'whatsapp', 'rcs']).map((c) => (
              <button
                key={c}
                className={'channel-pill' + (channel === c ? ' selected' : '')}
                onClick={() => setChannel(c)}
              >
                {c}
                {draft?.channel === c && <span className="ai-badge">AI suggests</span>}
              </button>
            ))}
          </div>
          {draft?.channel_reason && <p className="reason">{draft.channel_reason}</p>}
        </Step>
      )}

      {plan && message.trim() && (
        <Step n="04" title="Launch" last>
          <div className="rule-row" style={{ gridTemplateColumns: '1fr' }}>
            <input
              type="text"
              value={name}
              placeholder="Campaign name"
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <p className="muted">
            {plan.count.toLocaleString('en-IN')} shoppers · {channel} · launch is idempotent — a
            double-click can't send twice.
          </p>
          <div className="row mt">
            <button className="btn" disabled={busy === 'launch'} onClick={launch}>
              {busy === 'launch' ? <><span className="spin" />Launching…</> : `Launch to ${plan.count.toLocaleString('en-IN')} shoppers`}
            </button>
          </div>
        </Step>
      )}

      {err && <p className="error">{err}</p>}
    </>
  )
}

function Step({ n, title, children, last }) {
  return (
    <div className="step">
      <div className="step-rail">
        <div className="step-num">{n}</div>
        {!last && <div className="step-line" />}
      </div>
      <div className="step-body">
        <div className="card" style={{ marginBottom: 0 }}>
          <h2>{title}</h2>
          {children}
        </div>
      </div>
    </div>
  )
}

function RuleEditor({ meta, rules, match, onChange, onApply, busy }) {
  const fields = meta?.fields || []
  const categories = meta?.categories || []
  const opsFor = (f) => fields.find((x) => x.field === f)?.ops || ['=']

  const update = (i, patch) => {
    const next = rules.map((r, j) => (j === i ? { ...r, ...patch } : r))
    onChange(next, match)
  }
  const remove = (i) => onChange(rules.filter((_, j) => j !== i), match)
  const add = () =>
    onChange([...rules, { field: 'days_since_last_order', op: '>', value: 30 }], match)

  return (
    <div style={{ marginBottom: 18 }}>
      <div className="row" style={{ marginBottom: 10 }}>
        <span className="muted">Shoppers matching</span>
        <select
          style={{ width: 'auto' }}
          value={match}
          onChange={(e) => onChange(rules, e.target.value)}
        >
          <option value="all">all rules</option>
          <option value="any">any rule</option>
        </select>
      </div>

      {rules.map((r, i) => (
        <div key={i}>
          <div className="rule-row">
            <select
              value={r.field}
              onChange={(e) => {
                const f = e.target.value
                const ops = opsFor(f)
                update(i, {
                  field: f,
                  op: ops.includes(r.op) ? r.op : ops[0],
                  value: defaultValue(f, categories),
                  within_days: undefined,
                })
              }}
            >
              {fields.map((f) => (
                <option key={f.field} value={f.field}>
                  {f.field.replaceAll('_', ' ')}
                </option>
              ))}
            </select>
            <select value={r.op} onChange={(e) => update(i, { op: e.target.value })}>
              {opsFor(r.field).map((o) => (
                <option key={o} value={o}>
                  {o}
                </option>
              ))}
            </select>
            <ValueInput rule={r} categories={categories} onChange={(value) => update(i, { value })} />
            <button className="icon-btn" aria-label="Remove rule" onClick={() => remove(i)}>
              ×
            </button>
          </div>
          {PURCHASE.includes(r.field) && (
            <div className="rule-extra" style={{ marginBottom: 8 }}>
              within the last
              <input
                type="number"
                min="0"
                value={r.within_days ?? ''}
                placeholder="∞"
                onChange={(e) =>
                  update(i, {
                    within_days: e.target.value === '' ? undefined : Number(e.target.value),
                  })
                }
              />
              days (blank = ever)
            </div>
          )}
        </div>
      ))}

      <div className="row mt">
        <button className="btn ghost small" onClick={add}>
          + Add rule
        </button>
        <button className="btn small" disabled={busy || rules.length === 0} onClick={onApply}>
          Apply &amp; re-count
        </button>
      </div>
    </div>
  )
}

function ValueInput({ rule, categories, onChange }) {
  const { field, op, value } = rule
  if (op === 'between') {
    const [lo, hi] = Array.isArray(value) ? value : ['', '']
    return (
      <div className="row" style={{ gap: 6 }}>
        <input type="number" value={lo} onChange={(e) => onChange([num(e.target.value), num(hi)])} />
        <input type="number" value={hi} onChange={(e) => onChange([num(lo), num(e.target.value)])} />
      </div>
    )
  }
  if (op === 'in') {
    return (
      <input
        type="text"
        value={Array.isArray(value) ? value.join(', ') : value || ''}
        placeholder="comma, separated"
        onChange={(e) => onChange(e.target.value.split(',').map((s) => s.trim()).filter(Boolean))}
      />
    )
  }
  if (field === 'purchased_category' || field === 'favorite_category') {
    return (
      <select value={value || categories[0] || ''} onChange={(e) => onChange(e.target.value)}>
        {categories.map((c) => (
          <option key={c} value={c}>
            {c}
          </option>
        ))}
      </select>
    )
  }
  if (NUMERIC.includes(field)) {
    return <input type="number" value={value ?? ''} onChange={(e) => onChange(num(e.target.value))} />
  }
  return <input type="text" value={value || ''} onChange={(e) => onChange(e.target.value)} />
}

const num = (v) => (v === '' || v === null ? '' : Number(v))
const defaultValue = (field, categories) => {
  if (NUMERIC.includes(field)) return 30
  if (field === 'purchased_category' || field === 'favorite_category') return categories[0] || 'beans'
  return ''
}
