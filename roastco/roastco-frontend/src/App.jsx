import { useState } from 'react'
import Overview from './components/Overview.jsx'
import Studio from './components/Studio.jsx'
import Detail from './components/Detail.jsx'

export default function App() {
  const [view, setView] = useState({ name: 'overview' })

  const openCampaign = (id) => setView({ name: 'detail', id })

  return (
    <div className="shell">
      <header className="header">
        <div className="brand">
          <h1 className="wordmark">
            Roast <span className="amp">&amp;</span> Co
          </h1>
          <span className="eyebrow">Campaign Studio</span>
        </div>
        <nav className="nav" aria-label="Main">
          <button
            className={view.name === 'overview' ? 'active' : ''}
            onClick={() => setView({ name: 'overview' })}
          >
            Overview
          </button>
          <button
            className={view.name === 'studio' ? 'active' : ''}
            onClick={() => setView({ name: 'studio' })}
          >
            New campaign
          </button>
        </nav>
      </header>

      {view.name === 'overview' && (
        <Overview onOpen={openCampaign} onNew={() => setView({ name: 'studio' })} />
      )}
      {view.name === 'studio' && <Studio onLaunched={openCampaign} />}
      {view.name === 'detail' && (
        <Detail id={view.id} onBack={() => setView({ name: 'overview' })} />
      )}
    </div>
  )
}
