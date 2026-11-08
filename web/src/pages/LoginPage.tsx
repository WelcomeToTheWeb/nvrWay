import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import axios from 'axios';
import client from '../api/client';
import { useAuth } from '../auth/context';

/**
 * The door.
 *
 * What this page draws is decided by the server, not by the bundle. `GET
 * /api/auth/status` answers `{local_login, oidc[], registration}` where
 * `local_login` is `Users.CountLocalLogin() > 0` (api/auth.go AuthStatus), so a
 * deployment seeded without `BOOTSTRAP_ADMIN_PASSWORD` shows no form at all —
 * there is no password to type, and a form that invites one is a door that
 * cannot open.
 *
 * Nothing here is a spinner. While the read is outstanding the page says what
 * it is checking; when it fails it says so and offers to try again. That is the
 * console's behaviour contract, and it matters most on the page someone reaches
 * when everything else is broken.
 */

interface AuthStatus {
  local_login: boolean;
  oidc: { provider: string; name: string; issuer: string }[];
  registration: boolean;
}

type StatusState =
  | { kind: 'checking' }
  | { kind: 'ready'; value: AuthStatus }
  | { kind: 'failed'; reason: string };

function refusalMessage(error: unknown): string {
  if (axios.isAxiosError(error)) {
    const body = error.response?.data as { error?: string } | undefined;
    // The server's own words: "invalid credentials", "this account signs in
    // through its provider". They are already written for a person.
    if (body?.error) return body.error;
    if (error.code === 'ERR_NETWORK') return 'the server could not be reached';
    return 'the sign-in was refused';
  }
  return 'the sign-in was refused';
}

export default function LoginPage() {
  const navigate = useNavigate();
  const { login, isAuthenticated } = useAuth();

  const [status, setStatus] = useState<StatusState>({ kind: 'checking' });
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    setStatus({ kind: 'checking' });
    client
      .get<AuthStatus>('/auth/status')
      .then(({ data }) => setStatus({ kind: 'ready', value: data }))
      .catch((e: unknown) =>
        setStatus({
          kind: 'failed',
          reason:
            (axios.isAxiosError(e) ? (e.response?.data as { error?: string } | undefined)?.error : undefined) ??
            'the server could not be reached',
        }),
      );
  }, []);

  useEffect(load, [load]);

  // A browser that already holds a session has no business being offered the
  // door again — and signing in a second time would rotate a session that was
  // working.
  useEffect(() => {
    if (isAuthenticated) navigate('/live', { replace: true });
  }, [isAuthenticated, navigate]);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(username, password);
      navigate('/live', { replace: true });
    } catch (e: unknown) {
      setError(refusalMessage(e));
      setBusy(false);
    }
  };

  if (isAuthenticated) {
    return <p className="text-text-secondary">You are already signed in.</p>;
  }

  if (status.kind === 'checking') {
    return <p className="text-text-secondary">Checking how this installation allows sign-in…</p>;
  }

  if (status.kind === 'failed') {
    return (
      <main className="max-w-sm p-8">
        <h1 className="text-3xl font-light tracking-cap">Sign in</h1>
        <p className="mt-4 text-status-error">Could not read the sign-in methods.</p>
        <p className="mt-1 text-sm text-text-secondary">{status.reason}</p>
        <button
          type="button"
          onClick={load}
          className="mt-4 border border-rule px-3 py-1 text-sm hover:bg-bg-secondary"
        >
          Try again
        </button>
      </main>
    );
  }

  const { local_login: localLogin, oidc } = status.value;
  const nothingOffersAWayIn = !localLogin && oidc.length === 0;

  return (
    <main className="max-w-sm p-8">
      <h1 className="text-3xl font-light tracking-cap">Sign in</h1>

      {nothingOffersAWayIn && (
        <p className="mt-4 text-status-warning">
          No local account and no identity provider is enabled, so there is no way in.
        </p>
      )}

      {localLogin && (
        <form onSubmit={submit} className="mt-4 flex flex-col gap-3">
          <label className="flex flex-col gap-1 text-sm" htmlFor="login-username">
            <span className="text-xs font-semibold tracking-cap text-text-secondary">Username</span>
            <input
              id="login-username"
              name="username"
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              className="border border-rule bg-bg-card px-2 py-1 text-text-primary"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm" htmlFor="login-password">
            <span className="text-xs font-semibold tracking-cap text-text-secondary">Password</span>
            <input
              id="login-password"
              name="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="border border-rule bg-bg-card px-2 py-1 text-text-primary"
            />
          </label>

          {error && <p className="text-sm text-status-error">{error}</p>}

          <button
            type="submit"
            disabled={busy}
            className="border border-accent bg-accent px-3 py-1 text-sm text-bg-card disabled:opacity-60"
          >
            {busy ? 'Signing in…' : 'Sign in'}
          </button>
        </form>
      )}

      {oidc.length > 0 && (
        <div className="mt-6 flex flex-col gap-2">
          {oidc.map((p) => (
            <a
              key={p.provider}
              href={`/api/auth/sso/${encodeURIComponent(p.provider)}/authorize`}
              className="border border-rule px-3 py-2 text-sm hover:bg-bg-secondary"
            >
              <span className="block">Sign in with {p.name || p.provider}</span>
              <span className="block text-xs text-text-muted">{p.issuer}</span>
            </a>
          ))}
        </div>
      )}
    </main>
  );
}
