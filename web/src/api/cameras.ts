/**
 * The camera row and its zone rule set, as the server stores them.
 *
 * `models.Zone` in `server/models/camera.go` is the shape, and the console writes
 * it back through `PUT /api/v1/cameras/:id/zones` unchanged — the card's
 * "the exact `ZonesJSON` shape of Step 1". Nothing here renames a field, and
 * nothing here re-implements the validation: `server/api/zones.go` says *"One
 * validator, three readers (console, API, worker)"*, and a console that guessed
 * which polygons are legal would be a fourth reader with its own opinion. What
 * lives here is the **vocabulary** (the rules, the classes, the limits) and the
 * one thing only the console can do: mint a zone id.
 */

/** The rules a zone can run — `ZoneRules` in `server/models/camera.go`. */
export const ZONE_RULES = ['intrusion', 'loiter'] as const;
export type ZoneRule = (typeof ZONE_RULES)[number];

/** The classes the worker will act on. The rest of COCO is dropped
 * (`IGNORED_CLASSES`, `detector/nvr_detector/model.py`), and the API refuses a
 * zone that names one, with the list in the refusal. */
export const DETECTION_CLASSES = [
  'person',
  'bicycle',
  'car',
  'motorcycle',
  'bus',
  'truck',
  'dog',
  'cat',
  'bear',
] as const;

/** The bounds `Zone.Validate` and `normalizePolygon` enforce. Named here so the
 * editor can *advise* — "3 points close a shape" — without pretending to be the
 * authority. The authority answers 422, and its sentence is what the operator
 * reads. */
export const ZONE_LIMITS = {
  minVertices: 3,
  maxVertices: 32,
  maxZones: 16,
  maxSeverity: 3,
} as const;

/** `models.Zone`. `polygon` is a list of `[x, y]` pairs normalised to `0..1` of
 * the detection frame, which is why a camera changing its resolution cannot
 * silently invalidate a rule. */
export interface Zone {
  id: string;
  name: string;
  polygon: number[][];
  classes: string[];
  rule: string;
  dwell_seconds: number;
  severity: number;
  min_confidence: number;
  min_area_ratio: number;
  enabled: boolean;
}

/** `cameraView` in `server/api/cameras.go`. */
export interface CameraView {
  id: string;
  slug: string;
  name: string;
  source_uri: string;
  record_mode: string;
  detection_enabled: boolean;
  detection_fps: number;
  detection_width: number;
  detection_height: number;
  sensitivity: number;
  min_area_ratio: number;
  zones: Zone[];
  local_retention_days: number;
  remote_retention_days: number;
  active: boolean;
  created_at: string;
  updated_at: string;
}

/** The fields `cameraInput` accepts. Every one is a pointer, so a write sends
 * only what the operator touched and the server's defaults own the rest. */
export interface CameraWrite {
  slug?: string;
  name?: string;
  source_uri?: string;
  record_mode?: string;
  detection_enabled?: boolean;
  detection_fps?: number;
  detection_width?: number;
  detection_height?: number;
  sensitivity?: number;
  min_area_ratio?: number;
  local_retention_days?: number;
  remote_retention_days?: number;
  active?: boolean;
}

export const RECORD_MODES = ['continuous', 'events', 'off'] as const;

export function clamp01(value: number): number {
  if (!Number.isFinite(value)) return 0;
  return Math.min(1, Math.max(0, value));
}

/**
 * A pixel on the picture, as a position in the rule set.
 *
 * This is the whole coordinate contract in one function: the operator's pointer
 * is in CSS pixels of whatever the picture happens to be rendered at, and the
 * stored vertex is `0..1` of the detection frame. Everything the editor draws is
 * derived from the stored number, never the reverse, so a resized window cannot
 * drift a rule.
 */
export function toNormalized(
  box: { left: number; top: number; width: number; height: number },
  clientX: number,
  clientY: number,
): [number, number] {
  if (box.width <= 0 || box.height <= 0) return [0, 0];
  return [clamp01((clientX - box.left) / box.width), clamp01((clientY - box.top) / box.height)];
}

/**
 * A zone id the console has never issued for this installation.
 *
 * The server requires an id and never mints one, and it only checks for
 * duplicates *within the array it was sent* — so reusing the id of a zone that
 * was deleted is accepted. That is not harmless: event rows store zone **ids**
 * (`server/api/events.go` resolves them to names from the camera row on every
 * read), so a new rule that inherits a dead rule's id silently relabels every
 * event that already fired under it. The proof surface would lie about which
 * rule fired, months later.
 *
 * A monotone time component is what prevents it: an id minted now can never
 * equal one minted before, so a deleted rule's id is never handed out again.
 */
export function mintZoneId(existing: Zone[]): string {
  const taken = new Set(existing.map((zone) => zone.id));
  const stamp = Date.now().toString(36);
  for (let n = 1; ; n += 1) {
    const candidate = `z-${stamp}-${n}`;
    if (!taken.has(candidate)) return candidate;
  }
}

/** A zone with the server's own defaults, so a drawn shape is a valid rule the
 * moment it is closed and the operator's first edit is a name, not a form. */
export function blankZone(id: string, polygon: number[][]): Zone {
  return {
    id,
    name: '',
    polygon,
    classes: ['person'],
    rule: 'intrusion',
    dwell_seconds: 30,
    severity: 2,
    min_confidence: 0,
    min_area_ratio: 0,
    enabled: true,
  };
}

/** The shoelace sum, the same test `server/api/zones.go` runs: a polygon with no
 * area is a rule an operator believes is armed and the worker can never fire.
 * Used here to *warn*, never to refuse — the refusal is the server's. */
export function enclosesArea(polygon: number[][]): boolean {
  if (polygon.length < 3) return false;
  let area = 0;
  for (let i = 0; i < polygon.length; i += 1) {
    const [x1, y1] = polygon[i];
    const [x2, y2] = polygon[(i + 1) % polygon.length];
    area += x1 * y2 - x2 * y1;
  }
  return area !== 0;
}
