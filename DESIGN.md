---
name: nvrWay Console
description: Self-hosted NVR console — the home's record as a Metro tile mosaic, day and night shipped equal.
colors:
  # the world's fields — these four hues are identical in both themes
  field-cyan: "#00b7c3"
  field-cyan-ink: "#00282c"
  field-red: "#b00e00"
  field-red-ink: "#ffffff"
  field-green: "#007a33"
  field-green-ink: "#ffffff"
  field-amber: "#e8a112"
  field-amber-ink: "#241a00"
  # night ground (the console's primary reading scene: a dim room)
  night-ground: "#000000"
  night-surface: "#0d0d0d"
  night-tile: "#161616"
  night-rule: "#333333"
  night-ink: "#f2f2f2"
  night-ink-secondary: "#c2c2c2"
  night-ink-muted: "#9a9a9a"
  # day ground
  day-ground: "#f2f2f2"
  day-surface: "#e4e4e4"
  day-tile: "#ffffff"
  day-rule: "#c7c7c7"
  day-ink: "#000000"
  day-ink-secondary: "#3d3d3d"
  day-ink-muted: "#5c5c5c"
  # the accent that marks the operator's own drawing; at night it is field-cyan itself
  accent-day: "#00727a"
  accent-lit-night: "#4fd6e0"
  # the two theme-bound trouble text hues that are not already a field
  error-night: "#ff4b3e"
  warning-day: "#7a5800"
  # the online status word at night rides a lighter sibling of the green field
  online-night: "#2ea043"
  # the heavier hairline that tops a strip (at night it is the muted ink value)
  rule-strong-day: "#8a8a8a"
  # sev-1 and sev-2 reuse the muted ink and the amber field; only the top of the scale has its own hue
  severity-3-day: "#a8480a"
  severity-3-night: "#ff8b3d"
typography:
  wordmark:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "2.5rem"
    fontWeight: 300
    lineHeight: "1"
    letterSpacing: "-0.025em"
  display:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.875rem"
    fontWeight: 300
    lineHeight: "2.25rem"
    letterSpacing: "0.01em"
  numeral:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "3rem"
    fontWeight: 300
    lineHeight: "1"
  title:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1rem"
    fontWeight: 600
    lineHeight: "1.5rem"
    letterSpacing: "0.01em"
  body:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.875rem"
    fontWeight: 400
    lineHeight: "1.25rem"
  label:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.75rem"
    fontWeight: 600
    lineHeight: "1rem"
    letterSpacing: "0.01em"
  data:
    fontFamily: '"JetBrains Mono", ui-monospace, SFMono-Regular, monospace'
    fontSize: "0.75rem"
    fontWeight: 400
    lineHeight: "1rem"
  micro-label:
    fontFamily: '"JetBrains Mono", ui-monospace, SFMono-Regular, monospace'
    fontSize: "0.625rem"
    fontWeight: 400
    lineHeight: "1rem"
rounded:
  none: "0"
  full: "0"
spacing:
  "1": "4px"
  "2": "8px"
  "3": "12px"
  "4": "16px"
  "6": "24px"
  "8": "32px"
components:
  nav-tile-active:
    backgroundColor: "{colors.field-cyan}"
    textColor: "{colors.field-cyan-ink}"
    typography: "{typography.label}"
    rounded: "{rounded.none}"
    padding: "8px 12px"
  nav-tile:
    backgroundColor: "transparent"
    textColor: "{colors.night-ink-secondary}"
    typography: "{typography.label}"
    rounded: "{rounded.none}"
    padding: "8px 12px"
  button-accent:
    backgroundColor: "{colors.accent-day}"
    textColor: "{colors.day-tile}"
    typography: "{typography.label}"
    rounded: "{rounded.none}"
    padding: "4px 12px"
  button-accent-outline:
    backgroundColor: "transparent"
    textColor: "{colors.accent-day}"
    typography: "{typography.label}"
    rounded: "{rounded.none}"
    padding: "4px 12px"
  button-destructive:
    backgroundColor: "{colors.field-red}"
    textColor: "{colors.day-ground}"
    typography: "{typography.label}"
    rounded: "{rounded.none}"
    padding: "4px 12px"
  banner-stale:
    backgroundColor: "{colors.field-amber}"
    textColor: "{colors.field-amber-ink}"
    typography: "{typography.body}"
    rounded: "{rounded.none}"
    padding: "6px 24px"
  banner-refused:
    backgroundColor: "{colors.field-red}"
    textColor: "{colors.field-red-ink}"
    typography: "{typography.body}"
    rounded: "{rounded.none}"
    padding: "6px 24px"
  tile:
    backgroundColor: "{colors.night-tile}"
    textColor: "{colors.night-ink}"
    rounded: "{rounded.none}"
  tile-header-live:
    backgroundColor: "{colors.field-cyan}"
    textColor: "{colors.field-cyan-ink}"
    typography: "{typography.title}"
    rounded: "{rounded.none}"
    padding: "8px 12px"
  input-field:
    backgroundColor: "{colors.day-surface}"
    textColor: "{colors.day-ink}"
    typography: "{typography.data}"
    rounded: "{rounded.none}"
    padding: "4px 8px"
  chip-filter:
    backgroundColor: "transparent"
    textColor: "{colors.accent-day}"
    typography: "{typography.data}"
    rounded: "{rounded.none}"
    padding: "0px 6px"
  severity-ring-top:
    backgroundColor: "{colors.field-red}"
    textColor: "{colors.field-red-ink}"
    typography: "{typography.data}"
    rounded: "{rounded.none}"
    size: "24px"
---

# Design System: nvrWay Console

## Overview

**Creative North Star: "The Concourse Record"**

The home's surveillance record is a transit concourse at night. Every camera, every event, and every system fact is a flat colour field set into a dark wall, separated from its neighbours by the ground showing through the gap, and each field states its own condition out loud: cyan means live, amber means degraded, red means down, green means a segment closed. Nothing is decorated. Nothing floats. A tile does not need an icon, a badge, or a spinner to be understood, because the colour is the fact and the word beside it is the sentence.

The console refuses the category default of the surveillance control wall — the dark sidebar, the gauge cluster, the single neon accent doing all the work. nvrWay's operator is a homelab owner checking their own doors in a dim room, and what they need is not telemetry but legibility: which cameras are live, which events await review, whether the system is healthy, in one glance, from a phone or a desk. Density is welcome; ambiguity is not. So the system is typographic rather than pictorial — one family at four weights, a monospace for anything the operator compares, and a palette where every hue is a claim about the machine rather than a mood.

Day and night are first-class and honestly equal. The room decides first (`prefers-color-scheme`), the operator's toggle outranks it, and the choice is written before first paint so neither theme ever flashes. The four field hues do not move between themes — only the ground, the ink, and the rules do. A red tile is the same red at 2am and at noon, because a camera being down is not a thing that changes with the light.

**Direction:** Metro Tiles (`digital-design-canon-metro-typographic-tiles`), direction seed `c0b153b8258891cb` — chosen over the roll by the user and held against the product's clarity.

**Key Characteristics:**
- Flat saturated fields, separated by hairline gaps; zero radius, zero shadow, zero gradient.
- State lives in the field colour; secondary text tints from that field's own ink, never gray-on-colour.
- One voice: Archivo at 300/400/600, with JetBrains Mono for every comparable numeral.
- Exactly two trouble hues (amber, red); cyan, green, white and grey are state, not alarm.
- Every loading, absent, and failed state is a written sentence in the same box — never a spinner, never a skeleton, never a broken-image icon.
- One signature move: the tile flip. Everything else changes instantly.

## Colors

The palette is a signal set, not a colour scheme: eight hues, each one a fact the machine can assert, on a ground that goes from black to white without the hues moving.

### Primary
- **Concourse Cyan** (field-cyan): the live field and the operator's own mark. It is the header band of a playing camera tile, the lit nav tile for the page you are on, the selection colour, the caret, the focus ring, and the 20% wash the zone editor paints over a polygon the operator drew. Its ink is **Cyan Deep** (field-cyan-ink), a near-black teal — the only text colour allowed on a cyan field.
- **Signal Cyan, Day-Safe** (accent-day): the same hue pulled dark enough to clear AA as text on white. It carries links, the `open`/`live` word, active filter chips, and every primary action's border. At night the accent *is* Concourse Cyan; there is no second cyan.

### Secondary
- **Alert Red** (field-red) with **Red Ink** (field-red-ink): a camera the server calls down, a feed that has stopped opening, a severity-3 event, the destructive delete. Its ink is pure white — the only field in the system that takes white.
- **Warning Amber** (field-amber) with **Amber Ink** (field-amber-ink): a stream still opening, a feed retrying, a degraded backend, a severity-2 event. Its ink is a near-black brown, never white.

### Tertiary
- **Closed-Segment Green** (field-green) with **Green Ink** (field-green-ink): the recorder proved it closed a segment inside three of its own lengths. It is the system's only green *field*, and it is always a past-tense fact, never a promise about the next one.
- **Online Green** (online-night): the lighter sibling the `online` status word rides at night so it clears a black ground. The green *field* never moves between themes; only this word does.

### Neutral
- **Night Concourse** (night-ground) / **Day Concourse Stone** (day-ground): the two equal grounds. Tiles sit *in* them; the gap between tiles is ground, which is why no tile needs a border.
- **Night Surface** / **Day Surface**: the recessed step — hover fields, input backgrounds, the absent-media box.
- **Night Tile** / **Day Tile**: the raised step — a tile, a card, a drawer, and the ink of a filled accent button.
- **Night Rule** / **Day Rule**: 1px hairlines that rule rows inside a framed list; **Rule Strong, Day** (rule-strong-day) is the heavier hairline that tops a strip, and at night that heavier hairline is simply the muted-ink value.
- **Night Ink** / **Night Ink Secondary** / **Night Ink Muted** (and the day pair): primary text, secondary text, and the muted tier — which doubles as the offline grey, because an offline camera is not trouble, it is a fact stated quietly.
- **The parts nobody draws**: selection, caret, focus ring and scrollbar thumb are all themed from the world's own cyan and rules, so no browser default belongs to no design system.

### Named Rules
**The Field Is The Fact Rule.** A colour may only be used for the machine state its token is named after. `--field-red` is a camera down; it is never a marketing accent, never a hover tint, never a brand flourish.

**The Two Trouble Fields Rule.** Amber and red are the only trouble hues in the system. Green, cyan, white and grey are state. A third alarm colour is not permitted, so severity survives as a *number* riding the field rather than as a rainbow of fields.

**The Immovable Hues Rule.** The four field hues and their inks are byte-identical in `:root` and `.dark`. Only the ground, the ink tiers, the rules, and the text-role variants of accent/status/severity move between themes.

**The Tint From The Field Rule.** Secondary text on a coloured field uses that field's own ink token. Gray-on-colour is the failure mode this rule exists to prevent.

## Typography

**Display Font:** Archivo, weights 300 (display numerals, wordmark, page titles) and 600 (labels, data, headings), self-hosted from `web/public/fonts/` with `ui-sans-serif, system-ui, sans-serif` fallback.
**Body Font:** Archivo 400, same stack.
**Label/Mono Font:** JetBrains Mono (variable 100–800, one file), with `ui-monospace, SFMono-Regular, monospace` fallback.

**Character:** Archivo is a wide, plain grotesque with no ceremony — at 300 it reads as signage, at 600 it reads as a timetable. Paired with JetBrains Mono it produces exactly the register a record needs: human words for what happened, machine glyphs for what is measured. Nothing is italic, nothing is bolded for emphasis, and nothing is set in uppercase.

### Hierarchy
- **Wordmark** (300, 2.5rem, tight tracking): the `nvrWay` band in the header. The only place the family is set tight.
- **Display** (300, 1.875rem, cap tracking): every page title — Live, Events, Cameras, Settings, Audit, Sign in. Light weight at large size is what keeps a dense console from feeling urgent.
- **Numeral** (300, 3rem, line-height 1): the severity number on a flipped tile's back face. The one moment the console shouts, and it shouts a number.
- **Title** (600, 1rem–1.25rem, cap tracking): a tile's camera name, a section heading inside a panel.
- **Body** (400, 0.875rem): every sentence the console writes — states, failures, explanations. Sentences are the console's primary output; they are not an afterthought.
- **Label** (600, 0.75rem, cap tracking 0.01em): nav tiles, buttons, filter affordances, list-row primary words.
- **Data** (JetBrains Mono 400, 0.75rem–0.875rem): timestamps, slugs, durations, counts, severities, ids, form values. Anything the operator compares across rows.
- **Micro-label** (JetBrains Mono 400, 0.625rem): form field captions and the zone editor's vertex badges.

### Named Rules
**The Numerals Are Mono Rule.** Anything two rows of data must be compared on is JetBrains Mono: times, counts, slugs, durations, severity. Prose is Archivo. A numeral in Archivo is a bug.

**The Weight, Not The Case Rule.** A label is set at weight 600 with 0.01em tracking in its natural case. The console ships zero `uppercase` transforms; Metro states a label with weight, not with a stamp.

**The One Family Rule.** Archivo at 300/400/600 plus JetBrains Mono is the whole voice. No third family, no display face, no icon font.

## Layout

The model is a wall of fields, not a document. Every page is `p-6` (24px) inside a header band (`px-6 py-4`, hairline-bottomed) that carries the wordmark, the day/night toggle, sign out, and the nav.

- **The live mosaic** is the first thing on `/live` and leads the first viewport: `grid-cols-2` from the phone up, three wide at `xl` (1280px), four wide at `2xl` (1536px), with a **4px gap** (`gap-1`) — the ground doing the separating. Eight feeds read as two rows on a desk, which is the shape PRODUCT.md promises at 8 cameras. The direction brief asked for six tiles to lead on a phone; the build ships the whole wall two-wide and lets the caption row and the day's strip fall below the fold instead, which is where the brief's real promise (the record never crowding the feeds) still holds.
- **The caption row** under the wall is a single mono line — today's count, the camera count — set in `text-sm` mono with the numbers lifted to primary ink.
- **The day's strip** is hairline-topped (`border-t` rule-strong) and ruled per row (`border-b` rule), a fixed four-column grid `90px / 140px / 1fr / 40px`: time, camera slug, rule, severity. Every row is a link.
- **The event wall** is the one place with real air: `grid-cols-1 gap-4`, two up at `md`, three at `xl`. Cards need the 16px because they carry pictures.
- **Forms** are `grid-cols-1 gap-3`, three up at `md`; the zone editor's rule fields are `grid-cols-2 gap-2`, four up at `md`.
- **Lists** (audit ledger, storage backends, camera rows, OIDC providers) are a single framed column: `border border-rule` around the outside, `border-b border-rule px-3 py-2 last:border-b-0` per row, baseline-aligned with `gap-x-4`.
- **The drawer** is not a centred modal: `fixed inset-0 z-50` with a 60%-black scrim, and a right-side panel `w-full max-w-2xl` with a hairline left edge and `p-5`. It is a URL (`/events/:id`), so it survives reload and paste.
- **Responsive behaviour** is one breakpoint set (Tailwind `md`/`xl`/`2xl`). On a phone the actions ride the wordmark row and the nav takes a full second row; at `md` the header collapses to one line.

Spacing rhythm is Tailwind's own scale, used narrowly: **4px** between fields, **8px** inside a control, **12px** inside a cell, **16px** between stacked blocks (`mt-4` is the dominant vertical rhythm), **24px** of page padding.

## Elevation & Depth

The system has no shadows. There is not one `box-shadow` in the shipped stylesheet or in any component, and none is needed: depth here is spatial, not optical.

### Shadow Vocabulary
None. Depth is conveyed by four other devices:
- **The gap**: tiles are separated by the ground showing through 4px of space. The ground is the deepest plane in the system.
- **Tonal layering**: three steps — ground (`night-ground` → `night-surface` → `night-tile`; `day-ground` → `day-surface` → `day-tile`). A surface one step up is a surface one step nearer.
- **Hairlines**: 1px rules in `--rule`, and `--rule-strong` where a group needs a top edge. Rules meet at right angles and never balloon.
- **The scrim**: the drawer's 60%-black overlay plus `z-50` is the only literal overlay in the console.

The one place depth is *simulated* is the signature flip — a 1400px perspective on the tile's scene — and it is used for a state change, never for atmosphere.

### Named Rules
**The Zero Shadow Rule.** Surfaces are flat at rest and flat in every state. Hover changes a *fill*, never an elevation.

**The Gap Is The Border Rule.** Adjacent fields are separated by ground, not by a stroke. Hairlines appear only to rule rows inside a framed list, and a framed list uses one outer border rather than a border per row.

## Shapes

The form language is the rectangle, exclusively. `borderRadius` is flattened at the Tailwind level — `none`, `sm`, `DEFAULT`, `md`, `lg`, `xl`, `2xl`, `3xl` **and `full`** are all `0` — so even a `rounded-full` class draws a rectangle. The base layer repeats the guarantee for `input`, `select`, and `textarea`, because browser form defaults are the one place a radius creeps back in.

Corners are therefore square everywhere, and rules meet at right angles rather than following a curve. Borders are 1px hairlines for structure, 2px for emphasis: the severity ring is `border-2`, and the SVG strokes over a detection frame are `strokeWidth: 2`. Focus is a 2px solid outline in `--focus` with a 1px offset, applied globally through `*:focus-visible` so no component has to remember it.

Media bleeds to the tile's edge — a Metro field has no mat around it — and the standard media shape is `aspect-video`. The zone editor's canvas is a black rectangle with the operator's polygon painted over it at 20% accent fill and a full-strength accent stroke.

## Components

### Buttons
- **Shape:** square, no radius (0), hairline or accent-weight border, no shadow in any state.
- **Primary action (accent outline):** transparent fill, 1px accent border, accent text, label typography, `px-3 py-1` (4px 12px). This is the console's default affirmative control — save, search, add, retry.
- **Filled accent:** accent fill with the tile colour as ink (`px-3 py-1`). Reserved for the sign-in submit, the one action with no ambiguity about what it does.
- **Hover:** the fill changes and nothing else — `hover:bg-bg-secondary` with the ink stepping up to primary. Accent-outline buttons hover by border, not by fill.
- **Disabled:** the accent border and text fall back to rule and muted ink (`disabled:border-rule disabled:text-text-muted`); destructive buttons drop to 50% opacity. A disabled control still looks like the control it was.
- **Quiet / link actions:** mono text with an underline (`clear all`, `edit`, `delete`, pagination). Destructive quiet actions take the error hue on the text only.
- **Destructive (the console's only one):** `delete <slug>` sits in a confirmation section that first states the size of the act in numbers — segments, events, local files, remote purge jobs — then offers `keep it` (hairline, secondary ink) beside `delete <slug>` (error field, ground-coloured ink). The destructive field is the only place red is used for something the machine is not reporting.

### Chips / Filter Tokens
- **Style:** transparent fill, 1px accent border, accent mono text, `px-1.5`, square.
- **State:** an active filter is a chip with its `key: value` written out and a `×` clear button inside it; a removable class/zone chip on a card is the same shape in secondary ink and hovers to an accent border. Tapping a card's chip *adds* a filter — the chip is a door, not a label.
- **Severity filter:** three square mono buttons `1 2 3`; the selected one takes the accent border and accent text.

### Cards / Containers (the event card)
- **Corner Style:** square (0).
- **Background:** the tile colour, hairline-bordered (`border border-rule`).
- **Shadow Strategy:** none — see Elevation & Depth.
- **Border:** 1px `--rule` on all four sides; the thumbnail bleeds to the inner edge and is separated from the meta row by a hairline.
- **Internal Padding:** `px-3 py-2` on the meta row, `px-3 pb-3` on the token row.
- **Character:** the picture is first, always. When there is no picture the same box carries a sentence naming *which kind of absence this is* (`no snapshot yet`, `reading the frame`, or the server's own reason) in the surface colour — a wall of events with no pictures is still a wall an operator can read.

### Inputs / Fields
- **Style:** 1px `--rule` border, surface fill, mono text at `text-sm`, `px-2 py-1`, square. Labels sit above in mono micro-label (0.625rem) muted ink.
- **Focus:** the global 2px accent outline with 1px offset; the caret is accent everywhere.
- **Disabled:** muted ink; the border stays.
- **Selects and checkboxes** take the same treatment — no native radius survives, and a checkbox is labelled inline in mono rather than decorated.

### Navigation
- **Style:** nav tabs are tiles — `px-3 py-2`, label typography, secondary ink, no border, no underline.
- **Active:** the tile fills with the lit cyan field and takes cyan's own ink. It is the brightest thing in the header, which is the point: a concourse lights the destination you are on.
- **Hover:** surface fill, ink steps to primary.
- **Role gating:** a `viewer` never sees Settings or Audit. The server's role check is the gate; hiding the tab is courtesy, and a deep link meets the same refusal.
- **Settings sub-nav:** the same tile treatment on a hairline-ruled baseline, driven by the `?tab=` query so a section is a URL.
- **Mobile:** the nav drops to a full-width second row; five tiles still fit one line at phone measure.

### The Live Tile (signature)
A flat field with a header band that states the camera's condition in the field's own colour — cyan `live`, amber `opening`, red `down` — decided by two truths at once: this tile's own media state and the server's `camera.status` broadcast, with the server outranking the attempt. The camera's name is struck through *exactly* when the band says `down`: one truth, stated twice, never two conditions that can disagree. Below the band the picture bleeds edge to edge; below that, mono caption rows carry the slug and the recorder's line (`last segment 07:12:04 · continuous · 10s segments`), the fresh one wearing the green field.

### The Tile Flip (signature move)
When a new event lands for a camera, its tile turns once on its Y axis — 600ms, `cubic-bezier(0.16, 0.84, 0.24, 1)`, 1400px perspective — shows the event, and turns back after 2.6s. A tile that already had an event when it mounted does not flip: the operator arriving is not being alerted. The back face is a door to the event drawer and carries the camera's name, the time, the severity as a 3rem numeral, the rule that fired, and `tap to review` — so a flipped door is still the door to that camera. Its field is the world's two trouble fields and no third hue: amber below severity 3, red from 3 up, with the severity number riding the field so the scale survives the two-colour rule. `prefers-reduced-motion: reduce` removes the transition entirely; the face still changes, it simply does not travel.

### The Feed Banner (state as a field strip)
A degraded feed is a colour band across the page, not a line of console log: amber with amber ink while retrying (`role="status"`), red with white ink when the socket has refused (`role="alert"`). It says nothing when the feed is current, because a banner that is always on is a banner nobody reads.

### The Severity Ring
A 24px square, `border-2`, mono numeral inside. Levels 1 and 2 take their hue as border and text (`--severity-1` is the muted ink, `--severity-2` is the amber hue). Level 3 — top of the shipped scale — is the red field itself, filled, because a text-tinted outline would make the list disagree with the wall about what "call somebody" looks like.

### The Zone Editor
A black canvas with the operator's snapshot behind it, polygons stroked in accent and filled at 20% so the picture stays the picture, and each zone's first vertex carrying a DOM badge (`name · rule`) rather than a pixel label — it has to be readable at any density and reachable by keyboard. A recorded detection box is drawn only when the event names the frame its pixels belong to; otherwise the console states the count and says it will not guess where the boxes land.

### Sentences, Not Spinners
Every asynchronous state in the console is a written sentence in the box it belongs to: `Reading the camera list.`, `reading the frame`, `clip not available`, `no segment yet — the recorder has not closed one for this camera`, `the record could not be read — is the server running?`. The server's own words ride as a tooltip or a mono sub-line, present for the operator who wants the diagnosis and out of the way of the one who doesn't. There is no spinner, no skeleton, and no broken-image icon anywhere in the shipped build.

## Do's and Don'ts

### Do:
- **Do** write semantic token classes in components (`bg-bg-card`, `text-status-offline`, `border-severity-2`) and keep every hex in `web/src/index.css`. The shipped build has zero hex literals outside the token layer; keep it that way.
- **Do** state a machine's condition with a field colour *and* a word in that field's own ink — `background: var(--field-amber)` with `color: var(--field-amber-ink)`, never a grey.
- **Do** keep the four field hues and their inks identical in both themes; move only the ground, the ink tiers, the rules, and the text-role variants.
- **Do** separate adjacent fields with 4px of ground and rule rows inside a framed list with 1px hairlines (`px-3 py-2`, `last:border-b-0`).
- **Do** set every comparable numeral in JetBrains Mono at `text-xs`/`text-sm`, and every label at weight 600 with 0.01em tracking in natural case.
- **Do** give every loading, empty, and failed state a sentence that names which kind of state it is, in the same box the content would occupy.
- **Do** reserve the tile flip for a state change, keep it at 600ms with the exponential ease-out, and keep the `prefers-reduced-motion` escape hatch.
- **Do** keep the flip's back face carrying the camera's name, the severity number, and the rule — a door must still say which door it is.
- **Do** show a destructive control only to a role the server will accept, and make it state the size of the act in numbers before it acts.
- **Do** let media bleed to the tile's edge; a Metro field has no mat.

### Don't:
- **Don't** introduce a third trouble hue. Amber and red are the whole alarm vocabulary; severity survives as a number riding the field.
- **Don't** round anything. `rounded-full` is `0` in this build on purpose — do not "fix" it.
- **Don't** add a shadow, a gradient, a blur, or a backdrop filter. Depth is the gap and the tonal step.
- **Don't** put grey text on a coloured field.
- **Don't** use a spinner, a skeleton, or a bare icon to stand in for a sentence. If a glyph would have to be decoded from a legend, write the word instead — the shipped console states every state in words and colour and ships no icon set.
- **Don't** set a numeral in Archivo or a sentence in JetBrains Mono.
- **Don't** uppercase a label for emphasis; use weight 600.
- **Don't** let a component compute a truth the server already owns. Times shown are server-assigned times (the server clock owns event time), a camera's condition is `camera.status` plus this tile's own media state, and a box is drawn only on the frame the event names.
- **Don't** let the day theme be a afterthought or the night theme be the only one: both are first-class, chosen before first paint, and toggled without a reload.
