/**
 * The settings page: the destinations, the doors, the hooks, and the windows.
 *
 * Four tabs, one contract: **every mutation shows the server's state, not a
 * spinner.** The card names it outright, and it shapes everything below. A
 * connection test renders the tier's named answer (`discovery_unreachable` and
 * `bucket missing` are different fixes, so they are different words on
 * screen). A save renders the server's view of the row it wrote. A refusal
 * renders the server's sentence (`serverWords`), not a paraphrase. And a
 * secret renders as *set* or *unset* — the API never sends the value, so this
 * page could not render one if it tried, and the write payloads omit an
 * untouched secret because on this API omitted means keep and empty means
 * clear.
 *
 * The tabs are a URL (`/settings?tab=…`) for the reason the event drawer and
 * the camera editor are: a storage tier that refuses to connect at 2am is
 * worth pasting into a chat, and a reload should land on the tab you were
 * arguing about.
 *
 * The whole page is admin-only, and it says so to a viewer rather than
 * drawing four doors that all answer 403. The server enforces the same gate
 * (`RequireRole` on the `admin` group in `api/handlers.go`); a console that
 * rendered the doors anyway would be a console that invites a refusal.
 *
 * The retention tab has no Save button, and that is the decision, not an
 * omission: retention is authored by the environment, the sweeper honours it,
 * and a second author in a settings row would fork the policy into two truths.
 * The tab reads the windows the server will sweep by — including the
 * un-uploaded protection window, computed by the sweeper's own formula — and
 * names the env vars that change them.
 */
import { useCallback, useEffect, useState } from 'react';
import { useSearchParams } from 'react-router-dom';

import client, { serverDetail, serverWords } from '../api/client';
import {
  SECRET_FIELDS,
  SECRET_SET_FLAGS,
  STORAGE_KINDS,
  WEBHOOK_TRIGGERS,
  storageWrite,
  type OIDCDiscoveryRefusal,
  type OIDCProviderView,
  type OIDCProviderWrite,
  type QueueCounts,
  type RetentionView,
  type StorageBackendView,
  type StorageBackendWrite,
  type StorageKind,
  type StorageQueueResponse,
  type StorageTestResult,
  type WebhookDeliveryView,
  type WebhookView,
  type WebhookWrite,
} from '../api/settings';
import { type MediaSettings } from '../api/events';
import { useAuth } from '../auth/context';
import { useRealtime } from '../stores/realtime';

type Tab = 'storage' | 'access' | 'webhooks' | 'retention';

const TABS: { id: Tab; label: string }[] = [
  { id: 'storage', label: 'Storage' },
  { id: 'access', label: 'Access' },
  { id: 'webhooks', label: 'Webhooks' },
  { id: 'retention', label: 'Retention' },
];

/** The reading state every tab shares: a read that failed says so with the
 * server's sentence, and a read that is empty says *empty*, which is a
 * different fact about the installation. */
type ReadState<T> =
  | { kind: 'reading' }
  | { kind: 'failed'; reason: string }
  | { kind: 'ready'; rows: T };

function readWords(error: unknown, fallback: string): string {
  return serverWords(error, fallback);
}

export default function SettingsPage() {
  const { hasRole } = useAuth();
  const [params, setParams] = useSearchParams();
  const tab: Tab = (TABS.find((t) => t.id === params.get('tab'))?.id ?? 'storage') as Tab;

  if (!hasRole('admin')) {
    return (
      <main className="p-6">
        <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Settings</h1>
        <p className="mt-4 text-sm text-text-secondary">
          Settings belong to an admin. Your role can watch the wall and read the events; the
          destinations, the doors and the windows are not yours to change.
        </p>
      </main>
    );
  }

  return (
    <main className="p-6">
      <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Settings</h1>

      <nav className="mt-4 flex gap-1 border-b border-rule" aria-label="Settings sections">
        {TABS.map((entry) => (
          <button
            key={entry.id}
            type="button"
            onClick={() => setParams({ tab: entry.id })}
            aria-current={tab === entry.id ? 'page' : undefined}
            className={`px-3 py-1.5 font-cap text-xs uppercase tracking-cap ${
              tab === entry.id
                ? 'border border-b-0 border-rule bg-bg-card text-text-primary'
                : 'text-text-secondary hover:text-text-primary'
            }`}
          >
            {entry.label}
          </button>
        ))}
      </nav>

      {tab === 'storage' ? <StorageTab /> : null}
      {tab === 'access' ? <AccessTab /> : null}
      {tab === 'webhooks' ? <WebhooksTab /> : null}
      {tab === 'retention' ? <RetentionTab /> : null}
    </main>
  );
}

// ---------------------------------------------------------------------------
// Storage: the destinations, the test, and the queue.
// ---------------------------------------------------------------------------

/** The queue's four numbers, from the REST read and then from the feed. */
function QueueStrip({ rest }: { rest: QueueCounts | null }) {
  const report = useRealtime((state) => state.storageQueue);
  const [live, setLive] = useState<QueueCounts | null>(null);

  // The sweeper's `storage.queue` message carries the queue as the sweep left
  // it (`Report.Queue`). When one arrives it is the freshest answer the page
  // will get without a reload, so it wins over the read from mount time —
  // until the tab reloads and a fresh read wins again.
  useEffect(() => {
    const queue = report?.queue as QueueCounts | undefined;
    if (queue) setLive(queue);
  }, [report]);

  const counts = live ?? rest;
  const forceDeleted = typeof report?.force_deleted === 'number' ? report.force_deleted : 0;
  const problems = Array.isArray(report?.problems) ? (report?.problems as string[]) : [];

  return (
    <section className="mt-4 border border-rule bg-bg-card p-3" aria-label="Upload queue">
      <h2 className="font-cap text-sm uppercase tracking-cap text-text-primary">Upload queue</h2>
      {counts ? (
        <div className="mt-2 flex flex-wrap gap-x-6 gap-y-1 font-mono text-xs text-text-secondary">
          <span>pending {counts.pending}</span>
          <span>uploading {counts.uploading}</span>
          <span className={counts.failed > 0 ? 'text-status-error' : undefined}>
            failed {counts.failed}
          </span>
          <span>uploaded {counts.uploaded}</span>
        </div>
      ) : (
        <p className="mt-2 font-mono text-xs text-text-muted">The queue counts are unreadable.</p>
      )}
      {counts?.last_error ? (
        <p className="mt-1 font-mono text-xs text-status-error">last error: {counts.last_error}</p>
      ) : null}
      {forceDeleted > 0 ? (
        <p className="mt-1 font-mono text-xs text-status-error">
          force deleted: {forceDeleted} — an upload outage outlasted the protection window
        </p>
      ) : null}
      {problems.length > 0 ? (
        <ul className="mt-1 font-mono text-xs text-status-warning">
          {problems.map((problem, index) => (
            <li key={index}>{problem}</li>
          ))}
        </ul>
      ) : null}
    </section>
  );
}

function StorageTab() {
  const [page, setPage] = useState<ReadState<StorageBackendView[]>>({ kind: 'reading' });
  const [queue, setQueue] = useState<QueueCounts | null>(null);
  const [tests, setTests] = useState<Record<string, StorageTestResult | 'testing'>>({});
  const [editing, setEditing] = useState<string | 'new' | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  const load = useCallback(() => {
    setPage({ kind: 'reading' });
    void Promise.all([
      client.get<{ backends: StorageBackendView[] }>('/storage/backends'),
      client.get<StorageQueueResponse>('/storage/queue'),
    ])
      .then(([backends, queueResponse]) => {
        setPage({ kind: 'ready', rows: backends.data.backends ?? [] });
        setQueue(queueResponse.data.counts ?? null);
      })
      .catch((error: unknown) =>
        setPage({ kind: 'failed', reason: readWords(error, 'the storage backends are unreadable') }),
      );
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const test = useCallback(async (backend: StorageBackendView) => {
    setTests((current) => ({ ...current, [backend.id]: 'testing' }));
    try {
      const { data } = await client.post<StorageTestResult>(`/storage/backends/${backend.id}/test`);
      setTests((current) => ({ ...current, [backend.id]: data }));
    } catch (error) {
      setTests((current) => ({
        ...current,
        [backend.id]: {
          ok: false,
          error: 'connection_failed',
          reason: readWords(error, 'the test request itself failed'),
        },
      }));
    }
  }, []);

  const setEnabled = useCallback(
    async (backend: StorageBackendView, enabled: boolean) => {
      try {
        await client.put(`/storage/backends/${backend.id}/enabled`, { enabled });
        load();
      } catch (error) {
        setMessage(readWords(error, 'the enabled flag could not be written'));
      }
    },
    [load],
  );

  const makeDefault = useCallback(
    async (backend: StorageBackendView) => {
      try {
        await client.put(`/storage/backends/${backend.id}/default`);
        load();
      } catch (error) {
        setMessage(readWords(error, 'the default flag could not be moved'));
      }
    },
    [load],
  );

  const remove = useCallback(
    async (backend: StorageBackendView) => {
      try {
        const { data } = await client.delete<{ deleted: string; orphaned_jobs: number }>(
          `/storage/backends/${backend.id}`,
        );
        // The queue rows that pointed at it stay, and the count is the answer
        // to "why is the queue not moving" — the page says it, not the logs.
        setMessage(
          `deleted ${data.deleted}; ${data.orphaned_jobs} queue row(s) pointed at it and stay`,
        );
        load();
      } catch (error) {
        setMessage(readWords(error, 'the backend could not be deleted'));
      }
    },
    [load],
  );

  const backends = page.kind === 'ready' ? page.rows : [];
  const editingBackend = editing && editing !== 'new' ? backends.find((b) => b.id === editing) : null;

  return (
    <section className="mt-4">
      <QueueStrip rest={queue} />

      {page.kind === 'reading' ? (
        <p className="mt-4 text-sm text-text-secondary">Reading the storage backends.</p>
      ) : null}
      {page.kind === 'failed' ? (
        <p className="mt-4 text-sm text-status-error">{page.reason}</p>
      ) : null}
      {page.kind === 'ready' && backends.length === 0 ? (
        <p className="mt-4 text-sm text-text-secondary">
          No storage backends yet. Everything stays on this machine until you add one.
        </p>
      ) : null}

      {message ? <p className="mt-3 font-mono text-xs text-status-warning">{message}</p> : null}

      {page.kind === 'ready' && backends.length > 0 ? (
        <ul className="mt-4 border border-rule">
          {backends.map((backend) => (
            <StorageRow
              key={backend.id}
              backend={backend}
              test={tests[backend.id]}
              onTest={() => void test(backend)}
              onToggle={(enabled) => void setEnabled(backend, enabled)}
              onDefault={() => void makeDefault(backend)}
              onDelete={() => void remove(backend)}
              onEdit={() => setEditing(backend.id)}
            />
          ))}
        </ul>
      ) : null}

      {editing === 'new' ? (
        <BackendForm
          backend={null}
          onDone={() => {
            setEditing(null);
            load();
          }}
          onCancel={() => setEditing(null)}
        />
      ) : null}
      {editingBackend ? (
        <BackendForm
          backend={editingBackend}
          onDone={() => {
            setEditing(null);
            load();
          }}
          onCancel={() => setEditing(null)}
        />
      ) : null}

      {editing === null ? (
        <button
          type="button"
          onClick={() => setEditing('new')}
          className="mt-4 border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent"
        >
          add a backend
        </button>
      ) : null}
    </section>
  );
}

/** One destination. The address is drawn per kind because a `fs` row has no
 * endpoint and an s3 row's address is not complete without its bucket — the
 * same presence rules `storageKindProblem` enforces on the write. */
function StorageRow({
  backend,
  test,
  onTest,
  onToggle,
  onDefault,
  onDelete,
  onEdit,
}: {
  backend: StorageBackendView;
  test?: StorageTestResult | 'testing';
  onTest(): void;
  onToggle(enabled: boolean): void;
  onDefault(): void;
  onDelete(): void;
  onEdit(): void;
}) {
  const address =
    backend.kind === 'fs'
      ? backend.root_path
      : backend.kind === 's3'
        ? `${backend.endpoint}/${backend.bucket}`
        : backend.endpoint;

  return (
    <li className="border-b border-rule px-3 py-2 last:border-b-0">
      <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        <span className="font-cap text-sm uppercase tracking-cap text-text-primary">
          {backend.name}
        </span>
        <span className="font-mono text-xs text-text-muted">{backend.kind}</span>
        <span className="font-mono text-xs text-text-secondary">{address}</span>
        {backend.is_default ? (
          <span className="font-mono text-xs text-accent">default</span>
        ) : null}
        {!backend.enabled ? (
          <span className="font-mono text-xs text-status-warning">disabled</span>
        ) : null}
      </div>

      <div className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1">
        <TestBadge test={test} />
        <div className="ml-auto flex gap-2">
          <button
            type="button"
            onClick={onTest}
            disabled={test === 'testing'}
            className="font-mono text-xs text-text-secondary underline disabled:text-text-muted"
          >
            {test === 'testing' ? 'testing' : 'test connection'}
          </button>
          <button type="button" onClick={onEdit} className="font-mono text-xs text-text-secondary underline">
            edit
          </button>
          {!backend.is_default ? (
            <button type="button" onClick={onDefault} className="font-mono text-xs text-text-secondary underline">
              make default
            </button>
          ) : null}
          <button
            type="button"
            onClick={() => onToggle(!backend.enabled)}
            className="font-mono text-xs text-text-secondary underline"
          >
            {backend.enabled ? 'disable' : 'enable'}
          </button>
          <button type="button" onClick={onDelete} className="font-mono text-xs text-status-error underline">
            delete
          </button>
        </div>
      </div>
    </li>
  );
}

/** The test's answer, in the tier's own words. The four named states are the
 * storage package's sentinel messages; `connection_failed` is the honest
 * absence of a name, and the driver's sentence rides beside it either way. */
function TestBadge({ test }: { test?: StorageTestResult | 'testing' }) {
  if (!test) return null;
  if (test === 'testing') {
    return <span className="font-mono text-xs text-text-muted">testing…</span>;
  }
  if (test.ok) {
    return <span className="font-mono text-xs text-status-online">test: ok</span>;
  }
  return (
    <>
      <span className="font-mono text-xs text-status-error">test: {test.error}</span>
      {test.reason ? (
        <span className="font-mono text-xs text-text-muted" title={test.reason}>
          {test.reason}
        </span>
      ) : null}
    </>
  );
}

/** The fields the form owns per kind. `enabled` and `is_default` are absent:
 * they have their own routes (`PUT /:id/enabled`, `PUT /:id/default`) because
 * they are switches an operator flips without opening the row, and a form that
 * also wrote them would make a save of the bucket an un-audited flag change. */
const FORM_FIELDS: Record<StorageKind, (keyof StorageBackendWrite)[]> = {
  s3: ['name', 'endpoint', 'region', 'bucket', 'prefix', 'insecure_tls'],
  webdav: ['name', 'endpoint', 'prefix', 'username'],
  sftp: ['name', 'endpoint', 'root_path', 'username'],
  fs: ['name', 'root_path'],
};

function BackendForm({
  backend,
  onDone,
  onCancel,
}: {
  backend: StorageBackendView | null;
  onDone(): void;
  onCancel(): void;
}) {
  // The kind is chosen once, at the door: it decides which fields the row has
  // and which columns are sealed, and `UpdateStorageBackend` refuses to move
  // an existing row between kinds ("delete it and add the other one") because
  // the sealed credentials would come to mean something else.
  const [kind, setKind] = useState<StorageKind>(backend?.kind ?? 's3');
  const initialDraft = (forKind: StorageKind, name: string): Record<string, unknown> => {
    const initial: Record<string, unknown> = { kind: forKind, name };
    for (const field of FORM_FIELDS[forKind]) {
      if (field === 'name') continue;
      else if (field === 'insecure_tls') initial.insecure_tls = backend?.insecure_tls ?? false;
      else initial[field as string] = backend ? String(backend[field as keyof StorageBackendView] ?? '') : '';
    }
    for (const secret of SECRET_FIELDS[forKind]) initial[secret as string] = '';
    return initial;
  };
  const [draft, setDraft] = useState<Record<string, unknown>>(() =>
    initialDraft(kind, backend?.name ?? ''),
  );
  const [message, setMessage] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const set = (key: string, value: unknown) => setDraft((current) => ({ ...current, [key]: value }));

  const pickKind = (next: StorageKind) => {
    setKind(next);
    // A different kind is a different set of fields; the name is the one thing
    // worth carrying across, and the secrets were never filled yet anyway.
    setDraft(initialDraft(next, String(draft.name ?? '')));
  };

  const save = useCallback(async () => {
    setSaving(true);
    setMessage(null);
    // Every secret field of the kind is declared to `storageWrite`, which
    // travels only the non-empty ones: an untouched write-only input is
    // omitted — which on this API means *keep* — and clearing a secret is its
    // own deliberate act below, never a side effect of saving the name.
    const body = storageWrite(draft, (backend ?? {}) as Partial<Record<string, unknown>>, SECRET_FIELDS[kind] as string[]);
    try {
      if (backend) {
        await client.put<StorageBackendView>(`/storage/backends/${backend.id}`, body);
      } else {
        await client.post<StorageBackendView>('/storage/backends', body);
      }
      onDone();
    } catch (error) {
      // The server's presence check names the field (`"bucket is required for
      // an s3 backend"`), so the form does not duplicate the grammar.
      setMessage(serverDetail(error) ?? readWords(error, 'the backend could not be written'));
    } finally {
      setSaving(false);
    }
  }, [backend, draft, kind, onDone]);

  const clearSecret = useCallback(
    async (field: string) => {
      if (!backend) return;
      try {
        // The explicit empty string: the one payload shape that clears.
        await client.put<StorageBackendView>(`/storage/backends/${backend.id}`, { [field]: '' });
        onDone();
      } catch (error) {
        setMessage(readWords(error, 'the secret could not be cleared'));
      }
    },
    [backend, onDone],
  );

  return (
    <form
      className="mt-4 border border-rule bg-bg-card p-4"
      onSubmit={(event) => {
        event.preventDefault();
        void save();
      }}
    >
      <h3 className="font-cap text-sm uppercase tracking-cap text-text-primary">
        {backend ? `Edit ${backend.name}` : 'Add a backend'}
      </h3>
      {backend ? (
        <p className="mt-1 font-mono text-xs text-text-muted">
          kind: {kind} (a kind never changes on an existing row — delete it and add the other)
        </p>
      ) : (
        <label className="mt-2 block">
          <span className="font-mono text-[10px] uppercase text-text-muted">kind</span>
          <select
            value={kind}
            onChange={(event) => pickKind(event.target.value as StorageKind)}
            className="mt-0.5 block border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          >
            {STORAGE_KINDS.map((option) => (
              <option key={option} value={option}>
                {option}
              </option>
            ))}
          </select>
        </label>
      )}

      <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-3">
        {FORM_FIELDS[kind].map((field) =>
          field === 'name' || field === 'endpoint' || field === 'region' || field === 'bucket' ||
          field === 'prefix' || field === 'root_path' || field === 'username' ? (
            <label key={field} className="block">
              <span className="font-mono text-[10px] uppercase text-text-muted">{field}</span>
              <input
                value={String(draft[field] ?? '')}
                onChange={(event) => set(field as string, event.target.value)}
                className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
              />
            </label>
          ) : (
            <label key={field} className="flex items-center gap-2 font-mono text-xs text-text-secondary">
              <input
                type="checkbox"
                checked={Boolean(draft[field])}
                onChange={(event) => set(field as string, event.target.checked)}
              />
              insecure tls
            </label>
          ),
        )}

        {SECRET_FIELDS[kind].map((field) => {
          const key = field as string;
          const setFlag = SECRET_SET_FLAGS[key];
          const isSet = backend ? Boolean(setFlag ? backend[setFlag] : false) : false;
          return (
            <div key={key} className="block">
              <span className="font-mono text-[10px] uppercase text-text-muted">
                {key} — {isSet ? 'set' : 'unset'}
              </span>
              <div className="mt-0.5 flex items-center gap-2">
                <input
                  type="password"
                  value={String(draft[key] ?? '')}
                  placeholder={isSet ? 'already set — type to replace' : 'not set'}
                  onChange={(event) => set(key, event.target.value)}
                  className="w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
                />
                {isSet ? (
                  <button
                    type="button"
                    onClick={() => void clearSecret(key)}
                    className="font-mono text-xs text-status-error underline"
                  >
                    clear
                  </button>
                ) : null}
              </div>
            </div>
          );
        })}
      </div>

      <div className="mt-3 flex items-center gap-3">
        <button
          type="submit"
          disabled={saving}
          className="border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent disabled:border-rule disabled:text-text-muted"
        >
          {saving ? 'saving' : backend ? 'save backend' : 'add backend'}
        </button>
        <button
          type="button"
          onClick={onCancel}
          className="font-mono text-xs text-text-secondary underline"
        >
          cancel
        </button>
        {message ? <span className="font-mono text-xs text-status-error">{message}</span> : null}
      </div>
    </form>
  );
}

// ---------------------------------------------------------------------------
// Access: the OIDC doors.
// ---------------------------------------------------------------------------

function AccessTab() {
  const [page, setPage] = useState<ReadState<OIDCProviderView[]>>({ kind: 'reading' });
  const [editing, setEditing] = useState<string | 'new' | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  const load = useCallback(() => {
    setPage({ kind: 'reading' });
    void client
      .get<{ providers: OIDCProviderView[] }>('/settings/oidc')
      .then((response) => setPage({ kind: 'ready', rows: response.data.providers ?? [] }))
      .catch((error: unknown) =>
        setPage({ kind: 'failed', reason: readWords(error, 'the providers are unreadable') }),
      );
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const remove = useCallback(
    async (provider: OIDCProviderView) => {
      try {
        const { data } = await client.delete<{ deleted: string }>(
          `/settings/oidc/${encodeURIComponent(provider.name)}`,
        );
        setMessage(`deleted ${data.deleted}; the accounts it provisioned stay`);
        load();
      } catch (error) {
        setMessage(readWords(error, 'the provider could not be deleted'));
      }
    },
    [load],
  );

  const providers = page.kind === 'ready' ? page.rows : [];
  const editingProvider =
    editing && editing !== 'new' ? providers.find((p) => p.name === editing) : null;

  return (
    <section className="mt-4">
      {page.kind === 'reading' ? (
        <p className="mt-4 text-sm text-text-secondary">Reading the OIDC providers.</p>
      ) : null}
      {page.kind === 'failed' ? (
        <p className="mt-4 text-sm text-status-error">{page.reason}</p>
      ) : null}
      {page.kind === 'ready' && providers.length === 0 ? (
        <p className="mt-4 text-sm text-text-secondary">
          No OIDC providers. The login page offers the local admin only.
        </p>
      ) : null}
      {message ? <p className="mt-3 font-mono text-xs text-status-warning">{message}</p> : null}

      {page.kind === 'ready' && providers.length > 0 ? (
        <ul className="mt-4 border border-rule">
          {providers.map((provider) => (
            <li key={provider.id} className="border-b border-rule px-3 py-2 last:border-b-0">
              <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
                <span className="font-cap text-sm uppercase tracking-cap text-text-primary">
                  {provider.name}
                </span>
                <span className="font-mono text-xs text-text-secondary">{provider.issuer}</span>
                <span className="font-mono text-xs text-text-muted">
                  client secret: {provider.secret_set ? 'set' : 'unset'}
                </span>
                <span
                  className={`font-mono text-xs ${
                    provider.discovery_state === 'ok' ? 'text-status-online' : 'text-status-warning'
                  }`}
                >
                  discovery: {provider.discovery_state || 'never'}
                </span>
                {!provider.enabled ? (
                  <span className="font-mono text-xs text-status-warning">disabled</span>
                ) : null}
              </div>
              <p className="mt-1 font-mono text-xs text-text-muted">
                redirect uri (whitelist this in the IdP): {provider.redirect_uri}
              </p>
              <div className="mt-1 flex gap-2">
                <button
                  type="button"
                  onClick={() => setEditing(provider.name)}
                  className="font-mono text-xs text-text-secondary underline"
                >
                  edit
                </button>
                <button
                  type="button"
                  onClick={() => void remove(provider)}
                  className="font-mono text-xs text-status-error underline"
                >
                  delete
                </button>
              </div>
            </li>
          ))}
        </ul>
      ) : null}

      {editing === 'new' ? (
        <ProviderForm provider={null} onDone={() => { setEditing(null); load(); }} onCancel={() => setEditing(null)} />
      ) : null}
      {editingProvider ? (
        <ProviderForm
          provider={editingProvider}
          onDone={() => { setEditing(null); load(); }}
          onCancel={() => setEditing(null)}
        />
      ) : null}

      {editing === null ? (
        <button
          type="button"
          onClick={() => setEditing('new')}
          className="mt-4 border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent"
        >
          add a provider
        </button>
      ) : null}
    </section>
  );
}

function ProviderForm({
  provider,
  onDone,
  onCancel,
}: {
  provider: OIDCProviderView | null;
  onDone(): void;
  onCancel(): void;
}) {
  const [draft, setDraft] = useState<OIDCProviderWrite>(() => ({
    name: provider?.name ?? '',
    issuer: provider?.issuer ?? '',
    client_id: provider?.client_id ?? '',
    scope: provider?.scope ?? '',
    link_policy: provider?.link_policy ?? 'pending',
    allowed_domains: provider?.allowed_domains ?? '',
    enabled: provider?.enabled ?? true,
  }));
  const [clientSecret, setClientSecret] = useState('');
  const [message, setMessage] = useState<string | null>(null);
  const [discovery, setDiscovery] = useState<OIDCDiscoveryRefusal | null>(null);
  const [saving, setSaving] = useState(false);

  const set = <K extends keyof OIDCProviderWrite>(key: K, value: OIDCProviderWrite[K]) =>
    setDraft((current) => ({ ...current, [key]: value }));

  const save = useCallback(async () => {
    setSaving(true);
    setMessage(null);
    setDiscovery(null);
    const body: OIDCProviderWrite = { ...draft };
    // The same secret rule as every other write here: an untouched write-only
    // input is omitted, which keeps the stored secret.
    if (clientSecret !== '') body.client_secret = clientSecret;
    try {
      if (provider) {
        await client.put<OIDCProviderView>(
          `/settings/oidc/${encodeURIComponent(provider.name)}`,
          body,
        );
      } else {
        await client.post<OIDCProviderView>('/settings/oidc', body);
      }
      onDone();
    } catch (error) {
      // Discovery runs at save time, and its refusal is a named state, not a
      // 500: `discovery_unreachable` and `issuer_mismatch` are different
      // fixes, so they render as different words.
      const refusal = (error as { response?: { data?: OIDCDiscoveryRefusal } })?.response?.data;
      if (refusal?.discovery) {
        setDiscovery(refusal);
      } else {
        setMessage(readWords(error, 'the provider could not be written'));
      }
    } finally {
      setSaving(false);
    }
  }, [clientSecret, draft, onDone, provider]);

  return (
    <form
      className="mt-4 border border-rule bg-bg-card p-4"
      onSubmit={(event) => {
        event.preventDefault();
        void save();
      }}
    >
      <h3 className="font-cap text-sm uppercase tracking-cap text-text-primary">
        {provider ? `Edit ${provider.name}` : 'Add a provider'}
      </h3>
      <p className="mt-1 font-mono text-xs text-text-muted">
        discovery runs at save: the issuer is asked before the row is written.
      </p>

      <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-2">
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">name</span>
          <input
            value={draft.name ?? ''}
            onChange={(event) => set('name', event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">issuer</span>
          <input
            value={draft.issuer ?? ''}
            onChange={(event) => set('issuer', event.target.value)}
            placeholder="https://id.example.test/realms/nvr"
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">client id</span>
          <input
            value={draft.client_id ?? ''}
            onChange={(event) => set('client_id', event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <div className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">
            client secret — {provider ? (provider.secret_set ? 'set' : 'unset') : 'unset'}
          </span>
          <input
            type="password"
            value={clientSecret}
            placeholder={provider?.secret_set ? 'already set — type to replace' : 'not set'}
            onChange={(event) => setClientSecret(event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </div>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">link policy</span>
          <select
            value={draft.link_policy ?? 'pending'}
            onChange={(event) => set('link_policy', event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          >
            <option value="pending">pending</option>
            <option value="link">link</option>
          </select>
        </label>
        <label className="flex items-center gap-2 font-mono text-xs text-text-secondary">
          <input
            type="checkbox"
            checked={draft.enabled ?? true}
            onChange={(event) => set('enabled', event.target.checked)}
          />
          enabled
        </label>
      </div>

      {discovery ? (
        <p className="mt-3 font-mono text-xs text-status-error" role="alert">
          discovery: {discovery.discovery} — {discovery.discovery_reason}
        </p>
      ) : null}

      <div className="mt-3 flex items-center gap-3">
        <button
          type="submit"
          disabled={saving}
          className="border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent disabled:border-rule disabled:text-text-muted"
        >
          {saving ? 'saving' : provider ? 'save provider' : 'add provider'}
        </button>
        <button
          type="button"
          onClick={onCancel}
          className="font-mono text-xs text-text-secondary underline"
        >
          cancel
        </button>
        {message ? <span className="font-mono text-xs text-status-error">{message}</span> : null}
      </div>
    </form>
  );
}

// ---------------------------------------------------------------------------
// Webhooks: the doors out of the house, and the history that explains them.
// ---------------------------------------------------------------------------

function WebhooksTab() {
  const [page, setPage] = useState<ReadState<WebhookView[]>>({ kind: 'reading' });
  const [editing, setEditing] = useState<string | 'new' | null>(null);
  const [history, setHistory] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  const load = useCallback(() => {
    setPage({ kind: 'reading' });
    void client
      .get<{ webhooks: WebhookView[] }>('/settings/webhooks')
      .then((response) => setPage({ kind: 'ready', rows: response.data.webhooks ?? [] }))
      .catch((error: unknown) =>
        setPage({ kind: 'failed', reason: readWords(error, 'the webhook rows are unreadable') }),
      );
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const remove = useCallback(
    async (hook: WebhookView) => {
      try {
        const { data } = await client.delete<{ deleted: string; deliveries_removed: number }>(
          `/settings/webhooks/${hook.id}`,
        );
        setMessage(`deleted ${data.deleted}; ${data.deliveries_removed} delivery row(s) went with it`);
        load();
      } catch (error) {
        setMessage(readWords(error, 'the webhook could not be deleted'));
      }
    },
    [load],
  );

  const hooks = page.kind === 'ready' ? page.rows : [];
  const editingHook = editing && editing !== 'new' ? hooks.find((w) => w.id === editing) : null;

  return (
    <section className="mt-4">
      {page.kind === 'reading' ? (
        <p className="mt-4 text-sm text-text-secondary">Reading the webhook endpoints.</p>
      ) : null}
      {page.kind === 'failed' ? (
        <p className="mt-4 text-sm text-status-error">{page.reason}</p>
      ) : null}
      {page.kind === 'ready' && hooks.length === 0 ? (
        <p className="mt-4 text-sm text-text-secondary">
          No webhooks. Nothing outside this machine hears about events.
        </p>
      ) : null}
      {message ? <p className="mt-3 font-mono text-xs text-status-warning">{message}</p> : null}

      {page.kind === 'ready' && hooks.length > 0 ? (
        <ul className="mt-4 border border-rule">
          {hooks.map((hook) => (
            <li key={hook.id} className="border-b border-rule px-3 py-2 last:border-b-0">
              <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
                <span className="font-cap text-sm uppercase tracking-cap text-text-primary">
                  {hook.name}
                </span>
                <span className="font-mono text-xs text-text-secondary">{hook.url}</span>
                <span className="font-mono text-xs text-text-muted">
                  {hook.camera_id ? `camera ${hook.camera_id}` : 'every camera'}
                </span>
                <span className="font-mono text-xs text-text-muted">{hook.events.join(', ')}</span>
                <span className="font-mono text-xs text-text-muted">
                  secret: {hook.secret_set ? 'set' : 'unset'}
                </span>
                {!hook.enabled ? (
                  <span className="font-mono text-xs text-status-warning">disabled</span>
                ) : null}
              </div>
              {hook.last_error ? (
                <p className="mt-1 font-mono text-xs text-status-error">
                  last error: {hook.last_error}
                  {hook.last_delivered_at ? ` (last delivery ${hook.last_delivered_at})` : ''}
                </p>
              ) : hook.last_delivered_at ? (
                <p className="mt-1 font-mono text-xs text-status-online">
                  last delivered {hook.last_delivered_at}
                </p>
              ) : (
                <p className="mt-1 font-mono text-xs text-text-muted">never delivered</p>
              )}
              <div className="mt-1 flex gap-2">
                <button
                  type="button"
                  onClick={() => setHistory(history === hook.id ? null : hook.id)}
                  className="font-mono text-xs text-text-secondary underline"
                >
                  {history === hook.id ? 'hide history' : 'delivery history'}
                </button>
                <button
                  type="button"
                  onClick={() => setEditing(hook.id)}
                  className="font-mono text-xs text-text-secondary underline"
                >
                  edit
                </button>
                <button
                  type="button"
                  onClick={() => void remove(hook)}
                  className="font-mono text-xs text-status-error underline"
                >
                  delete
                </button>
              </div>
              {history === hook.id ? <DeliveryHistory webhookId={hook.id} /> : null}
            </li>
          ))}
        </ul>
      ) : null}

      {editing === 'new' ? (
        <WebhookForm hook={null} onDone={() => { setEditing(null); load(); }} onCancel={() => setEditing(null)} />
      ) : null}
      {editingHook ? (
        <WebhookForm
          hook={editingHook}
          onDone={() => { setEditing(null); load(); }}
          onCancel={() => setEditing(null)}
        />
      ) : null}

      {editing === null ? (
        <button
          type="button"
          onClick={() => setEditing('new')}
          className="mt-4 border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent"
        >
          add a webhook
        </button>
      ) : null}
    </section>
  );
}

/** The attempt ladder, newest first. "It answered 500 twice then 200" and "it
 * has never answered" are different problems, and only the sequence tells
 * them apart — which is why the history is a route of its own. */
function DeliveryHistory({ webhookId }: { webhookId: string }) {
  const [rows, setRows] = useState<ReadState<WebhookDeliveryView[]>>({ kind: 'reading' });

  useEffect(() => {
    void client
      .get<{ deliveries: WebhookDeliveryView[] }>(`/settings/webhooks/${webhookId}/deliveries`)
      .then((response) => setRows({ kind: 'ready', rows: response.data.deliveries ?? [] }))
      .catch((error: unknown) =>
        setRows({ kind: 'failed', reason: readWords(error, 'the delivery history is unreadable') }),
      );
  }, [webhookId]);

  if (rows.kind === 'reading') {
    return <p className="mt-2 font-mono text-xs text-text-muted">Reading the attempts.</p>;
  }
  if (rows.kind === 'failed') {
    return <p className="mt-2 font-mono text-xs text-status-error">{rows.reason}</p>;
  }
  if (rows.rows.length === 0) {
    return (
      <p className="mt-2 font-mono text-xs text-text-muted">
        No attempts: an attempt that never happened gets no row.
      </p>
    );
  }
  return (
    <ul className="mt-2 border border-rule">
      {rows.rows.map((row) => (
        <li key={row.id} className="border-b border-rule px-2 py-1 font-mono text-xs last:border-b-0">
          <span className={row.status === 'delivered' ? 'text-status-online' : 'text-status-error'}>
            attempt {row.attempt}: {row.status}
            {row.status_code ? ` (${row.status_code})` : ''}
          </span>
          {row.error ? <span className="ml-2 text-text-secondary">{row.error}</span> : null}
          <span className="ml-2 text-text-muted">{row.created_at}</span>
        </li>
      ))}
    </ul>
  );
}

function WebhookForm({
  hook,
  onDone,
  onCancel,
}: {
  hook: WebhookView | null;
  onDone(): void;
  onCancel(): void;
}) {
  const [draft, setDraft] = useState<WebhookWrite>(() => ({
    name: hook?.name ?? '',
    url: hook?.url ?? '',
    camera_id: hook?.camera_id ?? '',
    events: hook?.events ?? [...WEBHOOK_TRIGGERS],
    enabled: hook?.enabled ?? true,
  }));
  const [secret, setSecret] = useState('');
  const [message, setMessage] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const set = <K extends keyof WebhookWrite>(key: K, value: WebhookWrite[K]) =>
    setDraft((current) => ({ ...current, [key]: value }));

  const save = useCallback(async () => {
    setSaving(true);
    setMessage(null);
    const body: WebhookWrite = { ...draft };
    if (secret !== '') body.secret = secret;
    try {
      if (hook) {
        await client.put<WebhookView>(`/settings/webhooks/${hook.id}`, body);
      } else {
        await client.post<WebhookView>('/settings/webhooks', body);
      }
      onDone();
    } catch (error) {
      setMessage(serverDetail(error) ?? readWords(error, 'the webhook could not be written'));
    } finally {
      setSaving(false);
    }
  }, [draft, hook, onDone, secret]);

  return (
    <form
      className="mt-4 border border-rule bg-bg-card p-4"
      onSubmit={(event) => {
        event.preventDefault();
        void save();
      }}
    >
      <h3 className="font-cap text-sm uppercase tracking-cap text-text-primary">
        {hook ? `Edit ${hook.name}` : 'Add a webhook'}
      </h3>

      <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-2">
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">name</span>
          <input
            value={draft.name ?? ''}
            onChange={(event) => set('name', event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">url (http or https)</span>
          <input
            value={draft.url ?? ''}
            onChange={(event) => set('url', event.target.value)}
            placeholder="https://hooks.example.test/nvr"
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">
            camera id (empty = every camera)
          </span>
          <input
            value={draft.camera_id ?? ''}
            onChange={(event) => set('camera_id', event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <div className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">
            signing secret — {hook ? (hook.secret_set ? 'set' : 'unset') : 'unset'}
          </span>
          <input
            type="password"
            value={secret}
            placeholder={hook?.secret_set ? 'already set — type to replace' : 'not set'}
            onChange={(event) => setSecret(event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </div>
        <label className="flex items-center gap-2 font-mono text-xs text-text-secondary">
          <input
            type="checkbox"
            checked={draft.enabled ?? true}
            onChange={(event) => set('enabled', event.target.checked)}
          />
          enabled
        </label>
      </div>

      <div className="mt-3 flex items-center gap-3">
        <button
          type="submit"
          disabled={saving}
          className="border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent disabled:border-rule disabled:text-text-muted"
        >
          {saving ? 'saving' : hook ? 'save webhook' : 'add webhook'}
        </button>
        <button
          type="button"
          onClick={onCancel}
          className="font-mono text-xs text-text-secondary underline"
        >
          cancel
        </button>
        {message ? <span className="font-mono text-xs text-status-error">{message}</span> : null}
      </div>
    </form>
  );
}

// ---------------------------------------------------------------------------
// Retention: the windows, read from the machine that honours them.
// ---------------------------------------------------------------------------

function RetentionTab() {
  const [page, setPage] = useState<ReadState<RetentionView>>({ kind: 'reading' });
  // The roll and the segment length come through the same door the drawer's
  // strip pads by (P-11): one setting, one answer, two readers. The panel
  // never restates a default, so the console cannot show two numbers for one
  // knob. Its failure costs three rows, not the panel.
  const [roll, setRoll] = useState<MediaSettings | null>(null);

  useEffect(() => {
    void client
      .get<RetentionView>('/settings/retention')
      .then((response) => setPage({ kind: 'ready', rows: response.data }))
      .catch((error: unknown) =>
        setPage({ kind: 'failed', reason: readWords(error, 'the retention policy is unreadable') }),
      );
    void client
      .get<MediaSettings>('/v1/settings/media')
      .then((response) => setRoll(response.data))
      .catch(() => setRoll(null));
  }, []);

  if (page.kind === 'reading') {
    return <p className="mt-4 text-sm text-text-secondary">Reading the retention policy.</p>;
  }
  if (page.kind === 'failed') {
    return <p className="mt-4 text-sm text-status-error">{page.reason}</p>;
  }

  const view = page.rows;
  const rows: { label: string; value: string; note?: string }[] = [
    {
      label: 'local media window',
      value: `${view.local_days} days`,
      note: 'LOCAL_RETENTION_DAYS — a camera’s own local_retention_days overrides it',
    },
    {
      label: 'remote tier window',
      value: view.remote_forever ? 'keep forever' : `${view.remote_days} days`,
      note: 'REMOTE_RETENTION_DAYS — 0 means keep forever',
    },
    {
      label: 'event rows',
      value: `${view.event_days} days`,
      note: 'EVENT_RETENTION_DAYS — with them goes the footage an event was built from',
    },
    {
      label: 'un-uploaded protection',
      value: `${view.unuploaded_protection_days} days`,
      note: 'max(3 × local, 7): footage that never reached the tier is kept this long past its window',
    },
    { label: 'delivery history', value: `${view.webhook_delivery_days} days` },
    { label: 'audit ledger', value: `${view.audit_days} days` },
    { label: 'queue rows', value: `${view.job_days} days` },
    ...(roll
      ? [
          {
            label: 'clip pre-roll',
            value: `${roll.pre_roll_seconds}s`,
            note: 'PRE_ROLL_SECONDS — a clip starts this far before the event, and the drawer’s strip asks for segments from there',
          },
          {
            label: 'clip post-roll',
            value: `${roll.post_roll_seconds}s`,
            note: 'POST_ROLL_SECONDS — and ends this far after it',
          },
          {
            label: 'segment length',
            value: `${roll.segment_seconds}s`,
            note: 'SEGMENT_SECONDS — the same number the recorder’s status line carries',
          },
        ]
      : []),
  ];

  return (
    <section className="mt-4">
      <p className="text-sm text-text-secondary">
        These are the windows this installation sweeps by. They are authored by the environment —
        the server reads them at boot and this page reads them from the server; there is no
        console write path, because a policy with two authors is two policies.
      </p>
      <table className="mt-4 border border-rule">
        <tbody>
          {rows.map((row) => (
            <tr key={row.label} className="border-b border-rule last:border-b-0">
              <th className="px-3 py-1.5 text-left font-mono text-xs font-normal uppercase text-text-muted">
                {row.label}
              </th>
              <td className="px-3 py-1.5 font-mono text-xs text-text-primary">{row.value}</td>
              <td className="px-3 py-1.5 font-mono text-xs text-text-muted">{row.note ?? ''}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
