/**
 * The ledger page: who did what, to what, and what the server answered.
 *
 * The rows have existed since S2-08 — every mutating act and every refusal is
 * a row. This page is the answer to the question an operator actually arrives
 * with: *someone removed a camera and I want a name, a time, and whether the
 * server let them.* The refusals are the rows that settle arguments, so they
 * render with the same weight as the successes, in the server's own words —
 * `refused` is a stored column (`models/audit.go`), not an adjective invented
 * here.
 *
 * The filters live in the address bar (`/audit?actor=…&outcome=…`), the same
 * rule the event wall follows: a filtered ledger is pasteable, survives a
 * reload, and answers the same question for the next person who opens the
 * link. The filter names are the server's query parameters, so the URL is
 * also the API call.
 *
 * Admin-only, and the refusal is drawn before any request: the ledger carries
 * usernames, source IPs and the shape of every denial, which is exactly what
 * a lower role has no business reading — and the server (`RequireRole(admin)`
 * on the route) would say so on every one of the four reads a naive page
 * would fire.
 */
import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router-dom';

import client, { serverWords } from '../api/client';
import {
  AUDIT_FILTER_KEYS,
  AUDIT_OUTCOMES,
  auditFiltersFromParams,
  auditFiltersToParams,
  type AuditFilters,
  type AuditListResponse,
  type AuditView,
} from '../api/audit';
import { useAuth } from '../auth/context';

const PAGE_SIZE = 50;

export default function AuditPage() {
  const { hasRole } = useAuth();
  const [params, setParams] = useSearchParams();
  const filters = auditFiltersFromParams(params);
  // The page offset lives in the URL beside the filters, so a filtered ledger
  // three pages deep is still one pasteable link — and so a filter change,
  // which rewrites the params without an offset, resets the page for free.
  const rawOffset = Number(params.get('offset') ?? '0');
  const offset = Number.isFinite(rawOffset) && rawOffset > 0 ? Math.floor(rawOffset) : 0;
  const [page, setPage] = useState<AuditListResponse | null>(null);
  const [failed, setFailed] = useState<string | null>(null);

  useEffect(() => {
    if (!hasRole('admin')) return;
    const query = new URLSearchParams(params);
    query.set('limit', String(PAGE_SIZE));
    query.set('offset', String(offset));
    setPage(null);
    setFailed(null);
    void client
      .get<AuditListResponse>(`/audit?${query.toString()}`)
      .then((response) => setPage(response.data))
      .catch((error: unknown) => setFailed(serverWords(error, 'the ledger is unreadable')));
  }, [hasRole, offset, params]);

  if (!hasRole('admin')) {
    return (
      <main className="p-6">
        <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Audit</h1>
        <p className="mt-4 text-sm text-text-secondary">
          The ledger belongs to an admin. It carries every name that knocked and every door the
          server refused — which is not a thing to hand a role that did not write it.
        </p>
      </main>
    );
  }

  const applyFilters = (draft: AuditFilters) => {
    // Rewriting the params without an offset is the page reset: page three of
    // a filter that no longer applies is not a page, it is a bug wearing old
    // data.
    setParams(auditFiltersToParams(draft));
  };

  const goOffset = (next: number) => {
    const nextParams = new URLSearchParams(params);
    if (next > 0) nextParams.set('offset', String(next));
    else nextParams.delete('offset');
    setParams(nextParams);
  };

  const rows = page?.audits ?? [];
  const activeFilters = AUDIT_FILTER_KEYS.filter((key) => filters[key]);

  return (
    <main className="p-6">
      <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Audit</h1>

      <FilterBar filters={filters} onApply={applyFilters} />

      {activeFilters.length > 0 ? (
        <p className="mt-2 font-mono text-xs text-text-muted">
          filtered by {activeFilters.map((key) => `${key}=${filters[key]}`).join(' · ')}
        </p>
      ) : null}

      {failed ? <p className="mt-4 text-sm text-status-error">{failed}</p> : null}
      {!failed && !page ? (
        <p className="mt-4 text-sm text-text-secondary">Reading the ledger.</p>
      ) : null}
      {page && rows.length === 0 ? (
        <p className="mt-4 text-sm text-text-secondary">
          {activeFilters.length > 0
            ? 'Nothing in the ledger matches. A filter that matches nothing is an answer, not a failure.'
            : 'The ledger is empty. Nothing has been done yet, and nothing has been refused.'}
        </p>
      ) : null}

      {rows.length > 0 ? (
        <ul className="mt-4 border border-rule" aria-label="Ledger rows">
          {rows.map((row) => (
            <AuditRow key={row.id} row={row} />
          ))}
        </ul>
      ) : null}

      {page ? (
        <div className="mt-3 flex items-center gap-3">
          {offset > 0 ? (
            <button
              type="button"
              onClick={() => goOffset(Math.max(0, offset - PAGE_SIZE))}
              className="font-mono text-xs text-text-secondary underline"
            >
              newer
            </button>
          ) : null}
          {page.truncated ? (
            <button
              type="button"
              onClick={() => goOffset(offset + PAGE_SIZE)}
              className="font-mono text-xs text-text-secondary underline"
            >
              older
            </button>
          ) : null}
          <span className="font-mono text-xs text-text-muted">
            {offset + 1}–{offset + page.count} newest-first
          </span>
        </div>
      ) : null}
    </main>
  );
}

/** The filter row. It submits rather than firing on every keystroke: the URL
 * is the state, and a request per character is a denial-of-self on a ledger
 * that can hold a year of rows. */
function FilterBar({
  filters,
  onApply,
}: {
  filters: AuditFilters;
  onApply(filters: AuditFilters): void;
}) {
  const [draft, setDraft] = useState<AuditFilters>(filters);

  const set = (key: keyof AuditFilters, value: string) =>
    setDraft((current) => ({ ...current, [key]: value }));

  return (
    <form
      className="mt-4 flex flex-wrap items-end gap-3"
      onSubmit={(event) => {
        event.preventDefault();
        onApply(draft);
      }}
    >
      {(['actor', 'action', 'target_id'] as const).map((key) => (
        <label key={key} className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">{key}</span>
          <input
            value={draft[key] ?? ''}
            onChange={(event) => set(key, event.target.value)}
            className="mt-0.5 block border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
      ))}
      <label className="block">
        <span className="font-mono text-[10px] uppercase text-text-muted">outcome</span>
        <select
          value={draft.outcome ?? ''}
          onChange={(event) => set('outcome', event.target.value)}
          className="mt-0.5 block border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
        >
          <option value="">all</option>
          {AUDIT_OUTCOMES.map((outcome) => (
            <option key={outcome} value={outcome}>
              {outcome}
            </option>
          ))}
        </select>
      </label>
      <button
        type="submit"
        className="border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent"
      >
        filter
      </button>
    </form>
  );
}

/** One act, in the ledger's own words. The actor line keeps roles and
 * provider because both outlive the account: providers get deleted and roles
 * get changed, and "who removed a camera" must keep its answer. */
function AuditRow({ row }: { row: AuditView }) {
  const outcomeWords =
    row.outcome === 'ok'
      ? 'text-status-online'
      : row.outcome === 'refused'
        ? 'text-status-warning'
        : row.outcome === 'error'
          ? 'text-status-error'
          : 'text-text-muted';

  return (
    <li className="border-b border-rule px-3 py-2 last:border-b-0">
      <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        <span className="font-mono text-xs text-text-muted">{row.created_at}</span>
        <span className="font-cap text-sm uppercase tracking-cap text-text-primary">
          {row.actor_username || '(no account)'}
        </span>
        <span className="font-mono text-xs text-text-muted">
          {[row.actor_roles, row.actor_provider].filter(Boolean).join(' · ')}
        </span>
        <span className="font-mono text-xs text-text-secondary">{row.action}</span>
        {row.target_type ? (
          <span className="font-mono text-xs text-text-secondary">
            → {row.target_type}
            {row.target_id ? ` ${row.target_id}` : ''}
          </span>
        ) : null}
        <span className={`font-mono text-xs ${outcomeWords}`}>{row.outcome}</span>
      </div>
      {row.details ? (
        <p className="mt-1 font-mono text-xs text-text-muted">{JSON.stringify(row.details)}</p>
      ) : null}
      {row.source_ip ? (
        <p className="mt-0.5 font-mono text-[10px] text-text-muted">from {row.source_ip}</p>
      ) : null}
    </li>
  );
}
