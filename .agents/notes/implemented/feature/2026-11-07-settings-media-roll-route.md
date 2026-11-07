# Agent Note: settings-media-roll-route

Status: implemented

## Problem

The clip builder cuts a clip from the event window padded by PRE_ROLL_SECONDS/POST_ROLL_SECONDS, but the console was never told those numbers, so the drawer's segment strip drew a narrower band than the clip under it and could only apologise ("the server pads it by the roll settings and this console is not told them") — true everywhere, useful nowhere.

## Decision

`GET /api/v1/settings/media` (mounted beside `/live` on the plain read gate, any signed-in role) answers `pre_roll_seconds`, `post_roll_seconds`, `segment_seconds` verbatim from the same `Cfg` fields the clip builder and recorder read in-process. `web/src/api/events.ts` gains `MediaSettings` and `clipWindow(event, roll)` (started − pre, ended + post, whole-second ISO format; an open event pads its start and names no end). `EventDetail.tsx` fetches the route before the segment request, pads by it, and the caption names the padded window ("the window the clip was cut from: Ns of pre-roll…"); when the route fails the strip falls back to the unpadded event window and keeps S8-06's apology sentence — a tested behaviour, not a gap. `SettingsPage.tsx` RetentionTab adds three rows (clip pre-roll / clip post-roll / segment length) fetched from the same route, restating no default. docs/api.md Settings section and docs/configuration.md's roll row name the route.
## Alternatives considered

Extending `GET /api/settings/retention`: lost — it is mounted on the admin group and the event drawer is a viewer's screen; a viewer must pad their own strip, and moving the retention route down would leak the sweep policy to every role. `clip_from`/`clip_to` on the event view: lost — exact truth for one drawer only; the retention panel still needs the roll itself and segment_seconds would have no door. A `pad` query parameter on the segments route: lost as the card pre-rejected it — a server-side policy becomes a parameter every page must remember.
## Consequences

Cost: the drawer now makes one more request before the manifest (serial, so the strip appears marginally later), and the retention panel reads two routes. Buys: the strip's band is the footage the clip contains on every installation, the apology survives only where the roll genuinely cannot be read, and one number has one door (criterion: recorder status line and strip agree because both read `Cfg.SegmentSeconds`). Follow-up: any future console surface that draws clip windows should read settings/media rather than hard-coding 8/15.

