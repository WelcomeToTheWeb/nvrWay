/**
 * Media the browser cannot fetch by itself.
 *
 * `server/api/handlers.go` is blunt about it: "The bearer header is the only REST
 * credential. `?token=` is accepted on the WebSocket" and nowhere else. So an
 * `<img src="/api/v1/events/x/snapshot">` or a `<video src="…/clip">` is
 * answered **401** — a browser attaches no header to a subresource. The bytes
 * have to come through `api/client.ts`, which attaches the bearer and renews it,
 * and be handed to the element as an object URL.
 *
 * That costs the browser's HTTP cache and buys two things worth more than it.
 * The first is the drawer's most important sentence: when the media is gone the
 * route answers `404 {"error":"clip not available"}`, and a page that fetched
 * the bytes reads that body and renders **the server's own words** instead of
 * the browser's broken-player icon. The second is that a picture an event came
 * with never changes — the server says so outright (`Cache-Control: private,
 * max-age=3600`, "written once and never rewritten") — so it is worth holding
 * for the tab, which is what the cache below is: the same fact the response
 * header states, honoured on this side.
 */
import { useEffect, useState } from 'react';

import axios from 'axios';

import client from './client';

export type MediaState =
  /** Nothing to ask for: the row says there is no clip, so nothing is requested. */
  | { kind: 'none' }
  | { kind: 'reading' }
  | { kind: 'ready'; url: string }
  /** The route said the event stands and its media does not exist. `reason` is
   * the server's sentence — `clip not available`, `snapshot not available` —
   * never a paraphrase of it. */
  | { kind: 'absent'; reason: string }
  /** Anything else: a 502 from an unreachable tier, a 401 the renewal could not
   * fix, a network that is down. Also the server's words where it has them. */
  | { kind: 'failed'; reason: string };

/** One entry per path, holding the in-flight promise so twenty cards asking for
 * the same picture cause one request, and the settled URL so a page revisited
 * later asks for none. Object URLs are never revoked: they are the size of the
 * media the operator has actually looked at, and the tab is their lifetime. */
const cache = new Map<string, Promise<MediaState>>();

async function load(path: string): Promise<MediaState> {
  try {
    const response = await client.get<Blob>(path, { responseType: 'blob' });
    const url = URL.createObjectURL(response.data);
    return { kind: 'ready', url };
  } catch (error) {
    const reason = await refusalWords(error);
    if (axios.isAxiosError(error) && error.response?.status === 404) {
      return { kind: 'absent', reason };
    }
    return { kind: 'failed', reason };
  }
}

/** The server's sentence out of an error whose body arrived as a Blob, because
 * `responseType: 'blob'` types every response that way — the 404 included. */
async function refusalWords(error: unknown): Promise<string> {
  if (!axios.isAxiosError(error)) return 'the media could not be read';
  const data = error.response?.data;
  if (data instanceof Blob) {
    try {
      const parsed = JSON.parse(await data.text()) as { error?: string };
      if (parsed.error) return parsed.error;
    } catch {
      /* A body that is not JSON says nothing useful; the status does below. */
    }
  } else if (data && typeof data === 'object') {
    const parsed = data as { error?: string };
    if (parsed.error) return parsed.error;
  }
  if (error.response) return `the server answered ${error.response.status}`;
  return 'the media could not be read';
}

function remember(path: string): Promise<MediaState> {
  const held = cache.get(path);
  if (held) return held;
  const started = load(path);
  cache.set(path, started);
  // A failed read is not a fact about the media, it is a moment. Forget it so the
  // next look asks again — a tier that was briefly unreachable should not stay
  // unreachable for the rest of the tab.
  void started.then((state) => {
    if (state.kind === 'failed') cache.delete(path);
  });
  return started;
}

/**
 * The bytes at `path`, or the reason there are none. Pass `null` to ask for
 * nothing — a row that says `has_clip: false` should not spend a request to be
 * told what it already knows, and the difference between "this event never had a
 * clip" and "the clip is gone" is worth keeping in the page.
 *
 * `path` is a client-relative route (`/v1/events/<id>/clip`), so it goes through
 * the same `/api` root as every other request and the bundle still names no host.
 */
export function useMediaUrl(path: string | null): MediaState {
  const [state, setState] = useState<MediaState>(path ? { kind: 'reading' } : { kind: 'none' });

  useEffect(() => {
    if (!path) {
      setState({ kind: 'none' });
      return;
    }
    let live = true;
    setState({ kind: 'reading' });
    void remember(path).then((next) => {
      if (live) setState(next);
    });
    return () => {
      live = false;
    };
  }, [path]);

  return state;
}

/** Test seam: drop the held URLs so one test's picture is not another's. */
export function __resetMediaCache(): void {
  cache.clear();
}
