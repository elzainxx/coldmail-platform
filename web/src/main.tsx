import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App.tsx'
import { consumeAuthHash } from './auth/hash.ts'
import './index.css'
import { initTheme } from './shell/theme.ts'

const rootElement = document.getElementById('root')
if (!rootElement) {
  throw new Error('Root element #root not found in index.html')
}

// Before anything else: take a sign-in link's tokens out of the address bar (they are then in memory only), and put the chosen theme on the page.
const landing = consumeAuthHash()
initTheme()

createRoot(rootElement).render(
  <StrictMode>
    <App landing={landing} />
  </StrictMode>,
)
