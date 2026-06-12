# Roast & Co — Campaign Studio (frontend)

React + Vite single-page app for the Roast & Co mini-CRM. The whole flow is one screen-to-screen story: describe an audience in plain English → review and edit the rules the AI proposed → draft the message → launch → watch the funnel fill and revenue attribute, live.

Backend lives in its own repo (Go + Postgres). This app talks to it over a small JSON API; the only configuration is the API base URL.

## Run

```bash
npm install
cp .env.example .env        # set VITE_API_URL (default http://localhost:8080)
npm run dev                 # :5173
```

Production build: `npm run build` → static files in `dist/`.

## Deploy (Vercel)

Import the repo, framework preset **Vite**, env var `VITE_API_URL=https://<your-crm>.up.railway.app`. Set the backend's `FRONTEND_ORIGIN` to the Vercel URL so CORS is scoped rather than `*`.

## Design notes — "dark roast"

Espresso base `#16100b`, raised cards `#211810`, warm hairlines `#3a2c1c`, cream type `#f2e7d5`, one amber accent `#e8a849 → #c77b3a`; brick `#c4564a` is reserved for failure states. Type: **Fraunces** for display and numerals, **Archivo** for UI, **JetBrains Mono** for specs and template tokens. The signature element is the *brew funnel* — campaign stages as pour bars filling with a crema-edged amber gradient. The Studio's numbered steps aren't decoration: the flow genuinely is a sequence (describe → audience → message → launch), and each step's output feeds the next.

Quality floor: keyboard focus rings throughout, `prefers-reduced-motion` respected, responsive to ~360px. No UI framework — one hand-written stylesheet.

## Structure

```
src/
  api.js                 # tiny fetch client + ₹ formatter
  App.jsx                # header + view switching
  components/
    Overview.jsx         # stat tiles + campaign list (5s poll)
    Studio.jsx           # the 4-step flow incl. rule editor
    Detail.jsx           # live funnel, attribution, events (3s poll)
    Funnel.jsx           # the pour bars
  styles.css             # the entire design system
```
