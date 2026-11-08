/**
 * The shell: the route table, the role-aware nav, and the live feed's owner.
 *
 * Three obligations land here from the cards that built the pages before the
 * shell existed, and each is a route-table fact rather than a style choice:
 *
 * **`EventsPage` is mounted at both `/events` and `/events/:id`, and
 * `CamerasPage` at both `/cameras` and `/cameras/:id`.** Both pages read
 * `useMatch` and render the drawer / the editor over the list they already
 * have — so a drawer or an editor is a URL, deep-linkable and reload-proof.
 * A route table that sends `/events/:id` elsewhere loses the drawer with no
 * error anywhere, which is why the pairing is asserted in the suite and not
 * left to convention.
 *
 * **The shell owns the feed's lifecycle.** `LivePage` calls `connect()` and
 * deliberately never calls `disconnect()` — one socket per tab, and a tile
 * unmounting must not drop the feed the next page reads. So the shell opens
 * the feed once for a signed-in session and closes it when the session ends
 * (logout) or the shell itself unmounts. `connect()` is idempotent, so the
 * wall calling it again costs nothing.
 *
 * **The stale/refused banner has its home here.** The store already knows the
 * difference between "the socket dropped, retrying" and "the socket has
 * refused three times — check your configuration"; only the shell can say it
 * somewhere that is not one page deep.
 *
 * The nav is role-aware because the server is role-gated: a viewer who sees a
 * Settings tab learns the answer 403, and a console that invites a refusal is
 * a console that lies about its own doors. Hiding is courtesy; the server's
 * `RequireRole` is the gate — the pages still refuse on their own, so a
 * deep link to `/settings` meets the same sentence as the hidden tab.
 */
import { useEffect, useState, type ReactNode } from 'react';
import { NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom';

import { useAuth } from './auth/context';
import { useRealtime } from './stores/realtime';
import AuditPage from './pages/AuditPage';
import AuthCallbackPage from './pages/AuthCallbackPage';
import CamerasPage from './pages/CamerasPage';
import EventsPage from './pages/EventsPage';
import LivePage from './pages/LivePage';
import LoginPage from './pages/LoginPage';
import SettingsPage from './pages/SettingsPage';

/** The routes a signed-out browser may see. Everything else redirects, and
 * the redirect is a `Navigate` rather than a render so the address bar tells
 * the truth about where the session went. */
const PUBLIC_PATHS = ['/login', '/auth/callback'];

export default function App() {
  const { isAuthenticated, hasRole, logout } = useAuth();
  const location = useLocation();

  // The feed's owner. Signed in: open (idempotent — the wall may ask too).
  // Signed out or unmounted: close, so a logout does not leave a socket
  // streaming to a browser that just gave up its tokens.
  useEffect(() => {
    if (isAuthenticated) {
      useRealtime.getState().connect();
    } else {
      useRealtime.getState().disconnect();
    }
  }, [isAuthenticated]);

  useEffect(() => () => useRealtime.getState().disconnect(), []);

  if (!isAuthenticated && !PUBLIC_PATHS.includes(location.pathname)) {
    return <Navigate to="/login" replace />;
  }

  return (
    <div className="min-h-screen">
      {isAuthenticated ? (
        <header className="flex flex-wrap items-baseline gap-x-8 gap-y-2 border-b border-rule bg-bg-card px-6 py-4">
          <span className="font-light text-4xl tracking-tight text-text-primary">nvrWay</span>
          {/* On the phone the actions ride the wordmark row and the nav takes
              the full second row — five tiles fit one line at phone measure,
              so the wall starts higher. On a desk it stays one line. */}
          <div className="ml-auto flex items-baseline gap-4 md:order-3">
            <ThemeToggle />
            <button
              type="button"
              onClick={() => void logout()}
              className="px-3 py-2 text-sm font-semibold tracking-cap text-text-secondary hover:bg-bg-secondary hover:text-text-primary"
            >
              sign out
            </button>
          </div>
          <nav className="flex w-full flex-wrap gap-x-1 gap-y-1 md:order-2 md:w-auto" aria-label="Console">
            <NavTab to="/live">Live</NavTab>
            <NavTab to="/events">Events</NavTab>
            <NavTab to="/cameras">Cameras</NavTab>
            {hasRole('admin') ? <NavTab to="/settings">Settings</NavTab> : null}
            {hasRole('admin') ? <NavTab to="/audit">Audit</NavTab> : null}
          </nav>
        </header>
      ) : null}

      {isAuthenticated ? <FeedBanner /> : null}

      <Routes>
        <Route path="/login" element={isAuthenticated ? <Navigate to="/live" replace /> : <LoginPage />} />
        <Route path="/auth/callback" element={<AuthCallbackPage />} />
        <Route path="/live" element={<LivePage />} />
        {/* The drawer is a URL: the same page answers both, and reads
            `useMatch('/events/:id')` to decide whether the drawer is open. */}
        <Route path="/events" element={<EventsPage />} />
        <Route path="/events/:id" element={<EventsPage />} />
        {/* The editor is a URL too, for the same reason. */}
        <Route path="/cameras" element={<CamerasPage />} />
        <Route path="/cameras/:id" element={<CamerasPage />} />
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="/audit" element={<AuditPage />} />
        <Route path="*" element={<Navigate to="/live" replace />} />
      </Routes>
    </div>
  );
}

/** Nav tabs are tiles: the active route sits in the live field, cyan ink on
 * cyan, the way a destination on the concourse is lit when you are on it. */
function NavTab({ to, children }: { to: string; children: ReactNode }) {
  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        `px-2 py-1.5 text-xs font-semibold tracking-cap md:px-3 md:py-2 md:text-sm ${
          isActive
            ? 'bg-accent-light text-[var(--field-cyan-ink)]'
            : 'text-text-secondary hover:bg-bg-secondary hover:text-text-primary'
        }`
      }
    >
      {children}
    </NavLink>
  );
}

/** Day and night, honestly equal: the button names the world it will switch
 * TO, not the one you are in, so the label is the action. */
function ThemeToggle() {
  const [dark, setDark] = useState(
    () => typeof document !== 'undefined' && document.documentElement.classList.contains('dark'),
  );
  return (
    <button
      type="button"
      aria-label={`Switch to ${dark ? 'day' : 'night'} theme`}
      onClick={() => {
        const next = !dark;
        setDark(next);
        document.documentElement.classList.toggle('dark', next);
        try {
          localStorage.setItem('nvr-theme', next ? 'night' : 'day');
        } catch {
          /* a private-mode browser keeps the choice for this tab only */
        }
      }}
      className="border border-rule-strong bg-bg-secondary px-3 py-2 text-sm font-semibold tracking-cap text-text-primary"
    >
      {dark ? 'day' : 'night'}
    </button>
  );
}

/** The feed's own state, where the operator cannot miss it. The store has
 * already decided between "dropped, retrying" and "refused — go look at
 * something"; the banner repeats its sentence rather than inventing one, and
 * says nothing when the feed is current, because a banner that is always on
 * is a banner nobody reads. */
function FeedBanner() {
  const status = useRealtime((state) => state.status);
  const reason = useRealtime((state) => state.reason);

  // The degraded field: amber for "retrying", red for "go look". The strip is
  // the field itself — a Metro console states trouble as a colour band, not
  // as a line of console log.
  if (status === 'stale') {
    return (
      <p
        role="status"
        className="px-6 py-1.5 text-sm font-semibold"
        style={{ background: 'var(--field-amber)', color: 'var(--field-amber-ink)' }}
      >
        live feed: {reason ?? 'the feed dropped; retrying'}
      </p>
    );
  }
  if (status === 'refused') {
    return (
      <p
        role="alert"
        className="px-6 py-1.5 text-sm font-semibold"
        style={{ background: 'var(--field-red)', color: 'var(--field-red-ink)' }}
      >
        live feed: {reason ?? 'the feed has stopped opening'}
      </p>
    );
  }
  return null;
}
