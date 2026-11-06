/**
 * The cameras page: the rows, and the rules drawn over a picture.
 *
 * The list is the machine's inventory — what is recorded, what is watched, what
 * the media host says is producing right now. The editor is where an operator
 * turns one of those rows into a rule set, and it is a URL (`/cameras/:id`)
 * rather than modal state, for the same reason the event drawer is one: a rule
 * you are arguing about at 2am is worth pasting into a chat, and a reload should
 * not lose your place.
 *
 * Two write paths, deliberately apart. The row's fields go through
 * `PUT /api/v1/cameras/:id`, which re-renders go2rtc's config because a row can
 * change a stream. The polygons go through `PUT /api/v1/cameras/:id/zones`,
 * which does not, and carries its own audit row with the before and the after
 * (`server/api/zones.go`). So the form never sends `zones`: a body that wrote
 * both would make a save of the frame rate a zone write too, with the wrong
 * ledger entry behind it.
 *
 * The slug is shown and not edited. It is the name of every stored key this
 * camera has ever written (`nvr/<slug>/<YYYY>/<MM>/<DD>/…`), so renaming one is
 * a migration, not a text field — and a console that offers it without the
 * migration is a console that orphans footage.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { Link, useMatch, useNavigate } from 'react-router-dom';

import client, { serverWords } from '../api/client';
import { RECORD_MODES, type CameraView, type CameraWrite, type Zone } from '../api/cameras';
import { useAuth } from '../auth/context';
import { useRealtime } from '../stores/realtime';
import ZoneEditor from '../components/ZoneEditor';

type PageState =
  | { kind: 'reading' }
  | { kind: 'failed'; reason: string }
  | { kind: 'ready'; cameras: CameraView[] };

/** The fields this form owns. `zones` is absent on purpose — see the file's note
 * on the two write paths. */
type RowWrite = Pick<
  CameraWrite,
  | 'name'
  | 'source_uri'
  | 'record_mode'
  | 'detection_enabled'
  | 'detection_fps'
  | 'sensitivity'
  | 'min_area_ratio'
  | 'local_retention_days'
  | 'remote_retention_days'
  | 'active'
>;

function rowWrite(camera: CameraView): RowWrite {
  return {
    name: camera.name,
    source_uri: camera.source_uri,
    record_mode: camera.record_mode,
    detection_enabled: camera.detection_enabled,
    detection_fps: camera.detection_fps,
    sensitivity: camera.sensitivity,
    min_area_ratio: camera.min_area_ratio,
    local_retention_days: camera.local_retention_days,
    remote_retention_days: camera.remote_retention_days,
    active: camera.active,
  };
}

export default function CamerasPage() {
  const [page, setPage] = useState<PageState>({ kind: 'reading' });
  const match = useMatch('/cameras/:id');
  const selectedId = match?.params.id ?? null;
  const { hasAnyRole } = useAuth();
  const canWrite = hasAnyRole(['operator', 'admin']);

  const load = useCallback(() => {
    setPage({ kind: 'reading' });
    void client
      .get<{ cameras: CameraView[] }>('/v1/cameras')
      .then((response) => setPage({ kind: 'ready', cameras: response.data.cameras ?? [] }))
      .catch((error: unknown) =>
        setPage({ kind: 'failed', reason: serverWords(error, 'the cameras could not be read') }),
      );
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const cameras = page.kind === 'ready' ? page.cameras : [];
  const selected = useMemo(
    () => cameras.find((camera) => camera.id === selectedId) ?? null,
    [cameras, selectedId],
  );

  return (
    <main className="p-6">
      <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Cameras</h1>

      {page.kind === 'reading' ? (
        <p className="mt-4 text-sm text-text-secondary">Reading the camera rows.</p>
      ) : null}

      {page.kind === 'failed' ? (
        <p className="mt-4 text-sm text-status-error">{page.reason}</p>
      ) : null}

      {page.kind === 'ready' && cameras.length === 0 ? (
        <p className="mt-4 text-sm text-text-secondary">
          No cameras yet. Add the first one below; it will start recording as soon
          as the media host can reach it.
        </p>
      ) : null}

      {page.kind === 'ready' && cameras.length > 0 && !selected ? (
        <ul className="mt-4 border border-rule">
          {cameras.map((camera) => (
            <CameraRow key={camera.id} camera={camera} />
          ))}
        </ul>
      ) : null}

      {selected ? <CameraEditor camera={selected} canWrite={canWrite} onChanged={load} /> : null}

      {canWrite ? <NewCameraForm onCreated={load} /> : null}
    </main>
  );
}

/** One row of the inventory. The online/offline word comes from the feed, not
 * from the row: a camera row always says it exists, and `camera.status` is
 * go2rtc's own answer about whether it is producing. */
function CameraRow({ camera }: { camera: CameraView }) {
  const status = useRealtime((state) => state.cameras[camera.id]);

  return (
    <li className="border-b border-rule last:border-b-0">
      <Link
        to={`/cameras/${camera.id}`}
        className="flex flex-wrap items-baseline gap-x-4 gap-y-1 px-3 py-2 hover:bg-bg-secondary"
      >
        <span className="font-cap text-sm uppercase tracking-cap text-text-primary">
          {camera.name || camera.slug}
        </span>
        <span className="font-mono text-xs text-text-secondary">{camera.slug}</span>
        <span className="font-mono text-xs text-text-muted">{camera.record_mode}</span>
        <span className="font-mono text-xs text-text-muted">
          {camera.detection_enabled ? 'detection on' : 'detection off'}
        </span>
        <span className="font-mono text-xs text-text-muted">
          {camera.zones.length} zone{camera.zones.length === 1 ? '' : 's'}
        </span>
        {!camera.active ? (
          <span className="font-mono text-xs text-status-warning">inactive</span>
        ) : null}
        <span
          className={`ml-auto font-mono text-xs uppercase tracking-cap ${
            status ? (status.online ? 'text-status-online' : 'text-status-offline') : 'text-text-muted'
          }`}
        >
          {status ? (status.online ? 'online' : 'offline') : 'no status yet'}
        </span>
      </Link>
    </li>
  );
}

function CameraEditor({
  camera,
  canWrite,
  onChanged,
}: {
  camera: CameraView;
  canWrite: boolean;
  onChanged(): void;
}) {
  const navigate = useNavigate();
  const [draft, setDraft] = useState<RowWrite>(() => rowWrite(camera));
  const [zones, setZones] = useState<Zone[]>(camera.zones ?? []);
  const [message, setMessage] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  // A different row is a different form. Without this, editing one camera and
  // clicking another leaves the first camera's numbers in the second's fields.
  useEffect(() => {
    setDraft(rowWrite(camera));
    setZones(camera.zones ?? []);
    setMessage(null);
  }, [camera]);

  const dirty = useMemo(
    () => JSON.stringify(draft) !== JSON.stringify(rowWrite(camera)),
    [draft, camera],
  );

  const save = useCallback(async () => {
    setSaving(true);
    try {
      await client.put<CameraView>(`/v1/cameras/${camera.id}`, draft);
      setMessage('saved');
      onChanged();
    } catch (error) {
      setMessage(serverWords(error, 'the camera could not be written'));
    } finally {
      setSaving(false);
    }
  }, [camera.id, draft, onChanged]);

  const set = <K extends keyof RowWrite>(key: K, value: RowWrite[K]) =>
    setDraft((current) => ({ ...current, [key]: value }));

  return (
    <section className="mt-4 border border-rule bg-bg-card p-4">
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="font-cap text-lg uppercase tracking-cap text-text-primary">
          {camera.name || camera.slug}
        </h2>
        <button
          type="button"
          onClick={() => navigate('/cameras')}
          className="font-mono text-xs text-text-secondary underline"
        >
          back to the list
        </button>
      </header>
      <p className="mt-1 font-mono text-xs text-text-muted">
        {camera.slug} · detection frame {camera.detection_width}×{camera.detection_height}
      </p>

      <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-3">
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">name</span>
          <input
            value={draft.name ?? ''}
            disabled={!canWrite}
            onChange={(event) => set('name', event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="col-span-2 block">
          <span className="font-mono text-[10px] uppercase text-text-muted">
            source uri (rtsp://…, read by the media host)
          </span>
          <input
            value={draft.source_uri ?? ''}
            disabled={!canWrite}
            onChange={(event) => set('source_uri', event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">record mode</span>
          <select
            value={draft.record_mode ?? 'continuous'}
            disabled={!canWrite}
            onChange={(event) => set('record_mode', event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          >
            {RECORD_MODES.map((mode) => (
              <option key={mode} value={mode}>
                {mode}
              </option>
            ))}
          </select>
        </label>
        <label className="flex items-center gap-2 font-mono text-xs text-text-secondary">
          <input
            type="checkbox"
            checked={draft.detection_enabled ?? false}
            disabled={!canWrite}
            onChange={(event) => set('detection_enabled', event.target.checked)}
          />
          detection
        </label>
        <label className="flex items-center gap-2 font-mono text-xs text-text-secondary">
          <input
            type="checkbox"
            checked={draft.active ?? true}
            disabled={!canWrite}
            onChange={(event) => set('active', event.target.checked)}
          />
          active
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">detection fps</span>
          <input
            type="number"
            min={0}
            value={draft.detection_fps ?? 0}
            disabled={!canWrite}
            onChange={(event) => set('detection_fps', Number(event.target.value))}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">sensitivity</span>
          <input
            type="number"
            min={0}
            max={1}
            step={0.05}
            value={draft.sensitivity ?? 0}
            disabled={!canWrite}
            onChange={(event) => set('sensitivity', Number(event.target.value))}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">min area ratio</span>
          <input
            type="number"
            min={0}
            max={1}
            step={0.001}
            value={draft.min_area_ratio ?? 0}
            disabled={!canWrite}
            onChange={(event) => set('min_area_ratio', Number(event.target.value))}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">local days</span>
          <input
            type="number"
            min={0}
            value={draft.local_retention_days ?? 0}
            disabled={!canWrite}
            onChange={(event) => set('local_retention_days', Number(event.target.value))}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">remote days</span>
          <input
            type="number"
            min={0}
            value={draft.remote_retention_days ?? 0}
            disabled={!canWrite}
            onChange={(event) => set('remote_retention_days', Number(event.target.value))}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
      </div>

      {canWrite ? (
        <div className="mt-3 flex items-center gap-3">
          <button
            type="button"
            onClick={() => void save()}
            disabled={saving || !dirty}
            className="border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent disabled:border-rule disabled:text-text-muted"
          >
            {saving ? 'saving' : dirty ? 'save row' : 'saved'}
          </button>
          {message ? <span className="font-mono text-xs text-text-secondary">{message}</span> : null}
        </div>
      ) : (
        <p className="mt-3 font-mono text-xs text-text-secondary">
          your role can read cameras, not change them
        </p>
      )}

      <div className="mt-5">
        <ZoneEditor
          key={camera.id}
          camera={{ ...camera, zones }}
          canWrite={canWrite}
          onSaved={(written) => {
            setZones(written);
            setMessage(null);
          }}
        />
      </div>
    </section>
  );
}

/** The add-a-camera form. It asks for the four things the server cannot invent —
 * a slug, a name, a source, and how to record it — and leaves every tuning
 * default to `CreateCamera`, which owns them. */
function NewCameraForm({ onCreated }: { onCreated(): void }) {
  const [slug, setSlug] = useState('');
  const [name, setName] = useState('');
  const [sourceUri, setSourceUri] = useState('');
  const [recordMode, setRecordMode] = useState<string>('continuous');
  const [message, setMessage] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const create = useCallback(async () => {
    setSaving(true);
    try {
      await client.post<CameraView>('/v1/cameras', {
        slug,
        name,
        source_uri: sourceUri,
        record_mode: recordMode,
      });
      setMessage(null);
      setSlug('');
      setName('');
      setSourceUri('');
      onCreated();
    } catch (error) {
      setMessage(serverWords(error, 'the camera could not be added'));
    } finally {
      setSaving(false);
    }
  }, [name, onCreated, recordMode, slug, sourceUri]);

  return (
    <form
      className="mt-6 border border-rule bg-bg-card p-4"
      onSubmit={(event) => {
        event.preventDefault();
        void create();
      }}
    >
      <h2 className="font-cap text-sm uppercase tracking-cap text-text-primary">Add a camera</h2>
      <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-4">
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">slug</span>
          <input
            value={slug}
            onChange={(event) => setSlug(event.target.value)}
            placeholder="porch"
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">name</span>
          <input
            value={name}
            onChange={(event) => setName(event.target.value)}
            placeholder="Porch"
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block md:col-span-2">
          <span className="font-mono text-[10px] uppercase text-text-muted">source uri</span>
          <input
            value={sourceUri}
            onChange={(event) => setSourceUri(event.target.value)}
            placeholder="rtsp://user:pass@192.168.1.20:554/Streaming/Channels/101"
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">record mode</span>
          <select
            value={recordMode}
            onChange={(event) => setRecordMode(event.target.value)}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          >
            {RECORD_MODES.map((mode) => (
              <option key={mode} value={mode}>
                {mode}
              </option>
            ))}
          </select>
        </label>
      </div>
      <div className="mt-3 flex items-center gap-3">
        <button
          type="submit"
          disabled={saving || slug.trim() === '' || sourceUri.trim() === ''}
          className="border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent disabled:border-rule disabled:text-text-muted"
        >
          {saving ? 'adding' : 'add camera'}
        </button>
        {message ? <span className="font-mono text-xs text-status-error">{message}</span> : null}
      </div>
    </form>
  );
}
