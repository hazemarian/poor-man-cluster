# Console v2 + Arabic localisation — implementation contract

Every console page is rendered from the same token system, the same component
classes and the same translation layer. This file is the contract for any page work so
the console does not drift back into per-page styling.

**Status: enforced** — the i18n layer, the EN/AR dictionaries and the
parity/coverage/keysource tests ship with the console. The keyboard
shortcuts and §9 process history are a record, not requirements.

## 1. Translations

Templates never contain user-visible English. They call the per-request funcs:

| call | use |
| --- | --- |
| `{{T "page.section_key"}}` | plain string |
| `{{T .SomeDynamicKey}}` | dynamic key (e.g. shell breadcrumb) |
| `{{P 3 "plural.stacks"}}` | count with the language's plural rules (§3) |
| `{{N 1234}}` | grouped number, tabular figures |
| `{{N0 1789929731}}` | ungrouped identifier (revision/backup IDs — never comma-separated) |
| `{{TF "deploy.run_id" .Run.ID}}` | string with `{0}`, `{1}` placeholders |
| `{{icon "logs"}}` | inline sprite icon from `frag_icons.html` |
| `{{DIR}}` `{{LANG}}` `{{RTL}}` `{{THEME}}` | document direction / language / theme |

Keys are `namespace.thing`, one namespace per page (`overview.*`, `stacks.*`,
`services.*`, `deploy.*`, `webhooks.*`, `tls.*`, `backups.*`, `users.*`, `apikeys.*`,
`settings.*`, `login.*`, `setup.*`) plus the shared namespaces already defined:
`nav.*`, `common.*`, `st.*`, `err.*`, `cluster.*`, `plural.*`.

Entries live in `internal/ui/views/i18n/` as `dict_en_<group>.go` + `dict_ar_<group>.go`
pairs. The **file groups** are: `workloads` (stacks/services/backups/usage),
`auth` (login/setup/users/apikeys), `refinement` (tls/webhooks), `shell`
(nav/common/errors), `runtime` (services/plural), `cluster` (cluster/settings),
`access` (access-related dialogs), `delivery` (webhook deliveries).

```go
package i18n

func init() {
	register(EN, map[string]string{
		"stacks.title": "Stacks",
	})
}
```

**Do not add Arabic entries** — the `ar` dictionary is written by hand, one
reviewer, so the voice stays consistent. Missing Arabic keys are caught by
`views/i18n_coverage_test.go` (every template key must exist in both dictionaries) and
`i18n/dict_parity_test.go` (the two dictionaries must not drift, and every `plural.*`
key must carry all six CLDR categories with a case-inflected dual).

## 2. Markup

Use only the classes defined in `internal/ui/views/static/{base,components,patterns}.css`.
Read those three files before writing markup; the vocabulary is small and deliberate:

- page: `page-head` > `ph-text` (`h1`, `.sub`) + `ph-actions`
- surfaces: `panel` > `panel-head` (`h2`, `.ph-actions`) + `panel-body` (`tight`) + `panel-foot`
- metrics: `stats` > `stat` > `stat-l` / `stat-v` / `stat-f`; add class `is-unknown` when the value is unknown
- tables: `table-wrap` > `table` (`th.num`/`td.num` for numbers, `td .row-actions`)
- state: `pill` (`good` / `warn` / `bad` / `info` / `accent` / `plain`) wrapping `dot` + label
- messaging: `banner` (`bad` / `warn` / `ok`) with `ic` + `b-title` + `b-body` + `b-actions`;
  `msg` (`ok` / `err` / `warn`); `empty` with `h3` + `p` + `empty-actions`
- forms: `form-grid` / `field` (`label`, `.hint`, `.err`) / `check` / `form-actions`, `input`, `select`
- overlays: `drawer` (`is-open`, `drawer-head`, `drawer-body`, `drawer-foot`), `modal`, `scrim-overlay`
- patterns: `phases`/`phase` (deploy stepper), `log`/`ln`, `diff`/`d`, `rev-list`/`rev`, `run-bar`,
  `progress`, `timeline`/`tl-item`, `tabs`/`tab`, `capacity`, `kpis`, `toolbar`, `search`, `kv`/`kv-row`
- utilities: `mono`/`num`/`iso`/`flip`/`trunc`/`nowrap`/`muted`/`tert`/`sr-only`/`ic`/`ic-sm`, `row`, `grid-2`, `stack`, `divider`

Disclosure for raw payloads is `details.details` (summary + `pre`); the raw text itself
is not a special class — `pre` is styled globally.

`internal/ui/views/templates/app.html` (the shell) and `frag_overview.html` are the
reference implementations — match their structure and comment style.

## 3. States are not optional

The v1 audit found the same defects on every page. Do not reintroduce them:

- **unknown is never 0.** If a data source failed, render `unavailable` (a muted
  `pill` or an `—` with `title="{{T "err.metric_hint"}}"`), never a zero or an empty table.
  If the controller cannot express that today, add the field to that page's own
  controller file and say so in your report.
- **errors are humanised.** `banner` with the sentence first, then
  `<details><summary>{{T "common.show_details"}}</summary><pre class="raw">…</pre></details>`
  for the raw payload. Never lead with `pmcluster API 502: {…}`.
- **empty states are actionable** — one sentence plus the button that fixes it.
- **every list row has its own actions** (row menu or inline buttons).
- **destructive actions confirm** (`hx-confirm` with a translated, specific sentence).
- Mobile: cards as `kv` rows instead of squeezed tables, `>=44px` targets, no fixed px.

## 4. RTL

Never use physical box properties in markup or inline styles — the stylesheet is
logical (`inline-start`/`inline-end`) and `[dir="rtl"]` flips it. Two exceptions to
handle explicitly:

- Wrap every LTR identifier that sits inside RTL text — revisions, image digests, IPs,
  domains, shell commands, log lines — in the existing `iso` class (`dir="ltr"`,
  isolated), so bidi reordering cannot corrupt it.
- Mirror directional icons only: `{{icon "chevron"}}`, `back`, `logout`, `rollback`
  get `class="ic flip"`. Never mirror the magnifier, sliders, checkmarks or clocks.

## 5. Numbers

All counts go through `{{P n "plural.…"}}` and all bare numbers through `{{N n}}`.
Western digits are used in both languages — this is an operator console where numbers
are compared, copied into tickets and matched against log output.

## 6. Enforcement, and the two standalone documents

`views/i18n_coverage_test.go` fails when a template calls `T`/`TN`/`TF`/`P` with a key no
dictionary defines in **either** language, and when any template stops parsing. Run
`go test ./internal/ui/views/...` before calling a page done — the missing-key marker
`⟦key⟧` is otherwise only visible to a reader who knows both languages.

`login.html` and `setup.html` are standalone documents: they inherit no shell, so they
open with the same head (four stylesheets, the `pmc_theme` no-flash script, the icon
sprite template) and close with the same `auth-foot` row (language switcher + theme
toggle). Their data is `controllers.authPage` — a failure travels as `Failure` (an
`auth.err.*` key) plus `Detail` for `TF`, so the form can name the broken rule instead of
printing a finished English sentence.

Two rules learned the hard way:

- A control's border is `--border-control`, never `--border-default`. A control fills with
  `--bg-tertiary`, one step above the panel behind it, so a panel hairline measured ~1.3:1
  against its own fill and the field read as disabled. Panels keep the lighter hairline;
  controls get the stronger one.
- Never prepend `v` to a version. `APP_VERSION` already carries it (`v0.2.61-src`), so
  `v{{.Version}}` renders `vv0.2.61`.

## 7. A green build is not evidence a page renders

Templates are type-checked when they execute, not when the package compiles, and Go
streams the response as it is written. A template that names a field its data struct does
not define — or passes a type a helper was not declared for — emits the shell first, then
appends `template error: …` **after** it, and the request still returns **200**. Three real
bugs in this migration hid behind that:

- the console index passed its own data map to the shell, so the breadcrumb key was absent
  and `<title>` rendered a `nil` (the shell's chrome comes from `ShellData`, one builder);
- `frag_overview.html` passed Docker's `int` CPU count to a helper declared for `int64`;
- five pages referenced `ErrKey` / `SiteKnown` / `Base` on structs that never defined them.

So verify by rendering, in both modes, and look inside the body — a status code will not
tell you:

```sh
curl -s -H 'HX-Request: true' http://127.0.0.1:8042/web/tls | grep -c 'template error'   # htmx fragment path
curl -s http://127.0.0.1:8042/web/tls | grep -c 'template error'                        # full document path
```

The same check catches `⟦key⟧`: an untranslated key renders as its own name inside those
markers, which reads as noise rather than as a missing translation. Numeric helpers accept
`any` and coerce (`i18n.Num` / `i18n.Float`) precisely because a count can arrive as `int`,
`int64` or `uint64` depending on whether it came from a struct or from JSON — a helper
declared for one type fails the page for the other.

## 8. A key that lives in Go

`views/i18n_coverage_test.go` only sees keys written *in a template*. A controller also
hands keys to its template at render time (`page.MsgKey`, `d.ErrKey`), and a template scan
is blind to those — so a controller naming a key the dictionaries never defined leaks the
raw key into the UI, on a path that only runs after a mutation succeeds or fails, which is
exactly where a screenshot sweep does not look. `views/i18n_keysource_test.go` scans the Go
sources for key-shaped literals and requires both dictionaries to define each one; it skips
literals ending in a TLD or a file extension so hostnames and filenames are not reported.

Settled term decisions, where no glossary entry existed (siblings must match):

- **manifest → ملف التعريف** — not المانيفست, not بيان (which reads as "statement").
- **console → لوحة التحكم** — a lone اللوحة reads as any panel or board.

## 9. The Arabic reviewer pass

One writer per dictionary file is not enough for a language the writers cannot check: the
four drafting agents each produced fluent Arabic that disagreed with its siblings. The
second pass handed the whole 844-key payload to stronger models with the binding glossary
and the `arabic-ui` rules, then adjudicated their findings here, in one place.

**Who reviewed.** `codex exec` against three models — `gpt-6-astra`, `gpt-5.6-sol`,
`gpt-5.6-terra` — each given the same brief, the glossary, and every key as
`key ⇥ EN ⇥ AR`, 118 keys flagged. `sol` and `terra` also returned a list of systematic
notes; where two models independently flagged the same key, treat it as a defect rather than
a preference. `claude` and the `opencode` catalogue were unavailable (expired session /
out of credit), and `codex` needs the prompt on **stdin** — its bubblewrap sandbox cannot
start on this host (`Failed RTM_NEWADDR: Operation not permitted`), so a filesystem-using
prompt dies at the first tool call and reports "0 entries".

**What the reviewers found, and what was accepted.**

| Class | Decision |
| --- | --- |
| Field hints, placeholders, input instructions | **Imperative** (`أدخل كلمة مرور من 8 أحرف`), per the `arabic-ui` skill — 15 strings were declarative. Buttons stay المصدر. |
| `{0} يومًا` / `{0} سطرًا` counts | The fixed accusative serves 11–99 only. Counts are labelled instead (`الأيام المتبقية: {0}`) unless a `plural.*` key already exists. |
| `cluster API` | Latin **API** stays visible: واجهة API للعنقود. `واجهة العنقود` dropped the acronym the Settings page calls `عنوان API`. |
| `admin` | مدير النظام (the glossary term) — four strings had drifted to المسؤول. |
| `webhook` | خطاف الويب / خطافات الويب everywhere; eight strings had a bare الخطاف. |
| `body` | المتن, not الجسم — settled in favour of the term the config templates already used. |
| Status vs action | أ status is a participle: `فاشل`, `غير منسوخة احتياطيًا`. A completed system act is passive: `رُفض طلب الإطلاق من الخادم.` Failure copy uses تعذّر, not `لم تُزل`. |
| Rejected | Rewrites that only re-worded already-clear sentences (`*_unknown_body`), and any proposal that added information the English does not carry. |

**The traps worth keeping.** `صدفة` is a seashell — "interactive shell" had been rendered as
`صدفة تفاعلية`; it is now `بيئة أوامر تفاعلية`. And `يرفضها العملاء` reversed the source: the
source says clients are the ones being rejected. Both were invisible to a fluent-looking
dictionary and only surfaced when a model was asked to read every line against its English.

**Process rule.** Models propose, one reviewer decides. Applying their output mechanically
would have churned ~40 strings that were already correct, and produced two different
renderings of "roll back" in sibling keys.

## Bidi islands, calque bans, script purity

Sentence-level isolation lives in the **dictionary**, not in the templates: markup wraps data cells
(`dir="ltr"`), but a sentence held in a value can only protect itself. Wrap an island when a Latin run
abuts `:`/`(` plus a placeholder (`⁨Docker: {0}⁩`, `⁨otel: {0}⁩، ⁨traefik: {1}⁩`) or when two Latin runs are
split by a waw (`⁨stdout⁩ و⁨stderr⁩`) — otherwise the placeholder or the pair reads back-to-front. FSI/PDI
(`U+2068`/`U+2069`) is the convention already used by `tls.*` and `webhooks.*`; keep the counts balanced.
In Go source write them as `\u2068`/`\u2069` **escapes**, never as raw characters inside a string literal:
`golangci-lint`'s ST1018 turns a raw pair into a CI failure even though the render is correct, and the
escape is byte-identical at run time.
A lone Latin token between Arabic words needs no island.

Banned calques, checked value-by-value (the rule comments in `dict_ar_*.go` quote these forms as examples,
so scan **value lines only** — `grep -hP '^\s*"'`):

| Banned | Fix |
| --- | --- |
| `عبر` as an instrumental mediator (`عبر الشهادة`) | the باء: `بالشهادة الافتراضية` |
| `لمدة` before a duration (`لمدة 7 أيام`) | drop it: `صالحة 7 أيام` |
| `كـ` prefixing a role (`كحزمة جديدة`) | accusative of specification: `حزمةً جديدة` |
| `تم` + masdar, `فشل في` + masdar | المبني للمجهول (`أُطلق`), `تعذّر` / `لن ينجح` |
| `الخاص بك` | attached كاف: `مفتاحك` |
| `لا يجب`, `هناك`, `الأكثر` + adjective, `كن حذرًا`, `بشكل`/`بصورة`, `قم ب`/`يرجى` | reword; see the `arabic-ui` checklist |

Script purity and counts: no Persian/Urdu letters and no Arabic-Indic digits — test
`\u067e \u0686 \u0698 \u06af \u06a9 \u06cc \u06c0` and `\u0660-\u0669` with explicit code points, because
`ه` (U+0647) is ordinary Arabic heh and an `[ء-ي]` range also swallows `، ؛ ؟`. Counting punctuation
inside values requires parsing the string: every Go map entry ends with a comma, so a line-wise `,` count
returns the number of keys and looks like 844 violations.

## Values that mix digits with Latin letters

`toBytes` hands the template `"7.7 GiB"`, and the neutral space between the digit run and the letter run
resolves to the **paragraph** direction — bidi rule N1 counts EN digits as R for neutral resolution — so in
Arabic the two runs lay out right-to-left and the memory stat renders `GiB 7.7` (measured: the Latin run at
x165-186, the digits at x206-225, against digits-then-letters in English). The isolation belongs in the
**formatter** (`isolate()` in `views/render.go`), not at the call site: the template's `dir="ltr"` span
fixes only the template that remembers to add it, and `tb` is called by both the overview stats and the
deploy preview size. An isolate is a no-op in the English render.

**A credential that does not exist.** `settings.secrets_empty_body` named an "OTF token" that appears
nowhere in the tree (upstream never had the string either) — the real credentials are `edge_api_token`,
`sso_client_secret`, `edge_ui_secret`, `zo_root_user_password`. It now reads `the API token` / `رمز API`.

## Order-of-runs gate

`/tmp/bidi_order.py` walks every leaf element whose text holds both a digit and a Latin run, takes x/y for
each whitespace-delimited token, and flags a same-row adjacent pair whose x descends while the logical
order ascends. Three artifacts produced false flags before the checks were right:

- Zero-width isolates have no width of their own: their rects sit at the run boundary, so a first-char /
  last-char test reads `⁨HMAC-SHA256⁩` as reversed. Strip isolates and non-token punctuation first.
- A wrapped text node has no `\n`, so tokens on the second visual row start left of the first row's tail —
  compare tokens only when their `top` matches. Multi-line error bodies and shell samples, same reason.
- Adjacent LTR-ish tokens are in correct order whenever x ascends; a rule written the other way round
  flags `Ubuntu 24.04.5` as a defect.

## Placeholder examples stay bare

`https://github.com/acme/bookfair` is 256px of type; prefixed with `أدخل عنوان المستودع، مثل` it needs 446px
and the 298px field clips it mid-URL with no ellipsis (measured at 390px). Example values are
language-neutral and mirror the English dictionary verbatim; the imperative belongs in the field label and
its hint, which sit outside the box and cannot be clipped. The imperative-hint rule in `arabic-ui` is about
hints, not example values.
