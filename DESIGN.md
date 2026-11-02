# nvrWay Console — Design Contract

<!-- skeleton: S0-02 (2026-11-02). Headings only; the prose lands with S9-10 and
     the console work of Step 8. Each heading names the question it must answer,
     so a reviewer can tell an empty slot from an answered one. -->

Direction: _(to be chosen with the impeccable Operate pass on the Step 8 console;
state the seed and what the category default was, the way OurWay's DESIGN.md
opens.)_

Scope: the web console — shell, live view, event timeline, camera + zone
editor, settings, audit.

---

## 1. Thesis

The one idea that decides what every screen is for. For nvrWay that is a
claim about the operator's job — "is anything happening at my doors right
now, and what did it look like" — not a wall of gauges. State the thesis and
what category default it replaces.

## 2. The world

What exists on the screen and what it means: cameras, segments, events,
zones, storage tiers, the upload queue, the live feed. The vocabulary a
first-time operator must not have to learn from a manual.

## 3. Type

Type face, scale ramp, and the rule for where numbers go. The event card,
the timeline strip and the retention table all carry dense numerals — the
ramp must make them comparable at a glance.

## 4. Shell

Navigation, role-gated tabs (`viewer` sees no Settings), the header that
carries the live/stale state, and how a page is reached from a WebSocket
event.

## 5. Panel anatomy

The live panel, the event detail drawer, the zone editor overlay: what
goes in the header, the body and the footer of each, and the density each
one is allowed.

## 6. Controls

Buttons, filters, chips, toggles, destructive confirmations (delete event,
disable backend), and the state a control shows while its request is in
flight.

## 7. Motion

Transitions and their durations: the LIVE row appearing, a new event card,
the zone polygon vertex drag. Motion is for state change, not decoration.

## 8. State legibility (the operating contract)

**The question this section must answer: what does the console do when the
feed is stale, the recorder is offline, the upload queue is backlogged, or
the server clock and the camera's clock disagree?** The server clock owns
event time, so the console shows server-assigned times and never a
worker-side timestamp — say what the operator sees for each degraded state
and what they may not be shown.

## 9. Scale

4–8 cameras, one installation, one browser tab per operator. The layout
rule for 1 camera, 4 cameras, 8 cameras, and the rule that breaks first at
16 — stated, not discovered.

## 10. i18n

v1 ships one locale (English). State that as a decision with its cost, and
the rule a later locale must follow (every string through `t(...)`,
lockstep across locales — OurWay's convention).

## 11. Provenance

Where the product's facts come from: which screens are driven by which
endpoint, which numbers come from the server clock, which are computed in
the browser (zone geometry is normalized `0..1`; the console must not
re-derive event rules).

## 12. Open debts

The list of known design gaps with the card that closes each — the live
player's WebRTC fallback (`hls.js`), the remote clip that cannot seek, the
timeline strip that has no zoom.
