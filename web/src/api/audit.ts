/**
 * The ledger, as the server speaks it.
 *
 * The shape is `auditView` in `server/api/audit.go` and the filter names are
 * the query parameters `ListAudit` reads — nothing here renames a field on the
 * way in, so a field the server adds arrives instead of being dropped.
 *
 * The vocabulary that matters is `outcome`: the ledger records refusals as
 * rows, and the four words are the server's (`models/audit.go`), rendered from
 * the status code — `refused` is a 4xx, `not_found` a 404, `error` a 5xx. The
 * console draws those words and a colour, and invents no adjective for them:
 * `refused` on a viewer's delete attempt is the row that settles an argument,
 * and it must read exactly like the filter that finds it.
 */

/** The four stored outcomes — the same four the `outcome` filter validates. */
export const AUDIT_OUTCOMES = ['ok', 'refused', 'not_found', 'error'] as const;

export type AuditOutcome = (typeof AUDIT_OUTCOMES)[number];

/** One act. `actor_username` and `target_id` are text, not ids, and can be
 * empty — a failed login has no authenticated account, and a refusal before
 * the target resolved has no entity. The row still answers who knocked. */
export interface AuditView {
  id: string;
  actor_user_id: string;
  actor_username: string;
  /** Space-joined as the server stored it; the row shows it verbatim. */
  actor_roles: string;
  /** `local` or a provider slug — kept so the answer survives the provider. */
  actor_provider: string;
  action: string;
  target_type: string;
  target_id: string;
  outcome: AuditOutcome | '';
  /** Parsed server-side from the JSON text column; null when the row has none. */
  details: Record<string, unknown> | null;
  source_ip: string;
  created_at: string;
}

export interface AuditListResponse {
  audits: AuditView[];
  count: number;
  limit: number;
  offset: number;
  truncated: boolean;
}

/** The ledger's query, in the server's parameter names — the same rule the
 * event wall follows: the filter lives in the address bar, so a filtered
 * ledger is pasteable and survives a reload. */
export const AUDIT_FILTER_KEYS = ['actor', 'action', 'outcome', 'target_id'] as const;

export type AuditFilterKey = (typeof AUDIT_FILTER_KEYS)[number];

export type AuditFilters = Partial<Record<AuditFilterKey, string>>;

export function auditFiltersFromParams(params: URLSearchParams): AuditFilters {
  const filters: AuditFilters = {};
  for (const key of AUDIT_FILTER_KEYS) {
    const value = params.get(key);
    if (value) filters[key] = value;
  }
  return filters;
}

export function auditFiltersToParams(filters: AuditFilters): URLSearchParams {
  const params = new URLSearchParams();
  for (const key of AUDIT_FILTER_KEYS) {
    const value = filters[key];
    if (value) params.set(key, value);
  }
  return params;
}
