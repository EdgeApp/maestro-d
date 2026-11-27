import { createRoot } from 'react-dom/client'
import { Dashboard } from './dashboard.js'

export const root = createRoot(document.getElementById('root')!)
root.render(<Dashboard />)
