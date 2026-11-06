import React from 'react';
import ReactDOM from 'react-dom/client';
import './index.css';

// The scaffold's mount point. index.html names this file as the entry, so the
// toolchain has something real to typecheck, build and serve — a scaffold whose
// entry does not exist is a config file, not a console.
//
// It is a placeholder on purpose, and it says so. The router arrives with
// S8-09 (routes + role-aware nav); until then there is nothing here to
// navigate to, and a console that renders an empty frame would be the spinner
// the plan forbids.
function Scaffold() {
  return (
    <main className="min-h-screen p-8">
      <h1 className="font-cap text-2xl tracking-cap uppercase text-text-primary">
        nvrWay
      </h1>
      <p className="mt-2 text-sm text-text-secondary">
        Console scaffold. No pages are mounted yet — the router lands with the
        routes card (S8-09).
      </p>
    </main>
  );
}

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    <Scaffold />
  </React.StrictMode>,
);
