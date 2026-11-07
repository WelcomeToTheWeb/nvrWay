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
import { useEffect, type ReactNode } from 'react';
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
        <header className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-rule bg-bg-card px-6 py-3">
          <span className="font-cap text-lg uppercase tracking-cap text-text-primary">nvrWay</span>
          <nav className="flex flex-wrap gap-x-4 gap-y-1" aria-label="Console">
            <NavTab to="/live">Live</NavTab>
            <NavTab to="/events">Events</NavTab>
            <NavTab to="/cameras">Cameras</NavTab>
            {hasRole('admin') ? <NavTab to="/settings">Settings</NavTab> : null}
            {hasRole('admin') ? <NavTab to="/audit">Audit</NavTab> : null}
          </nav>
          <button
            type="button"
            onClick={() => void logout()}
            className="ml-auto font-mono text-xs text-text-secondary underline"
          >
            sign out
          </button>
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

function NavTab({ to, children }: { to: string; children: ReactNode }) {
  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        `font-cap text-sm uppercase tracking-cap ${
          isActive ? 'text-accent' : 'text-text-secondary hover:text-text-primary'
        }`
      }
    >
      {children}
    </NavLink>
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

  if (status === 'stale') {
    return (
      <p role="status" className="border-b border-rule bg-bg-card px-6 py-1.5 font-mono text-xs text-status-warning">
        live feed: {reason ?? 'the feed dropped; retrying'}
      </p>
    );
  }
  if (status === 'refused') {
    return (
      <p role="alert" className="border-b border-rule bg-bg-card px-6 py-1.5 font-mono text-xs text-status-error">
        live feed: {reason ?? 'the feed has stopped opening'}
      </p>
    );
  }
  return null;
}
