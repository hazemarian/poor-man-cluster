---
version: alpha
name: pmcluster console
description: A calm control plane — flat zinc surfaces, hairline separation, one indigo accent, IBM Plex.
colors:
  # one accent; -strong is the same hue darkened until white text clears 4.5:1
  primary: "#6D7CFF"
  primary-hover: "#5A67E8"
  primary-strong: "#5460DC"
  primary-strong-hover: "#4B57D6"
  primary-surface: "rgba(109, 124, 255, 0.12)"
  primary-border: "rgba(109, 124, 255, 0.22)"
  # chrome
  surface-body: "#0A0B0E"
  surface-sidebar: "#0D0F15"
  surface-card: "#101218"
  surface-elevated: "#161922"
  surface-hover: "#1A1E27"
  surface-input: "#1C2029"
  surface-lead: "#1C2130"
  # hairlines do the separating
  border-subtle: "#191C23"
  border-default: "#23272F"
  border-active: "#2C313B"
  border-control: "#454C5A"
  # text — tertiary never lands below 4.5:1 on any surface it can sit on
  text-primary: "#E9EBF0"
  text-secondary: "#A5ACBA"
  text-tertiary: "#8A93A3"
  # four fixed semantics, each with a tint and a border at the same alphas
  success: "#3DDC97"
  success-surface: "rgba(61, 220, 151, 0.12)"
  success-border: "rgba(61, 220, 151, 0.22)"
  warning: "#F5B945"
  warning-surface: "rgba(245, 185, 69, 0.12)"
  warning-border: "rgba(245, 185, 69, 0.22)"
  danger: "#FB6F6F"
  danger-surface: "rgba(251, 111, 111, 0.12)"
  danger-border: "rgba(251, 111, 111, 0.22)"
  info: "#49C7F0"
  info-surface: "rgba(73, 199, 240, 0.12)"
  info-border: "rgba(73, 199, 240, 0.22)"
  # light theme — only the tokens whose value changes; applied via [data-theme="light"]
  primary-light: "#4B57D6"
  primary-hover-light: "#4049B8"
  primary-strong-light: "#4049B8"
  surface-body-light: "#FBFBFC"
  surface-sidebar-light: "#F5F6F8"
  surface-card-light: "#FFFFFF"
  surface-hover-light: "#F1F2F5"
  surface-input-light: "#F4F5F7"
  surface-lead-light: "#EBEEF4"
  border-subtle-light: "#EDEEF1"
  border-default-light: "#E4E6EA"
  border-active-light: "#D3D7DE"
  border-control-light: "#B9C0CB"
  text-primary-light: "#14161A"
  text-secondary-light: "#5B616E"
  text-tertiary-light: "#676E7C"
  success-light: "#126C48"
  warning-light: "#8A5A08"
  danger-light: "#B32B2B"
  info-light: "#16779B"
typography:
  h1:
    fontFamily: IBM Plex Sans
    fontSize: 30px
    fontWeight: 600
    lineHeight: 1.3
    letterSpacing: "-0.035em"
  h2:
    fontFamily: IBM Plex Sans
    fontSize: 16px
    fontWeight: 600
    lineHeight: 1.3
  h3:
    fontFamily: IBM Plex Sans
    fontSize: 14px
    fontWeight: 600
    lineHeight: 1.3
  metric:
    fontFamily: IBM Plex Sans
    fontSize: 32px
    fontWeight: 600
    lineHeight: 1.3
    letterSpacing: "-0.01em"
    fontFeature: "tnum"
  body-md:
    fontFamily: IBM Plex Sans
    fontSize: 14px
    fontWeight: 400
    lineHeight: 1.5
  body-sm:
    fontFamily: IBM Plex Sans
    fontSize: 13px
    fontWeight: 400
    lineHeight: 1.5
  label-xs:
    fontFamily: IBM Plex Sans
    fontSize: 12px
    fontWeight: 500
    lineHeight: 1.4
    letterSpacing: "0.06em"
  mono-code:
    fontFamily: IBM Plex Mono
    fontSize: 12.5px
    fontWeight: 400
    lineHeight: 1.6
    fontFeature: "tnum"
rounded:
  sm: 5px
  md: 8px
  lg: 12px
  xl: 16px
spacing:
  s-1: 4px
  s-2: 8px
  s-3: 12px
  s-4: 16px
  s-5: 20px
  s-6: 24px
  s-8: 32px
  s-10: 40px
  s-12: 48px
components:
  button-primary:
    backgroundColor: "{colors.primary-strong}"
    textColor: "#FFFFFF"
    typography: "{typography.body-sm}"
    rounded: "{rounded.md}"
    height: 38px
    padding: 12px
  button-primary-hover:
    backgroundColor: "{colors.primary-strong-hover}"
    textColor: "#FFFFFF"
    rounded: "{rounded.md}"
  button-secondary:
    backgroundColor: "{colors.surface-input}"
    textColor: "{colors.text-primary}"
    typography: "{typography.body-sm}"
    rounded: "{rounded.md}"
    height: 38px
  panel:
    backgroundColor: "{colors.surface-card}"
    textColor: "{colors.text-primary}"
    rounded: "{rounded.lg}"
    padding: "{spacing.s-4}"
  panel-header:
    backgroundColor: "{colors.surface-card}"
    textColor: "{colors.text-primary}"
    typography: "{typography.h2}"
    padding: "{spacing.s-3}"
  nav-item-active:
    backgroundColor: "{colors.primary-surface}"
    textColor: "{colors.text-primary}"
    typography: "{typography.body-sm}"
    rounded: "{rounded.md}"
    height: 38px
  input:
    backgroundColor: "{colors.surface-input}"
    textColor: "{colors.text-primary}"
    typography: "{typography.body-md}"
    rounded: "{rounded.md}"
    height: 38px
    padding: "{spacing.s-2}"
  banner-error:
    backgroundColor: "{colors.danger-surface}"
    textColor: "{colors.text-primary}"
    typography: "{typography.body-sm}"
    rounded: "{rounded.md}"
    padding: "{spacing.s-3}"
  stat-value:
    backgroundColor: "{colors.surface-lead}"
    textColor: "{colors.text-primary}"
    typography: "{typography.metric}"
    padding: "{spacing.s-4}"
  pill-success:
    backgroundColor: "{colors.success-surface}"
    textColor: "{colors.success}"
    typography: "{typography.label-xs}"
  mono-identifier:
    backgroundColor: "{colors.surface-input}"
    textColor: "{colors.text-primary}"
    typography: "{typography.mono-code}"
---

## Overview

**A calm control plane.** A dark-first operator console that answers three questions in
order — *is the cluster healthy, what is deployed, what is about to change* — and never
renders a state it does not actually know.

Everything else follows from those two clauses. The answer order decides page order and
hierarchy; the second clause is why "unknown" is a first-class state rather than a zero.

### Direction

**v1 (rejected):** near-black navy `#0B1220`, blue `#4C8DFF`, Inter. It fixed the
architectural failures but read as a generic AI-SaaS dashboard — the exact look both
reference products reject.

**v2 (adopted, shipped):** a cool-neutral zinc ramp, hairline separation, one indigo
accent, IBM Plex. Flat: no gradients anywhere, no shadows except true overlays.

**ServerKit** (`jhd3197/ServerKit`) contributes the neutral ramp and semantic palette,
IBM Plex Sans + Mono, the grouped sidebar, one accent that is var-backed and
runtime-themable, status as **dot plus label** ("never color alone"), and the six-phase
deploy console with a live filterable log. It is also the standing **anti-reference**: no
gradient text, no glassmorphism, no hero-metric card grids, no uppercase eyebrow on every
section, no toy roundness, no cPanel clutter, no joyless enterprise tables.

**CranL** (`cranl.com`) contributes restraint and type, not layout: flat surfaces separated
by 1px hairlines instead of shadows, **one** accent used only for the primary action, 16px
monochrome line icons, generous section padding, no gradients.

### The four global states

These are architectural; they are what most of the visual work is actually for.

- **Error.** One line of plain language, a severity icon, the *reason* and a *remedy*, a
  retry, and the raw payload behind a `Show details` disclosure. A JSON envelope or daemon
  sentence must never appear in the reading path. Machine strings stay available and
  stay copyable — demoted, not deleted.
- **Unknown ≠ zero.** A metric is one of *real value* / `—` with a reason
  ("unavailable: Docker daemon unreachable") / skeleton. A failed fetch suppresses any
  "0 results" claim on the same page: `0 services` beside a 500 is a lie the UI told.
- **Empty.** Icon, one sentence, and the actual primary action as a button — never inert
  prose pointing at a tab that does not exist.
- **Dirty / saving.** A form shows that it changed, the save action is disabled until it
  did, validation is inline and per-field. Destructive or global actions (apply to swarm,
  rollback, delete, rotate) require a confirm that enumerates what will change.

## Colors

One accent, four semantics, a zinc ramp, and hairlines doing all the separating.

- **Primary (#6D7CFF):** indigo. Text, icons, hairlines, focus ring, active nav row.
  Nowhere else, ever.
- **Primary-strong (#5460DC):** filled accent surfaces. `#6D7CFF` under white text is
  3.51:1, so a filled button uses the darkened variant (5.1:1) while `#6D7CFF` keeps the
  roles where it already passed.
- **surface-lead (#1C2130):** the lead stat cell, which carries the headline state. It
  exists as a dedicated token because `surface-elevated` is `#FFFFFF` in light — identical
  to `surface-card` — so the cell lost its lift when reusing it.
- **Text-tertiary (#8A93A3):** was 3.05–3.5:1 on every panel it sat on; now ≥4.58:1 on
  every surface it can land on.
- **Semantics:** success / warning / danger / info, each with a `-surface` tint and a
  `-border` at the same alphas. Status is always *dot plus label*.
- **Light theme:** dark is the default (`color-scheme: dark`); `[data-theme="light"]`
  overrides the `*-light` values, including darkened semantic text so it clears AA on its
  own tint and on hovered table rows.

**Measured, not eyeballed.** Resting row actions lost their `opacity: .35` — axe measured
it at 1.8:1, and it hid the affordance from anyone who does not hover. After the pass,
dark-theme colour-contrast violations across all 15 routes went **221 → 0** and the
accessibility rail (axe) sits at **0 violations**.

**Lint note.** `design.md lint` reports three `contrast-ratio` warnings
(`nav-item-active`, `banner-error`, `pill-success`) because it evaluates text against the
`-surface` token as if it were opaque; those fills are 10–12% alpha tints composited over
the card. Composited properly, the ratios are 13.63:1 / 13.48:1 / 8.53:1 in dark and
15.72:1 / 15.45:1 / 5.54:1 in light — all above AA. The warning is an artifact of the spec's
model, not a finding; axe on the live pages agrees.

## Typography

IBM Plex Sans, IBM Plex Sans Arabic, IBM Plex Mono — self-hosted, 19 `@font-face`
declarations, subset per script with `unicode-range`, `font-display: swap`.

Six UI steps, one page-title step, one metric step, and nothing below 12px.
**Mono is sized on its own step:** IBM Plex Mono runs ~4% wide, so 12.5px mono optically
matches 13px sans.

`h2` (panel title, 16px) and `h3` (in-card sub-heading, 14px) deliberately differ — in the
first v2 cut both were 14px and a panel title was indistinguishable from its own
sub-heading. The page title is 30px, 26px at phone width.

**Tabular figures everywhere a number is a column of data** (`.num`, `.stat-v`, `td.num`,
`.mono`) — a metrics table that jitters by digit width is unreadable at 13px.

**Tracking is a Latin-only adjustment.** Negative letter-spacing is neutralised in RTL
(`[dir="rtl"] h1 { letter-spacing: 0 }`, likewise the auth headings) because Arabic is
cursive and tracking breaks its joins.

**Uppercase micro-labels are allowed for field labels, table headers and kpi keys** —
banned as decorative section eyebrows. That is the difference between this and the
anti-reference.

## Layout

A 4px grid, named: `s-1…s-12` (4 8 12 16 20 24 32 40 48). Paddings, gaps and margins pick
from the scale, never a literal. `--gap-icon: 6px` exists for the one case that sits
between steps: an icon next to its own label.

```
nav-w        240px    sticky sidebar
rail-w        64px    collapsed rail (token defined; collapse not built)
top-w         72px    topbar / sidebar head
content-max 1440px    content column
control-h     38px    button, input, icon button, segmented control — one toolbar baseline
control-h-sm  32px    small / ghost
touch         44px    every interactive control under (hover: none) and (pointer: coarse)
```

Panel padding is 16px on every side, its header 12px, its footer 12px; sections separate by
24px; a page is 24/32/48px of content padding. Table rows carry 12px vertical padding, so a
row is 44px and doubles as a touch target.

Breakpoints in use: **1199 / 1024 / 768 / 767 / 640**. At ≤767px the sidebar becomes an
off-canvas drawer and a fixed bottom tab bar takes the five primary destinations.

## Elevation & Depth

Shadows are reserved for true overlays and appear exactly once:

```
--shadow-overlay: 0 16px 40px rgba(0,0,0,.55)      dark
--shadow-overlay: 0 16px 40px rgba(20,22,26,.14)   light
```

Everything else separates with 1px hairlines and gaps: panels by `border-default`, internal
table rules by `border-subtle`, hover and selected states by `border-active`. **No
elevation on hover, no cards floating over cards** — depth is information (this is above
that), never decoration.

## Shapes

```
r-sm 5px    inputs, pills' inner, focus ring
r-md 8px    buttons, banners, nav rows
r-lg 12px   panels
r-xl 16px   drawers, modals
pills       fully round (badges, status dots' labels)
```

No toy roundness: radii stay at or below 16px, and a control's radius is never larger than
its own height allows without turning into a lozenge.

## Components

- **`panel`** — the only container. Header (16px title, hairline bottom), body, optional
  footer. Never nested inside another panel; a panel inside a panel is a table.
- **`button-primary`** — one per screen, and its label names the consequence
  (`Deploy v20260919-d549528 to test-lms`), never `Submit`. Filled with
  `{colors.primary-strong}`, height `--control-h`.
- **`button-secondary` / `btn-ghost`** — everything else. Secondary carries the
  `border-control` hairline because a control sits on `surface-input` inside a card, where
  `border-default` vanishes against its own fill.
- **`nav-item-active`** — `{colors.primary-surface}` fill, no left bar, no shadow. The
  active row is the only nav decoration.
- **`input`** — `surface-input` fill, `border-control` hairline, 38px, focus ring in
  `{colors.primary}` with 2px offset.
- **`banner-error`** — reason + remedy, one action, `Show details` disclosure holding the
  raw payload. `{colors.danger-surface}` fill, `danger-border`, never a full-saturation red
  block.
- **`stat-value`** — the single large number, `{typography.metric}`, tabular figures, with
  its unit and its timestamp in `body-sm` beneath it. The lead cell takes `surface-lead`.
- **`pill-success`** (and the warning/danger/info siblings) — dot plus label, tinted fill,
  semantic text colour. **Never colour alone**: the label carries the state.
- **`mono-identifier`** — revisions, digests, hostnames, IPs, paths and raw payloads:
  `{typography.mono-code}`, `direction: ltr`, `unicode-bidi: isolate`.
- **`kv` rows** — the mobile form of every table row. A table is never horizontally
  scrolled on a phone; it becomes label/value pairs.
- **Empty state** — icon, one sentence, and the real primary action.
- **Not built:** toasts, segmented controls, a deploy stepper, `.progress`/`.kpis`/
  `.capacity` blocks. Their selectors exist in CSS without a producer — see Open items.

## Do's and Don'ts

**Do**

1. **Hairlines, not cards.** Panels separated by 1px rules and 24–32px gaps; stat groups
   share dividers instead of each becoming a card.
2. **One accent, one primary action per screen.** Indigo on the primary button, the active
   nav row and the focus ring — nowhere else. Semantics carry the rest.
3. **Every number is a state, not a value.** Unknown and stale render as text with a
   reason, never as `0`. Every metric carries its unit and its timestamp.
4. **Density with hierarchy.** 13px table rows at 44px height; mono for identifiers, sans
   for prose; weight and colour doing the scale work instead of boxes and gaps.
5. **Name the consequence.** `Deploy v20260919-d549528 to test-lms`, not `Submit`.
   Confirmations restate resource, revision and blast radius.
6. **Logical properties only.** `inline-start/end`, `margin-inline`, `padding-inline` — the
   same sheet must render LTR and RTL without a second stylesheet.
7. **Isolate LTR islands.** Revisions, digests, hostnames, IPs, paths and payloads go in
   `bdi` or `.iso`. A value mixing digits with Latin letters (`7.7 GiB`) reorders inside
   RTL text without isolation.

**Don't**

1. No gradient text, no glassmorphism, no hero-metric card grids.
2. No uppercase eyebrow above every section.
3. No shadows for elevation on hover or focus — hairlines and tint do that work.
4. No raw daemon or JSON payload in the reading path. Demote it behind `Show details`;
   never delete it.
5. No `0` for a failed fetch, and never a "0 results" claim on a page whose fetch failed.
6. No prose in Go. Every user-visible string is a translation key; the dictionaries own the
   words (see `docs/console-i18n-contract.md`).
7. No literal px outside the token set, and no inline `style=` for sizing that a token
   already covers (`--touch` in particular).
8. No page-scoped override until the shared rule has been checked: a per-page modifier is
   right when its consumers genuinely disagree (`.form-grid-2`), and a band-aid when they
   do not.

## Shell and responsive

`app` is a two-column grid: a 240px sticky sidebar (full `100dvh`, hairline inline-end
border, `overscroll-behavior-y: none` on `html.console` so document bounce cannot drag it)
and the content column. A 72px topbar holds the mobile menu button, breadcrumbs and the
language and theme controls. Content is capped at 1440px.

At ≤767px the sidebar becomes an off-canvas drawer (`.is-open` + `.scrim`, toggled by
`[data-drawer-open]` / `[data-drawer-close]`) and a fixed bottom `.tabbar` carries the five
primary destinations with 44px targets. Icon buttons and the drawer-close control are 44×44
`inline-grid` so the icon centres to ±0.00px — a 38px block-level button put it 3px high,
which vision review called "fine" and the DOM disproved.

The one deliberate exception: **stacks open in an inspector at the bottom of their own
page**, not in a drawer, because the operator needs the list and the open stack at the same
time. The shell's drawer belongs to the sidebar.

`login.html` and `setup.html` are full standalone documents with their own head and auth
footer. That is intentional, not duplication by accident.

## Arabic / RTL

Direction is a property of the document (`dir="rtl" lang="ar"`), and the CSS is written so a
page cannot get it wrong by omission:

- **All box-direction properties are logical.**
- **Directional icons mirror through one utility** — `[dir="rtl"] .flip { transform:
  scaleX(-1) }` — not through per-rule overrides.
- **LTR islands stay LTR and isolated** (`bdi`, `.iso`), with a stylesheet-level safety net
  on `[dir="rtl"] pre, code, samp, kbd, .mono`, which is why 23 raw-payload `<pre>` blocks
  need no per-site attribute.
- **Values mixing digits with Latin letters are bidi-isolated** (FSI/PDI).
- **Arabic type**: IBM Plex Sans Arabic in three weights, subset by script, sizes unchanged,
  tracking zeroed.
- **Prose lives in the dictionaries.** Six Arabic plural categories are handled by
  `registerPlural`, with the dual nominative/oblique distinction.
- **Enforcement** is the contract document plus three central tests (EN/AR key parity,
  used-but-undefined keys, plural registration) — not per-call-site review.

## Designed, not built

Preserved intent — these are the reasons the current pages still feel incomplete, each with
its code-side prerequisite:

- **Deploy review drawer** — resolved revision, image-digest diff against what is live,
  dry-run toggle, progress log with a deploy ID, one-click rollback. Needs a small API
  addition: resolve the requested revision server-side and return the resulting digest and
  endpoint set, so the diff is real rather than decorative.
- **TLS chain status as its own field**, distinct from expiry, plus SAN coverage and a live
  day countdown. Needs chain parsing persisted next to expiry — this is the screen that
  would have caught the live leaf-only incident without `openssl s_client`.
- **Webhook delivery history** — per-delivery status and response code, last-delivery
  timestamps, rotate/revoke per row, secret shown once at creation.
- **Backups** — schedule with next run, last successful run with a staleness warning, size
  and retention, restore/download/delete, and a loud *failed* / *missed* state.
- **Users / API keys** — labelled actions column, `Edit` split into edit / reset password /
  disable, self-demotion guard, status and last-login columns.
- **Login** — environment badge so nobody signs into the wrong cluster, password reveal, no
  build token in unauthenticated output.
- **Search / ⌘K palette** over every destination.
- **Retired:** a full-height icon rail. The 240px sidebar with a grouped nav is what
  shipped; `--rail-w` remains as a token for a future collapse.

## Mapping to the code

```
internal/ui/views/templates/*.html       25 templates; fragments define blocks, app/login/setup are shells
internal/ui/views/static/fonts.css       19 @font-face, 3 families, script subsets, swap
internal/ui/views/static/base.css        tokens, reset, typography, shell
internal/ui/views/static/components.css  buttons, panels, forms, kv, banner, pills
internal/ui/views/static/patterns.css    page patterns, stats, tables, tabbar, responsive blocks
internal/ui/views/static/preferences.js theme + language controls
internal/ui/views/render.go              ShellBase, Localize, func map, static handler
internal/ui/views/i18n/*.go              dictionaries, formatter, plural rules
internal/ui/controllers/*.go             page data; emits translation keys, never prose
internal/cluster/embeds/edge-stack.yml   console/API routing on the edge
docs/console-i18n-contract.md            the machine-enforced translation + markup contract
```

## Screenshots

The capture set is kept outside the repository — process artifacts, not shipped assets
(`docs/redesign/`, 36 files, ~26 MB, gitignored). It is the visual record this document is
written from:

- `current-*.png` (11) — the pre-redesign build, page by page: the evidence for the four
  global states.
- `concept-*.png` (9, desktop + mobile) — v1-direction concepts, superseded by v2.
- `v2/*.png` (10) — the shipped direction, dark and light, desktop and mobile.
- `references/*.png` (6) — the ServerKit and CranL captures the direction is drawn from.

## Open items

- **RTL tracking.** Negative tracking is neutralised rule by rule; `.auth-word` (-.02em)
  and `.delivery-link h3` (-.015em) still carry Latin tracking in Arabic. Fix: tokenize the
  two tracking values and zero them in one `[dir="rtl"]` block.
- **Dead CSS classes.** ~49 selectors ship with no producer in templates, JS or Go
  (`.timeline`/`.tl-*`, `.change-list`, `.run-bar`, `.progress`, `.kpis`, `.capacity`,
  `.badge-ext`, `.drawer-*` internals, `.scrim-overlay`, `.ic-sm`, `.nowrap`, `.tert`,
  `.only-wide`, `.topbar-context`). Delete them, or move the intended ones into *Designed,
  not built* as samples — a reader currently cannot tell designed-and-used from
  designed-and-dropped.
- **Tokens declared but not applied:** `--control-h-lg`, `--rail-w` (no collapse built).
- **`--touch` is bypassed by 11 inline `style="min-height:44px"`** in templates; they need
  a `.tap` utility, because an inline style also outranks any media-query fix.
- **The icon vocabulary has no contract test** while the translation keys have three.
  `{{icon "x"}}` with no `<symbol id="i-x">` renders as an invisible gap.
- **Theme names are stringly typed** across Go, three templates and `preferences.js`.
- **`.tight` / responsive padding** are re-declared per breakpoint; they want a
  `--panel-pad` custom property so a modifier needs no matching override per block.
- **Contrast** is clean in dark across all 15 routes (221 → 0) and the axe rail is at 0;
  re-run both after any palette change, since the light theme's semantic variants are
  hand-derived.
