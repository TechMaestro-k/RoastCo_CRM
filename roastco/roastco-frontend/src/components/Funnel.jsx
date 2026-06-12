const STAGES = [
  ['sent', 'Sent'],
  ['delivered', 'Delivered'],
  ['opened', 'Opened'],
  ['read', 'Read'],
  ['clicked', 'Clicked'],
]

export default function Funnel({ stats }) {
  const total = stats.recipients || 1
  return (
    <>
      <div className="funnel">
        {STAGES.map(([key, label]) => {
          const n = stats.funnel?.[key] ?? 0
          const pct = Math.round((n / total) * 100)
          return (
            <div className="pour" key={key}>
              <span className="stage">{label}</span>
              <div className="track">
                <div className="fill" style={{ width: pct + '%' }} />
              </div>
              <div className="nums">
                <span className="n">{n.toLocaleString('en-IN')}</span>
                <span className="p">{pct}%</span>
              </div>
            </div>
          )
        })}
      </div>
      {stats.failed > 0 && (
        <div className="fail-line">
          <span className="fail-dot" />
          {stats.failed} failed ({Math.round((stats.failed / total) * 100)}%) — retried, then
          dead-lettered honestly
        </div>
      )}
      {stats.pending > 0 && (
        <div className="muted mt">{stats.pending} still queued (retrying with backoff)</div>
      )}
    </>
  )
}
