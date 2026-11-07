/**
 * The settings surface, as the server speaks it.
 *
 * The shapes are copies of the Go views — `storageBackendView` and
 * `storageJobView` in `server/api/storage.go`, `webhookView` and
 * `webhookDeliveryView` in `server/api/settings.go`, `oidcProviderView` in
 * `server/api/oidc_settings.go`, and the retention answer in
 * `server/api/retention.go`. Nothing here renames a field on the way in, for
 * the reason `api/events.ts` states: a field the server adds arrives instead of
 * being dropped, and a field it renames fails a test rather than drawing a
 * blank column.
 *
 * The secrets are the point of the whole module. Every credential column
 * arrives as a `*_set` flag and nothing else — the server's views do not carry
 * the values, so a form here cannot accidentally render one, and the write
 * payloads are all-optional for the mirror reason: **an omitted secret keeps
 * the stored value, an explicitly empty one clears it** (the pointer rule in
 * `applyStorageInput` / `applyWebhookInput`). A form that spread every field
 * into its body would wipe a password the operator never touched, so the write
 * helpers below send only what changed, and the suites pin that.
 */

/** `models.StorageKinds` — the four destinations the storage package opens. */
export const STORAGE_KINDS = ['s3', 'webdav', 'sftp', 'fs'] as const;
export type StorageKind = (typeof STORAGE_KINDS)[number];

/** The four named connection states (`server/storage/storage.go` sentinels)
 * plus `connection_failed`, the honest absence of a name. They are rendered
 * verbatim: the card's exit criterion is that these words reach the operator,
 * because each one is a different fix. */
export const CONNECTION_STATES = [
  'ok',
  'discovery_unreachable',
  'bad credentials',
  'sftp host key changed',
  'bucket missing',
  'connection_failed',
] as const;

/** `storageBackendView` in `server/api/storage.go`. No secret is in it; the
 * `*_set` flags are all the API concedes, per column, because the form has one
 * field per column and needs to know which ones already hold something. */
export interface StorageBackendView {
  id: string;
  name: string;
  kind: StorageKind;
  endpoint: string;
  region: string;
  bucket: string;
  prefix: string;
  username: string;
  insecure_tls: boolean;
  root_path: string;
  enabled: boolean;
  is_default: boolean;

  secret_set: boolean;
  password_set: boolean;
  access_key_set: boolean;
  secret_key_set: boolean;
  private_key_path_set: boolean;

  created_at: string;
  updated_at: string;
}

/** `storageBackendInput` — pointers throughout on the server, optionals here,
 * for the same reason: omitted keeps, empty clears, and only the payload shape
 * tells them apart. */
export interface StorageBackendWrite {
  name?: string;
  kind?: StorageKind;
  endpoint?: string;
  region?: string;
  bucket?: string;
  prefix?: string;
  username?: string;
  password?: string;
  access_key?: string;
  secret_key?: string;
  private_key_path?: string;
  insecure_tls?: boolean;
  root_path?: string;
  enabled?: boolean;
  is_default?: boolean;
}

/** The test route's answer. `ok: true` carries the kind and name back; a
 * refusal carries the named state and the driver's own sentence. */
export type StorageTestResult =
  | { ok: true; kind: StorageKind; name: string }
  | { ok: false; error: string; reason: string };

/** `store.QueueCounts` — the queue in one line, both on the REST read and
 * inside the sweeper's `storage.queue` WS report. */
export interface QueueCounts {
  pending: number;
  uploading: number;
  failed: number;
  uploaded: number;
  last_error: string;
}

/** `storageJobView` in `server/api/storage.go`. `last_error` travels because
 * the queue page is where a failed upload is discovered, and a row that says
 * `failed` without saying why sends the operator to the logs. */
export interface StorageJobView {
  id: string;
  kind: string;
  action: string;
  state: string;
  target: string;
  remote_key: string;
  backend_id: string;
  backend_name: string;
  attempts: number;
  bytes: number;
  last_error: string;
  next_attempt_at: string;
  created_at: string;
  updated_at: string;
}

export interface StorageQueueResponse {
  jobs: StorageJobView[];
  count: number;
  state: string;
  limit: number;
  offset: number;
  truncated: boolean;
  /** Totals for the whole queue, not the page — the headline numbers are
   * "what is stuck". */
  counts: QueueCounts;
}

/** `webhookView` in `server/api/settings.go`. The signing secret has no path
 * out: `secret_set` is all the API concedes. `last_error` and
 * `last_delivered_at` are the one-line answer to "is this webhook working". */
export interface WebhookView {
  id: string;
  name: string;
  url: string;
  camera_id: string;
  events: string[];
  enabled: boolean;
  secret_set: boolean;
  last_error: string;
  last_delivered_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface WebhookWrite {
  name?: string;
  url?: string;
  /** Write-only. Omit to keep the stored secret; send `''` to clear it. */
  secret?: string;
  camera_id?: string;
  events?: string[];
  enabled?: boolean;
}

/** `webhookDeliveryView` — one attempt. The history is the answer to "why did
 * my hook fail": "it answered 500 twice then 200" and "it has never answered"
 * are different problems the last row alone cannot tell apart. */
export interface WebhookDeliveryView {
  id: string;
  event: string;
  status: string;
  status_code: number;
  attempt: number;
  error: string;
  delivered_at: string | null;
  created_at: string;
  payload: unknown;
}

/** `oidcProviderView` in `server/api/oidc_settings.go`. `redirect_uri` is the
 * server's own computation — the string the operator whitelists in the IdP —
 * so the page shows it and never lets anyone type one. */
export interface OIDCProviderView {
  id: string;
  name: string;
  type: string;
  issuer: string;
  discovery_url: string;
  client_id: string;
  secret_set: boolean;
  scope: string;
  enabled: boolean;
  redirect_uri: string;
  auth_url: string;
  token_url: string;
  user_info_url: string;
  jwks_uri: string;
  discovery_state: string;
  pkce_supported: boolean;
  discovered_at: string;
  link_policy: string;
  allowed_domains: string;
  claims_email: string;
  claims_username: string;
  claims_name: string;
  claims_groups: string;
  require_verified_email: boolean;
  sort_order: number;
}

export interface OIDCProviderWrite {
  name?: string;
  issuer?: string;
  discovery_url?: string;
  client_id?: string;
  /** Write-only, like every other secret here. */
  client_secret?: string;
  scope?: string;
  enabled?: boolean;
  link_policy?: string;
  allowed_domains?: string;
  require_verified_email?: boolean;
  claims_email?: string;
  claims_username?: string;
  claims_name?: string;
  claims_groups?: string;
  sort_order?: number;
}

/** The named discovery states (`server/sso/discovery.go`). A save that fails
 * discovery answers 422 with one of these in `discovery`, and the page renders
 * the name: `discovery_unreachable` and `issuer_mismatch` are different fixes,
 * and a page that flattened them into "save failed" reproduces the support
 * ticket the route exists to prevent. */
export const DISCOVERY_STATES = [
  'ok',
  'discovery_unreachable',
  'discovery_document',
  'issuer_mismatch',
  'no_code_flow',
] as const;

/** The 422 the OIDC create/update routes answer when the issuer refuses. */
export interface OIDCDiscoveryRefusal {
  error: string;
  discovery: string;
  discovery_reason: string;
}

/** `RetentionSettings` in `server/api/retention.go`. Read-only by contract:
 * the environment is the policy's only author, and `author` says so. */
export interface RetentionView {
  local_days: number;
  remote_days: number;
  remote_forever: boolean;
  event_days: number;
  unuploaded_protection_days: number;
  webhook_delivery_days: number;
  audit_days: number;
  job_days: number;
  author: string;
}

/** The trigger this build sends (`webhooks.TriggerClose`). The route refuses
 * any other name with the sentence "this build sends only …", so the form
 * offers exactly this one rather than a list the server would reject. */
export const WEBHOOK_TRIGGERS = ['event.close'] as const;

/**
 * The sealed secret columns each kind actually has, so the form draws one
 * write-only field per column the row seals and the `*_set` flags have a name
 * to sit beside. `username` is absent on purpose: it is a plain column the
 * view carries verbatim, not a sealed one. `fs` has none — a directory is
 * addressed by its path, and its row holds no credential to keep.
 */
export const SECRET_FIELDS: Record<StorageKind, (keyof StorageBackendWrite)[]> = {
  s3: ['access_key', 'secret_key'],
  webdav: ['password'],
  sftp: ['password', 'private_key_path'],
  fs: [],
};

/** The `*_set` flag beside each secret field, for the "set / unset" rendering. */
export const SECRET_SET_FLAGS: Record<string, keyof StorageBackendView> = {
  password: 'password_set',
  access_key: 'access_key_set',
  secret_key: 'secret_key_set',
  private_key_path: 'private_key_path_set',
};

/**
 * The diff between a form draft and the stored row, in write-payload shape.
 *
 * This is where the omitted-vs-empty rule is enforced for the non-secret
 * fields: a field nobody touched is **absent** from the body, not re-sent, so
 * a save of the name cannot race a concurrent edit of the endpoint. Secrets
 * are the exception the caller controls: `secretFields` names the ones the
 * operator actually typed (or cleared), and only those travel.
 */
export function storageWrite<T extends Record<string, unknown>>(
  draft: T,
  stored: Partial<T>,
  secretFields: (keyof T)[],
): Partial<T> & Record<string, unknown> {
  const body: Record<string, unknown> = {};
  for (const key of Object.keys(draft) as (keyof T)[]) {
    if (secretFields.includes(key)) {
      const value = draft[key];
      // A secret travels only when it is non-empty: an untouched write-only
      // input is empty, and empty must mean "keep", never "clear". Clearing
      // is a deliberate act with its own button, not a side effect of Save.
      if (typeof value === 'string' && value !== '') body[key as string] = value;
      continue;
    }
    if (draft[key] !== stored[key]) body[key as string] = draft[key];
  }
  return body as Partial<T> & Record<string, unknown>;
}
