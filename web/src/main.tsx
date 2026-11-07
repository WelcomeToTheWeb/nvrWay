import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';

import App from './App';
import { AuthProvider } from './auth/context';
import './index.css';

// The console's mount point. The router, the auth provider, and nothing else:
// every decision about who sees which page lives in `App` (the route table)
// and in the pages themselves (the role refusals), so this file stays a
// wiring diagram. `BrowserRouter` is here rather than inside `App` because
// the suites mount `App` under a `MemoryRouter` — the shell must not assume
// which router history it was given.
ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    <BrowserRouter>
      <AuthProvider>
        <App />
      </AuthProvider>
    </BrowserRouter>
  </React.StrictMode>,
);
