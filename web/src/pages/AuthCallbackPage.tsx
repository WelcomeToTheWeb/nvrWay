import { useEffect, useRef, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { useAuth } from '../auth/context';

/**
 * The other half of the OIDC handoff.
 *
 * The server returns here with a **fragment**, never a query
 * (`api/sso_routes.go`): `#access_token=…&refresh_token=…` on success,
 * `#error=<reason>&provider=<name>&error_description=<detail>` on a refusal,
 * plus `&pending_identity=<id>` when an identity is waiting for a decision. A
 * fragment is not sent to any server in a request, so the tokens never reach a
 * proxy log or a `Referer` — and the page has to scrub it, because a token left
 * in the address bar is a token in the browser history, on the screen, and in
 * anything that reads a URL.
 *
 * The refusal reasons are the contract's own vocabulary (S2-04). They are
 * rendered as sentences because a pending identity, a domain the operator did
 * not list, and an IdP that asserted two subjects in one login are three
 * different screens with three different fixes — and the raw code is printed
 * beside them, because a fact this page cannot interpret is still a fact the
 * operator can quote.
 */

type Handoff =
  | { kind: 'adopting'; access: string; refresh: string }
  | { kind: 'refused'; reason: string; provider: string; description: string; pendingIdentity: string | null }
  | { kind: 'incomplete' }
  | { kind: 'empty' };

/** The reason strings the server can send, in the operator's words. */
const REASONS: Record<string, string> = {
  provider_disabled: 'This sign-in provider is switched off.',
  code_exchange_failed: 'The provider did not complete the exchange with the server.',
  missing_id_token: 'The provider returned no identity token.',
  id_token_verification_failed: 'The identity token could not be verified against the provider’s keys.',
  nonce_mismatch: 'This login did not match the one the browser started.',
  id_token_userinfo_disagreement: 'The provider asserted two different accounts in one login.',
  subject_missing: 'The provider returned no account identifier.',
  no_email_claim: 'The provider did not say which email this account owns.',
  domain_not_allowed: 'This email’s domain is not allowed by the operator’s rules.',
  email_unverified: 'The provider says this email is not verified.',
  identity_pending: 'An administrator has to approve this identity before it can sign in.',
  account_pending: 'This account is waiting for an administrator’s decision.',
  sso_login_failed: 'The sign-in failed.',
};

function parseFragment(hash: string): Handoff {
  const params = new URLSearchParams(hash.startsWith('#') ? hash.slice(1) : hash);
  const error = params.get('error');
  if (error) {
    return {
      kind: 'refused',
      reason: error,
      provider: params.get('provider') ?? '',
      description: params.get('error_description') ?? '',
      pendingIdentity: params.get('pending_identity'),
    };
  }
  const access = params.get('access_token');
  const refresh = params.get('refresh_token');
  if (access && refresh) return { kind: 'adopting', access, refresh };
  // One token and not the other is not a session this console can keep: it
  // would die at the 30-minute mark with nothing to renew with.
  if (access || refresh) return { kind: 'incomplete' };
  return { kind: 'empty' };
}

export default function AuthCallbackPage() {
  const navigate = useNavigate();
  const { adoptTokens } = useAuth();
  const [handoff, setHandoff] = useState<Handoff | null>(null);

  // React 18 StrictMode mounts, unmounts and remounts the effect on the same
  // instance. The first pass scrubs the fragment, so a second pass would read
  // an empty hash and report that nothing arrived — a page that passes its test
  // and fails in development. The ref outlives the simulated remount; the
  // handoff is read once.
  const handled = useRef(false);

  useEffect(() => {
    if (handled.current) return;
    handled.current = true;

    const result = parseFragment(window.location.hash);
    if (result.kind !== 'adopting') {
      setHandoff(result);
      return;
    }

    const { pathname, search } = window.location;
    window.history.replaceState(null, '', pathname + search);

    adoptTokens(result.access, result.refresh).then(() => navigate('/live', { replace: true }));
  }, [adoptTokens, navigate]);

  if (handoff === null || handoff.kind === 'adopting') {
    return <p className="p-8 text-text-secondary">Signing you in…</p>;
  }

  return (
    <main className="max-w-md p-8">
      <h1 className="font-cap text-2xl tracking-cap uppercase">Sign in</h1>

      {handoff.kind === 'refused' && (
        <>
          <p className="mt-4 text-status-error">
            {REASONS[handoff.reason] ?? 'The sign-in was refused for a reason this page does not know.'}
          </p>
          <p className="mt-2 text-sm text-text-secondary">
            Reason: <code className="font-mono">{handoff.reason}</code>
            {handoff.provider && (
              <>
                {' '}
                from provider <code className="font-mono">{handoff.provider}</code>
              </>
            )}
          </p>
          {handoff.description && <p className="mt-1 text-sm text-text-muted">{handoff.description}</p>}
          {handoff.pendingIdentity && (
            <p className="mt-3 text-sm">
              Pending identity: <code className="font-mono">{handoff.pendingIdentity}</code>. An
              administrator decides from Settings — nothing is merged automatically.
            </p>
          )}
        </>
      )}

      {handoff.kind === 'incomplete' && (
        <p className="mt-4 text-status-error">
          The sign-in handoff was incomplete: it carried a token but not both halves, and a session
          with no refresh token cannot outlive its first half hour.
        </p>
      )}

      {handoff.kind === 'empty' && (
        <p className="mt-4 text-status-warning">
          No sign-in handoff arrived. Nothing was returned to this page, so nothing was stored.
        </p>
      )}

      <Link to="/login" className="mt-6 inline-block border border-rule px-3 py-1 text-sm hover:bg-bg-secondary">
        Sign in again
      </Link>
    </main>
  );
}
