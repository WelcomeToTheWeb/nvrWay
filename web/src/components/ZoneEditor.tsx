/**
 * The zone editor: the operator draws, the server decides.
 *
 * The plan's sentence is the contract — `<canvas>` over
 * `GET /api/v1/cameras/:id/snapshot`, click appends a vertex, Enter closes,
 * Escape cancels, drag moves a vertex, Delete removes the zone, and the write is
 * `PUT /api/v1/cameras/:id/zones` with the exact `ZonesJSON` shape of Step 1.
 * Everything below is that, plus the two things the sentence leaves open.
 *
 * **The canvas is a painting, not the state.** Every vertex lives in React state
 * as a `0..1` pair, and the canvas is repainted *from* it. That is not tidiness:
 * a canvas is a bitmap whose size is whatever CSS gave it, so a rule that lived
 * in canvas coordinates would be a rule that changes when the window does. The
 * pointer is converted to normalised numbers the moment it arrives
 * (`toNormalized`) and every pixel drawn afterwards is derived from the stored
 * number — which is also what makes the geometry testable at all, because a
 * headless test has no pixels to read back.
 *
 * **The console advises and never refuses.** `server/api/zones.go` states the
 * rule — *"One validator, three readers (console, API, worker)"* — and a fourth
 * opinion about which polygons are legal is how two installations end up
 * disagreeing. So Enter closes whatever shape is on the board, even a two-point
 * one, and the editor says in its own voice that the server wants 3–32 vertices;
 * the refusal itself is the server's 422, and both halves of its body are
 * rendered verbatim. An operator who is blocked locally never learns the
 * sentence the API was written to say.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import client, { serverDetail, serverWords } from '../api/client';
import { useMediaUrl } from '../api/media';
import {
  DETECTION_CLASSES,
  ZONE_LIMITS,
  ZONE_RULES,
  blankZone,
  enclosesArea,
  mintZoneId,
  toNormalized,
  type CameraView,
  type Zone,
} from '../api/cameras';

export interface ZoneEditorProps {
  /** The row as the server answered it. `zones` is the rule set this editor
   * starts from; `detection_width/height` is the frame the stored vertices are
   * fractions of, and the only aspect available before a picture arrives. */
  camera: CameraView;
  /** The role floor for the write (`operator`, `admin`). A viewer sees the same
   * picture and is told what they cannot do, rather than staring at controls
   * that answer 403. */
  canWrite: boolean;
  /** A successful write, answered by the server's own array — the page updates
   * its row from it instead of refetching. */
  onSaved(zones: Zone[]): void;
}

/** A vertex this close (CSS px) to the pointer is the one being dragged. In
 * normalised terms it would be a different distance on every screen. */
const GRAB_PX = 9;

interface Drag {
  zoneId: string;
  index: number;
}

interface Refusal {
  error: string;
  detail: string | null;
}

/** A shape the server will refuse: too few vertices, or none of the area. */
function isDegenerate(polygon: number[][]): boolean {
  return polygon.length < ZONE_LIMITS.minVertices || !enclosesArea(polygon);
}

/** The accent, as the theme defines it. Canvas takes a colour string, not a
 * custom property, so the token is read off the element; when the platform
 * gives nothing back, `currentColor` is the honest fallback — the canvas
 * element carries `text-accent`, so it resolves to the same token by way of
 * the cascade. */
function accentColor(canvas: HTMLCanvasElement): string {
  const read = getComputedStyle(canvas).getPropertyValue('--accent').trim();
  return read !== '' ? read : 'currentColor';
}

export default function ZoneEditor({ camera, canWrite, onSaved }: ZoneEditorProps) {
  const [zones, setZones] = useState<Zone[]>(() => camera.zones ?? []);
  // What the server last confirmed, as the baseline for "unsaved". It is the
  // editor's own memory, not the row's: a save of the row's *other* fields
  // refetches the camera, and a board that reset on every row refresh would
  // throw away a polygon half-drawn. Switching cameras remounts this component
  // (`key={camera.id}` in CamerasPage), which is where the board is reset.
  const [confirmed, setConfirmed] = useState(() => JSON.stringify(camera.zones ?? []));
  const [selected, setSelected] = useState<string | null>(null);
  const [draft, setDraft] = useState<number[][] | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [saving, setSaving] = useState(false);
  // A repaint the canvas cannot ask for itself: it happens when the picture
  // arrives, because an image that loads after the last paint is otherwise a
  // blank rectangle until the next edit.
  const [tick, setTick] = useState(0);
  // `?t=` on the snapshot path. The route answers `Cache-Control: private,
  // max-age=5`, so a fresh frame is a fresh URL — the server's own cache is what
  // keeps this from pinning the camera's producer.
  const [frameNonce, setFrameNonce] = useState(0);

  const canvasRef = useRef<HTMLCanvasElement>(null);
  const imageRef = useRef<HTMLImageElement | null>(null);
  const dragRef = useRef<Drag | null>(null);

  const frame = useMediaUrl(`/v1/cameras/${camera.id}/snapshot${frameNonce > 0 ? `?t=${frameNonce}` : ''}`);
  const frameUrl = frame.kind === 'ready' ? frame.url : null;

  // The box follows the picture, so a 4:3 frame is not drawn into a 16:9 hole:
  // a stretched backdrop is a rule that looks aimed at the driveway and is not.
  // Before the picture arrives this is only a placeholder — `detection_width`
  // and `detection_height` describe the detection input (and default to 0,
  // meaning "the server's"), not the stream's shape — and the moment the frame
  // loads the image's own aspect takes over. A vertex is converted with the box
  // the canvas actually had at that instant, so a placeholder is never a wrong
  // number, only a brief shape.
  const [aspect, setAspect] = useState<string>(() =>
    camera.detection_width > 0 && camera.detection_height > 0
      ? `${camera.detection_width} / ${camera.detection_height}`
      : '16 / 9',
  );

  const dirty = useMemo(() => JSON.stringify(zones) !== confirmed, [zones, confirmed]);

  // ---------------------------------------------------------------- painting

  const paint = useCallback(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const rect = canvas.getBoundingClientRect();
    // No box means nothing is on screen to paint into — a detached canvas, or a
    // test runner with no layout engine. Asking for a 2d context here would be
    // a platform complaint about a thing that was never going to be drawn.
    if (rect.width <= 0 || rect.height <= 0) return;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;

    const dpr = window.devicePixelRatio || 1;
    const w = Math.round(rect.width * dpr);
    const h = Math.round(rect.height * dpr);
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w;
      canvas.height = h;
    }
    ctx.clearRect(0, 0, w, h);

    const image = imageRef.current;
    if (image && image.naturalWidth > 0) ctx.drawImage(image, 0, 0, w, h);

    const accent = accentColor(canvas);
    for (const zone of zones) {
      const degenerate = isDegenerate(zone.polygon);
      ctx.beginPath();
      zone.polygon.forEach(([x, y], i) => {
        if (i === 0) ctx.moveTo(x * w, y * h);
        else ctx.lineTo(x * w, y * h);
      });
      ctx.closePath();
      // The plan's fill: the accent at 20 %, so the picture stays the picture.
      ctx.globalAlpha = degenerate ? 0.08 : 0.2;
      ctx.fillStyle = accent;
      ctx.fill();
      ctx.globalAlpha = 1;
      ctx.setLineDash(degenerate ? [6, 4] : []);
      ctx.lineWidth = zone.id === selected ? 3 : 1.5;
      ctx.strokeStyle = accent;
      ctx.stroke();
      ctx.setLineDash([]);

      for (const [x, y] of zone.polygon) {
        ctx.fillStyle = accent;
        ctx.fillRect(x * w - 3, y * h - 3, 6, 6);
      }
    }

    if (draft) {
      ctx.beginPath();
      draft.forEach(([x, y], i) => {
        if (i === 0) ctx.moveTo(x * w, y * h);
        else ctx.lineTo(x * w, y * h);
      });
      ctx.globalAlpha = 0.2;
      ctx.fillStyle = accent;
      ctx.fill();
      ctx.globalAlpha = 1;
      ctx.setLineDash([4, 4]);
      ctx.lineWidth = 1.5;
      ctx.strokeStyle = accent;
      ctx.stroke();
      ctx.setLineDash([]);
      for (const [x, y] of draft) ctx.fillRect(x * w - 3, y * h - 3, 6, 6);
    }
  }, [zones, draft, selected]);

  useEffect(() => {
    paint();
  }, [paint, tick, aspect, frameUrl]);

  // The window's size is not the canvas's size until it is laid out, and the
  // backing store has to follow. A resize listener is enough: the editor has no
  // container that changes size underneath it.
  useEffect(() => {
    const onResize = () => paint();
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, [paint]);

  useEffect(() => {
    if (!frameUrl) {
      imageRef.current = null;
      setTick((n) => n + 1);
      return;
    }
    let live = true;
    const image = new Image();
    image.onload = () => {
      if (!live) return;
      imageRef.current = image;
      if (image.naturalWidth > 0 && image.naturalHeight > 0) {
        setAspect(`${image.naturalWidth} / ${image.naturalHeight}`);
      }
      setTick((n) => n + 1);
    };
    image.onerror = () => {
      if (!live) return;
      imageRef.current = null;
      setTick((n) => n + 1);
    };
    image.src = frameUrl;
    return () => {
      live = false;
    };
  }, [frameUrl]);

  // ------------------------------------------------------------- geometry

  const box = useCallback(() => {
    const canvas = canvasRef.current;
    return canvas ? canvas.getBoundingClientRect() : null;
  }, []);

  /** The vertex under the pointer, in any zone — dragging a corner of a rule you
   * have not selected yet is an edit, not a mistake worth punishing. The test is
   * made in CSS pixels, because a vertex is a thing an operator aims at, not a
   * fraction of a screen. */
  const grab = useCallback(
    (clientX: number, clientY: number, rect: DOMRect): Drag | null => {
      const nx = (clientX - rect.left) / rect.width;
      const ny = (clientY - rect.top) / rect.height;
      let best: Drag | null = null;
      let bestDistance = Infinity;
      for (const zone of zones) {
        zone.polygon.forEach(([x, y], index) => {
          const distance = Math.hypot((x - nx) * rect.width, (y - ny) * rect.height);
          if (distance <= GRAB_PX && distance < bestDistance) {
            bestDistance = distance;
            best = { zoneId: zone.id, index };
          }
        });
      }
      return best;
    },
    [zones],
  );

  const onPointerDown = useCallback(
    (event: React.PointerEvent<HTMLCanvasElement>) => {
      const canvas = canvasRef.current;
      const rect = box();
      if (!canvas || !rect || rect.width <= 0 || rect.height <= 0) return;
      // Focus without a click landing on a control: Enter, Escape and Delete are
      // the canvas's keys, and they only reach it if it has focus.
      canvas.focus();
      if (!canWrite) return;

      const held = grab(event.clientX, event.clientY, rect);
      if (held) {
        dragRef.current = held;
        setSelected(held.zoneId);
        // Capture keeps a drag alive when the pointer leaves the picture. Not
        // every platform has it (jsdom does not), and a drag that ends at the
        // edge is a drag, not a bug.
        if (typeof canvas.setPointerCapture === 'function') {
          canvas.setPointerCapture(event.pointerId);
        }
        return;
      }

      const point = toNormalized(rect, event.clientX, event.clientY);
      if (draft) {
        setDraft([...draft, point]);
        setNotice(
          draft.length + 1 >= ZONE_LIMITS.minVertices
            ? null
            : `${draft.length + 1} point${draft.length + 1 === 1 ? '' : 's'} — ${ZONE_LIMITS.minVertices} close a shape`,
        );
        return;
      }
      setSelected(zoneIdAt(zones, point) ?? null);
    },
    [box, canWrite, draft, grab, zones],
  );

  // Move and up live on the window, so a drag that leaves the canvas still
  // tracks the pointer and still ends when the button is let go.
  useEffect(() => {
    const move = (event: PointerEvent) => {
      const held = dragRef.current;
      const rect = box();
      if (!held || !rect || rect.width <= 0 || rect.height <= 0) return;
      const point = toNormalized(rect, event.clientX, event.clientY);
      setZones((current) =>
        current.map((zone) =>
          zone.id === held.zoneId
            ? {
                ...zone,
                polygon: zone.polygon.map(([x, y], i) =>
                  i === held.index ? point : [x, y],
                ),
              }
            : zone,
        ),
      );
    };
    const up = () => {
      dragRef.current = null;
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
    window.addEventListener('pointercancel', up);
    return () => {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', up);
      window.removeEventListener('pointercancel', up);
    };
  }, [box]);

  // ------------------------------------------------------------ keyboard

  const startDraft = useCallback(() => {
    if (zones.length >= ZONE_LIMITS.maxZones) {
      setNotice(`a camera holds at most ${ZONE_LIMITS.maxZones} zones`);
      return;
    }
    setRefusal(null);
    setDraft([]);
    setSelected(null);
    setNotice('click the picture to place points; Enter closes the shape');
  }, [zones.length]);

  const closeDraft = useCallback(() => {
    if (!draft) return;
    if (draft.length < 2) {
      setNotice(`a shape needs at least 2 points to close; this has ${draft.length}`);
      return;
    }
    const id = mintZoneId(zones);
    const zone = blankZone(id, draft);
    setZones([...zones, zone]);
    setSelected(id);
    setDraft(null);
    setNotice(
      isDegenerate(draft)
        ? `"${zone.name || id}" has ${draft.length} vertices — the server wants ${ZONE_LIMITS.minVertices}–${ZONE_LIMITS.maxVertices}, and it will refuse this write until it has them`
        : null,
    );
  }, [draft, zones]);

  const cancelDraft = useCallback(() => {
    setDraft(null);
    setNotice(null);
  }, []);

  const removeZone = useCallback(
    (id: string) => {
      setZones((current) => current.filter((zone) => zone.id !== id));
      setSelected((current) => (current === id ? null : current));
    },
    [],
  );

  const onKeyDown = useCallback(
    (event: React.KeyboardEvent<HTMLElement>) => {
      // A key typed into a field is an answer to that field. Enter in the name
      // box means "this name", not "close the polygon".
      const target = event.target as HTMLElement | null;
      if (target && /^(INPUT|SELECT|TEXTAREA|BUTTON)$/.test(target.tagName)) return;
      if (event.key === 'Enter') {
        event.preventDefault();
        closeDraft();
      } else if (event.key === 'Escape') {
        if (draft) cancelDraft();
        else setSelected(null);
      } else if (event.key === 'Delete' || event.key === 'Backspace') {
        if (!selected || !canWrite) return;
        event.preventDefault();
        removeZone(selected);
      }
    },
    [canWrite, cancelDraft, closeDraft, draft, removeZone, selected],
  );

  // --------------------------------------------------------------- writes

  const save = useCallback(async () => {
    setSaving(true);
    setRefusal(null);
    try {
      const { data } = await client.put<{ camera_id: string; slug: string; zones: Zone[] }>(
        `/v1/cameras/${camera.id}/zones`,
        zones,
      );
      // The server's array is the truth: it drops a vertex that repeats the
      // first, so the shape on screen is redrawn from what it kept.
      const written = data.zones ?? zones;
      setZones(written);
      setConfirmed(JSON.stringify(written));
      onSaved(written);
    } catch (error) {
      setRefusal({ error: serverWords(error, 'the zones could not be written'), detail: serverDetail(error) });
    } finally {
      setSaving(false);
    }
  }, [camera.id, onSaved, zones]);

  const patch = useCallback((id: string, change: Partial<Zone>) => {
    setZones((current) => current.map((zone) => (zone.id === id ? { ...zone, ...change } : zone)));
  }, []);

  // ---------------------------------------------------------------- render

  const frameWords =
    frame.kind === 'reading'
      ? 'reading the frame'
      : frame.kind === 'absent'
        ? `${frame.reason} — you can still draw, but you are drawing without a picture`
        : frame.kind === 'failed'
          ? `${frame.reason} — you can still draw, but you are drawing without a picture`
          : null;

  return (
    <section className="border border-rule bg-bg-card p-4" onKeyDown={onKeyDown}>
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="font-cap text-sm uppercase tracking-cap text-text-primary">
          Detection zones
        </h3>
        <span className="font-mono text-xs text-text-muted">
          {zones.length} of {ZONE_LIMITS.maxZones} · vertices normalised 0..1
        </span>
      </header>

      <div className="relative mt-3" style={{ aspectRatio: aspect }}>
        <canvas
          ref={canvasRef}
          tabIndex={0}
          aria-label={`zone editor over the ${camera.slug} snapshot`}
          onPointerDown={onPointerDown}
          className="block h-full w-full bg-black text-accent outline-none"
        />
        {/* The rule badge is DOM, not pixels: it has to be readable at any
            density, and an operator has to be able to reach it with a keyboard.
            Its position is the stored vertex, so it doubles as the proof that
            the number round-trips. */}
        {zones.map((zone) => (
          <button
            key={zone.id}
            type="button"
            onClick={() => setSelected(zone.id)}
            className={`absolute -translate-x-0 -translate-y-full border px-1 font-mono text-[10px] uppercase tracking-cap ${
              zone.id === selected
                ? 'border-accent bg-bg-card text-accent'
                : 'border-rule bg-bg-card text-text-secondary'
            }`}
            style={{
              left: `${(zone.polygon[0]?.[0] ?? 0) * 100}%`,
              top: `${(zone.polygon[0]?.[1] ?? 0) * 100}%`,
            }}
          >
            {zone.name || zone.id} · {zone.rule}
            {zone.enabled ? '' : ' · off'}
          </button>
        ))}
        {frameWords ? (
          <p className="absolute inset-x-0 top-1/2 -translate-y-1/2 px-4 text-center text-sm text-text-secondary">
            {frameWords}
          </p>
        ) : null}
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2">
        {canWrite ? (
          draft ? (
            <>
              <button
                type="button"
                onClick={closeDraft}
                className="border border-accent px-2 py-1 font-cap text-xs uppercase tracking-cap text-accent"
              >
                close shape
              </button>
              <button
                type="button"
                onClick={cancelDraft}
                className="border border-rule px-2 py-1 font-cap text-xs uppercase tracking-cap text-text-secondary"
              >
                cancel
              </button>
              <span className="font-mono text-xs text-text-muted">{draft.length} points</span>
            </>
          ) : (
            <button
              type="button"
              onClick={startDraft}
              className="border border-accent px-2 py-1 font-cap text-xs uppercase tracking-cap text-accent"
            >
              new zone
            </button>
          )
        ) : (
          <p className="font-mono text-xs text-text-secondary">
            your role can read zones, not change them
          </p>
        )}

        <button
          type="button"
          onClick={() => setFrameNonce(Date.now())}
          className="border border-rule px-2 py-1 font-cap text-xs uppercase tracking-cap text-text-secondary"
        >
          new frame
        </button>

        {canWrite ? (
          <button
            type="button"
            onClick={() => void save()}
            disabled={saving || !dirty}
            className="border border-accent px-3 py-1 font-cap text-xs uppercase tracking-cap text-accent disabled:border-rule disabled:text-text-muted"
          >
            {saving ? 'saving' : dirty ? 'save zones' : 'saved'}
          </button>
        ) : null}
      </div>

      {notice ? <p className="mt-2 font-mono text-xs text-status-warning">{notice}</p> : null}

      {refusal ? (
        <p className="mt-2 text-sm text-status-error" role="alert">
          {refusal.error}
          {refusal.detail ? (
            <span className="ml-2 font-mono text-xs text-text-secondary">{refusal.detail}</span>
          ) : null}
        </p>
      ) : null}

      {zones.length === 0 ? (
        <p className="mt-3 text-sm text-text-secondary">
          No zones on this camera. Without a zone the worker records nothing to
          report — every rule set starts with one polygon.
        </p>
      ) : (
        <ul className="mt-3 space-y-3">
          {zones.map((zone) => (
            <ZoneRow
              key={zone.id}
              zone={zone}
              selected={zone.id === selected}
              canWrite={canWrite}
              onSelect={() => setSelected(zone.id)}
              onPatch={(change) => patch(zone.id, change)}
              onRemove={() => removeZone(zone.id)}
            />
          ))}
        </ul>
      )}
    </section>
  );
}

/** The zone whose polygon contains the point — the ray cast the worker runs
 * (`detector/nvr_detector/zones.py`), on this side, for a click. */
function zoneIdAt(zones: Zone[], point: [number, number]): string | null {
  const [px, py] = point;
  for (const zone of zones) {
    let inside = false;
    for (let i = 0, j = zone.polygon.length - 1; i < zone.polygon.length; j = i++) {
      const [xi, yi] = zone.polygon[i];
      const [xj, yj] = zone.polygon[j];
      const crosses = yi > py !== yj > py;
      if (crosses && px < ((xj - xi) * (py - yi)) / (yj - yi) + xi) {
        inside = !inside;
      }
    }
    if (inside) return zone.id;
  }
  return null;
}

function ZoneRow({
  zone,
  selected,
  canWrite,
  onSelect,
  onPatch,
  onRemove,
}: {
  zone: Zone;
  selected: boolean;
  canWrite: boolean;
  onSelect(): void;
  onPatch(change: Partial<Zone>): void;
  onRemove(): void;
}) {
  const degenerate = isDegenerate(zone.polygon);
  const label = zone.name || zone.id;

  return (
    <li className={`border p-3 ${selected ? 'border-accent' : 'border-rule'}`}>
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          onClick={onSelect}
          className="font-mono text-xs text-text-secondary"
          aria-label={`select ${label}`}
        >
          {zone.polygon.length} vertices
        </button>
        {degenerate ? (
          <span className="font-mono text-xs text-status-warning">
            the server will refuse this shape
          </span>
        ) : null}
        <label className="ml-auto flex items-center gap-1 font-mono text-xs text-text-secondary">
          <input
            type="checkbox"
            checked={zone.enabled}
            disabled={!canWrite}
            onChange={(event) => onPatch({ enabled: event.target.checked })}
          />
          enabled
        </label>
        {canWrite ? (
          <button
            type="button"
            onClick={onRemove}
            aria-label={`remove ${label}`}
            className="border border-rule px-1 font-mono text-xs text-text-secondary hover:border-status-error hover:text-status-error"
          >
            remove
          </button>
        ) : null}
      </div>

      <div className="mt-2 grid grid-cols-2 gap-2 md:grid-cols-4">
        <label className="col-span-2 block">
          <span className="font-mono text-[10px] uppercase text-text-muted">name</span>
          <input
            value={zone.name}
            disabled={!canWrite}
            onChange={(event) => onPatch({ name: event.target.value })}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">rule</span>
          <select
            value={zone.rule}
            disabled={!canWrite}
            onChange={(event) => onPatch({ rule: event.target.value })}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          >
            {ZONE_RULES.map((rule) => (
              <option key={rule} value={rule}>
                {rule}
              </option>
            ))}
          </select>
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">severity</span>
          <select
            value={zone.severity}
            disabled={!canWrite}
            onChange={(event) => onPatch({ severity: Number(event.target.value) })}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          >
            {[1, 2, 3].map((level) => (
              <option key={level} value={level}>
                {level}
              </option>
            ))}
          </select>
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">dwell s</span>
          <input
            type="number"
            min={1}
            value={zone.dwell_seconds}
            disabled={!canWrite}
            onChange={(event) => onPatch({ dwell_seconds: Number(event.target.value) })}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">min conf</span>
          <input
            type="number"
            min={0}
            max={1}
            step={0.05}
            value={zone.min_confidence}
            disabled={!canWrite}
            onChange={(event) => onPatch({ min_confidence: Number(event.target.value) })}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <label className="block">
          <span className="font-mono text-[10px] uppercase text-text-muted">min area</span>
          <input
            type="number"
            min={0}
            max={1}
            step={0.001}
            value={zone.min_area_ratio}
            disabled={!canWrite}
            onChange={(event) => onPatch({ min_area_ratio: Number(event.target.value) })}
            className="mt-0.5 w-full border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
          />
        </label>
        <div className="col-span-2 md:col-span-1">
          <span className="font-mono text-[10px] uppercase text-text-muted">classes</span>
          <div className="mt-0.5 flex flex-wrap gap-1">
            {DETECTION_CLASSES.map((cls) => {
              const on = zone.classes.includes(cls);
              return (
                <button
                  key={cls}
                  type="button"
                  disabled={!canWrite}
                  aria-pressed={on}
                  onClick={() =>
                    onPatch({
                      classes: on
                        ? zone.classes.filter((c) => c !== cls)
                        : [...zone.classes, cls],
                    })
                  }
                  className={`border px-1 font-mono text-[10px] ${
                    on ? 'border-accent text-accent' : 'border-rule text-text-muted'
                  }`}
                >
                  {cls}
                </button>
              );
            })}
          </div>
        </div>
      </div>
    </li>
  );
}
