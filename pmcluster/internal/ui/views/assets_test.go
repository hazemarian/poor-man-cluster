package views

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

// assetTag matches every tag in a rendered document that pulls in an asset the
// console version pins: stylesheets, scripts and the favicon.
var assetTag = regexp.MustCompile(`<(?:link rel="stylesheet" href|link rel="icon" href|script src)="(/web/static/[^"]+)"`)

var versionSuffix = regexp.MustCompile(`^/web/static/[A-Za-z0-9._/-]+\?v=[0-9a-f]{12}$`)

func assetURLs(t *testing.T, doc string) []string {
	t.Helper()
	var urls []string
	for _, m := range assetTag.FindAllStringSubmatch(doc, -1) {
		urls = append(urls, m[1])
	}
	return urls
}

func renderDoc(t *testing.T, r *Renderer, name string, data any) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/web/", nil)
	r.Page(c, name, data)
	if rec.Code != http.StatusOK {
		t.Fatalf("render %s = %d, want 200: %s", name, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestRenderedShellVersionsEveryAsset counts the asset tags in each console
// document instead of spot-checking one link: a new stylesheet or script that
// forgets the version parameter would silently reintroduce the stale-CSS bug.
func TestRenderedShellVersionsEveryAsset(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	reqCtx, _ := gin.CreateTestContext(rec)
	reqCtx.Request = httptest.NewRequest(http.MethodGet, "/web/", nil)

	pages := []struct {
		name      string
		data      any
		wantTotal int
		wantCSS   int
		wantJS    int
		wantIcon  int
	}{
		{"app", ShellBase(reqCtx), 12, 7, 4, 1},
		{"login", gin.H{}, 9, 7, 1, 1},
		{"setup", gin.H{}, 9, 7, 1, 1},
		{"terminal", gin.H{"Stack": "demo", "Service": "web", "Version": "1"}, 8, 5, 2, 1},
	}

	for _, p := range pages {
		t.Run(p.name, func(t *testing.T) {
			doc := renderDoc(t, r, p.name, p.data)
			urls := assetURLs(t, doc)
			if len(urls) != p.wantTotal {
				t.Fatalf("%s has %d versioned-capable asset tags, want %d: %v", p.name, len(urls), p.wantTotal, urls)
			}
			for _, u := range urls {
				if !versionSuffix.MatchString(u) {
					t.Errorf("%s references an unversioned asset: %s", p.name, u)
				}
			}
			if got := strings.Count(doc, `<link rel="stylesheet"`); got != p.wantCSS {
				t.Errorf("%s has %d stylesheet links, want %d", p.name, got, p.wantCSS)
			}
			if got := strings.Count(doc, `<script src=`); got != p.wantJS {
				t.Errorf("%s has %d external scripts, want %d", p.name, got, p.wantJS)
			}
			if got := strings.Count(doc, `<link rel="icon"`); got != p.wantIcon {
				t.Errorf("%s has %d icon links, want %d", p.name, got, p.wantIcon)
			}
			// No bare /web/static/ reference may survive in the document: a
			// single unversioned URL is enough to pin a deploy-old asset.
			for _, u := range urls {
				if strings.Contains(u, "?v=") {
					continue
				}
				t.Errorf("%s left an unversioned asset reference: %s", p.name, u)
			}
		})
	}
}

// TestRenderedShellAssetsAreStableAcrossRenders asserts the same bytes render
// the same URLs, so an unchanged asset keeps its cache entry.
func TestRenderedShellAssetsAreStableAcrossRenders(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/web/", nil)

	first := assetURLs(t, renderDoc(t, r, "app", ShellBase(c)))
	second := assetURLs(t, renderDoc(t, r, "app", ShellBase(c)))
	if len(first) == 0 {
		t.Fatal("rendered shell referenced no assets")
	}
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("asset URLs changed between renders:\n%v\n%v", first, second)
	}
}

// TestAssetVersionTracksBytes is the core property: the version changes when
// and only when the served bytes change.
func TestAssetVersionTracksBytes(t *testing.T) {
	v1 := buildAssets(fstest.MapFS{
		"a.css":  {Data: []byte("body{color:red}")},
		"b.js":   {Data: []byte("var x=1;")},
		"f.woff": {Data: []byte("FONT")},
	})
	v2 := buildAssets(fstest.MapFS{
		"a.css":  {Data: []byte("body{color:blue}")},
		"b.js":   {Data: []byte("var x=1;")},
		"f.woff": {Data: []byte("FONT")},
	})

	if v1["a.css"].version == v2["a.css"].version {
		t.Error("changed stylesheet kept its version")
	}
	if v1["b.js"].version != v2["b.js"].version {
		t.Error("unchanged script changed its version")
	}
	if v1["a.css"].version == "" || v1["b.js"].version == "" {
		t.Fatal("empty version produced")
	}
}

// TestNestedAssetReferencesAreVersioned covers fonts.css -> woff2: a font
// change must change the font URL, the stylesheet URL that embeds it, and the
// stylesheet's bytes, so the whole chain busts together.
func TestNestedAssetReferencesAreVersioned(t *testing.T) {
	v1 := buildAssets(fstest.MapFS{
		"fonts.css": {Data: []byte("src: url(/web/static/f.woff2) format('woff2');")},
		"f.woff2":   {Data: []byte("FONT-A")},
	})
	v2 := buildAssets(fstest.MapFS{
		"fonts.css": {Data: []byte("src: url(/web/static/f.woff2) format('woff2');")},
		"f.woff2":   {Data: []byte("FONT-B")},
	})

	body := string(v1["fonts.css"].body)
	if !strings.Contains(body, "/web/static/f.woff2?v="+v1["f.woff2"].version) {
		t.Fatalf("nested font reference is not versioned: %s", body)
	}
	if strings.Contains(body, "?v="+v2["f.woff2"].version) {
		t.Error("stylesheet embeds the wrong font version")
	}
	if v1["fonts.css"].version == v2["fonts.css"].version {
		t.Error("stylesheet version did not change when its font changed")
	}

	// Re-running the rewriter over an already-versioned reference must not
	// append a second ?v=.
	again := string(versionAssetRefs(v1["fonts.css"].body, map[string]string{"f.woff2": v1["f.woff2"].version}))
	if again != body {
		t.Errorf("rewrite is not idempotent:\n%s\n%s", body, again)
	}
}

// TestTemplatesNeverHardcodeStaticURLs is the drift guard: every asset
// reference must go through the static helper, or the next added stylesheet
// ships unversioned and the stale-CSS bug returns for that file alone.
func TestTemplatesNeverHardcodeStaticURLs(t *testing.T) {
	entries, err := files.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := files.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.Contains(string(b), "/web/static/") {
			t.Errorf("%s hardcodes a /web/static/ URL; use {{static \"name\"}}", e.Name())
		}
	}
}

// TestStaticHandlerCachingByVersion asserts the policy split: a matching
// version is immutable, everything else revalidates, and a stale version is
// still served rather than 404'd.
func TestStaticHandlerCachingByVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET(StaticBase+"/*filepath", StaticHandler())

	get := func(target string, hdr map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	hashed := StaticURL("components.css")
	if !versionSuffix.MatchString(hashed) {
		t.Fatalf("StaticURL(components.css) = %q, want a versioned URL", hashed)
	}

	bare := get("/web/static/components.css", nil)
	if bare.Code != http.StatusOK {
		t.Fatalf("unversioned GET = %d, want 200", bare.Code)
	}
	if cc := bare.Header().Get("Cache-Control"); cc != revalidateCache {
		t.Errorf("unversioned Cache-Control = %q, want %q", cc, revalidateCache)
	}
	etag := bare.Header().Get("ETag")
	if etag == "" {
		t.Error("static response has no ETag")
	}

	versioned := get(hashed, nil)
	if versioned.Code != http.StatusOK {
		t.Fatalf("versioned GET = %d, want 200", versioned.Code)
	}
	if cc := versioned.Header().Get("Cache-Control"); cc != immutableCache {
		t.Errorf("versioned Cache-Control = %q, want %q", cc, immutableCache)
	}
	if got := versioned.Header().Get("ETag"); got != etag {
		t.Errorf("versioned ETag = %q, want %q", got, etag)
	}
	if !strings.Contains(versioned.Body.String(), "[data-tip]") {
		t.Error("served components.css is missing the tooltip styles")
	}

	notModified := get("/web/static/components.css", map[string]string{"If-None-Match": etag})
	if notModified.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", notModified.Code)
	}
	if notModified.Body.Len() != 0 {
		t.Errorf("304 carried a body of %d bytes", notModified.Body.Len())
	}

	// A stale hash is a cache-buster, never a hard dependency: the current
	// bytes are served, just not under an immutable policy.
	stale := get("/web/static/components.css?v=000000000000", nil)
	if stale.Code != http.StatusOK {
		t.Fatalf("stale-hash GET = %d, want 200 (must not 404)", stale.Code)
	}
	if cc := stale.Header().Get("Cache-Control"); cc != revalidateCache {
		t.Errorf("stale-hash Cache-Control = %q, want %q", cc, revalidateCache)
	}
	if stale.Body.String() != versioned.Body.String() {
		t.Error("stale-hash response did not serve the current bytes")
	}

	// Nested font URLs served from fonts.css are themselves immutable, and
	// every one of them is versioned — not just the first.
	rawFonts, err := StaticFS.ReadFile("static/fonts.css")
	if err != nil {
		t.Fatalf("read fonts.css: %v", err)
	}
	wantRefs := strings.Count(string(rawFonts), "url(/web/static/fonts/")
	if wantRefs == 0 {
		t.Fatal("fonts.css no longer references any font")
	}
	fonts := get(StaticURL("fonts.css"), nil)
	if fonts.Code != http.StatusOK {
		t.Fatalf("fonts.css = %d, want 200", fonts.Code)
	}
	fontRef := regexp.MustCompile(`url\(/web/static/fonts/[A-Za-z0-9._-]+\.woff2\?v=[0-9a-f]{12}\)`)
	refs := fontRef.FindAllString(fonts.Body.String(), -1)
	if len(refs) != wantRefs {
		t.Errorf("fonts.css versions %d of %d font references", len(refs), wantRefs)
	}
	if got, want := strings.Count(fonts.Body.String(), "url(/web/static/fonts/"), wantRefs; got != want {
		t.Errorf("served fonts.css has %d font references, want %d", got, want)
	}
	if len(refs) == 0 {
		t.Fatal("fonts.css does not version its font URLs")
	}
	fontURL := strings.TrimSuffix(strings.TrimPrefix(refs[0], "url("), ")")
	font := get(fontURL, nil)
	if font.Code != http.StatusOK {
		t.Fatalf("font %s = %d, want 200", fontURL, font.Code)
	}
	if cc := font.Header().Get("Cache-Control"); cc != immutableCache {
		t.Errorf("font Cache-Control = %q, want %q", cc, immutableCache)
	}

	// Unknown paths still 404 through the fallback and are not cached.
	missing := get("/web/static/does-not-exist.css", nil)
	if missing.Code != http.StatusNotFound {
		t.Errorf("unknown asset = %d, want 404", missing.Code)
	}
}
