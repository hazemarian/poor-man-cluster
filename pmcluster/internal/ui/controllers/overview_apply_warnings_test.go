package controllers

import (
	"strings"
	"testing"
)

// applyRecordWithWarnings is the /api/cluster/apply body for a degraded apply:
// three recorded warnings, the shape the daemon writes after a partial run.
const applyRecordWithWarnings = `{"present":true,"at":"2026-11-08T22:41:07.123456Z",` +
	`"version":"v0.2.262","source":"reconcile","stacks":["edge"],"configs":[],` +
	`"warnings":["edge_traefik could not be rebuilt","backup agent: no object store",` +
	`"observability_parseable: retention unchanged"],"warning_count":3,` +
	`"line":"last platform apply: 2026-11-08T22:41:07Z by reconcile (v0.2.262)"}`

// TestOverview_ApplyWarningsBannerFormatsItsArguments: the dicts carried printf
// verbs (`%d step(s)`, `recorded at %s by %s`) that nothing substituted, so the
// banner read literally "could not finish %d step(s)". The count, timestamp and
// actor must reach the page.
func TestOverview_ApplyWarningsBannerFormatsItsArguments(t *testing.T) {
	prev := applyRecordFixture
	applyRecordFixture = applyRecordWithWarnings
	t.Cleanup(func() { applyRecordFixture = prev })

	h := newCtrlHarness(t)
	h.mountOverview()
	body := h.get(t, "/web/overview").Body.String()

	if !strings.Contains(body, "could not finish 3 step(s)") {
		t.Errorf("banner title does not carry the real warning count:\n%s", body)
	}
	banner := applyBanner(t, body)
	for _, verb := range []string{"%d", "%s"} {
		if strings.Contains(banner, verb) {
			t.Errorf("rendered banner still shows the raw verb %q:\n%s", verb, banner)
		}
	}
	if !strings.Contains(banner, "recorded at 2026-11-08 22:41 UTC by reconcile") {
		t.Errorf("recorded-at line does not carry a formatted timestamp and the actor:\n%s", banner)
	}
	for _, w := range []string{"edge_traefik could not be rebuilt", "observability_parseable: retention unchanged"} {
		if !strings.Contains(banner, w) {
			t.Errorf("warning %q missing from the banner list", w)
		}
	}

	// Arabic is why the verbs stay in the dicts: the arg positions move, so the
	// substitution must be the dictionary's ({0}/{1}, not a Go format string).
	body = h.get(t, "/web/overview?lang=ar").Body.String()
	banner = applyBanner(t, body)
	if !strings.Contains(banner, "لم يكمل 3 خطوة") {
		t.Errorf("arabic banner title does not carry the count:\n%s", banner)
	}
	if strings.Contains(banner, "%d") || strings.Contains(banner, "%s") {
		t.Errorf("arabic banner still shows a raw verb:\n%s", banner)
	}
	if !strings.Contains(banner, "مسجّل عند 2026-11-08 22:41 UTC بواسطة reconcile") {
		t.Errorf("arabic recorded-at line does not carry timestamp and actor:\n%s", banner)
	}
}

// applyBanner cuts the apply-warnings banner out of the overview body. The page
// renders several warn banners (version skew, quorum); an assertion about this
// one must not be satisfied — or broken — by another.
func applyBanner(t *testing.T, body string) string {
	t.Helper()
	anchor := strings.Index(body, "could not finish")
	if anchor < 0 {
		anchor = strings.Index(body, "لم يكمل")
	}
	if anchor < 0 {
		t.Fatalf("overview body has no apply-warnings banner:\n%s", body)
	}
	start := strings.LastIndex(body[:anchor], `<div class="banner warn">`)
	if start < 0 {
		t.Fatalf("apply-warnings banner has no banner container:\n%s", body[anchor:])
	}
	end := len(body)
	if next := strings.Index(body[start+1:], `<div class="banner`); next >= 0 {
		end = start + 1 + next
	}
	return body[start:end]
}

// TestOverview_ApplyWarningsAbsentWhenNone: an apply that degraded nothing must
// not render the banner at all — a warning banner with an empty list is worse
// than no banner, because it says something is wrong and shows nothing.
func TestOverview_ApplyWarningsAbsentWhenNone(t *testing.T) {
	prev := applyRecordFixture
	applyRecordFixture = `{"present":true,"at":"2026-11-08T22:41:07Z","version":"v0.2.262",` +
		`"source":"reconcile","stacks":["edge"],"configs":[],"warnings":[],"warning_count":0}`
	t.Cleanup(func() { applyRecordFixture = prev })

	h := newCtrlHarness(t)
	h.mountOverview()
	body := h.get(t, "/web/overview").Body.String()

	if strings.Contains(body, "could not finish") {
		t.Errorf("zero warnings must render no banner:\n%s", body)
	}
	if strings.Contains(body, "recorded at") {
		t.Errorf("zero warnings must render no recorded-at line:\n%s", body)
	}
}
