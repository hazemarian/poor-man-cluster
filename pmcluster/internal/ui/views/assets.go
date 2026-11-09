package views

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Static assets are content-addressed: every URL the templates emit carries
// ?v=<hash of the bytes actually served>. A deploy that changes a stylesheet
// changes its URL, so a browser that still has the previous document open
// cannot keep running the old file from its HTTP cache for an hour — the
// reason a shipped tooltip style looked broken after v0.2.263.
//
// A URL whose ?v matches the current bytes is immutable and cached for a year;
// a bare or stale URL is revalidated on every use. A stale hash is never a
// 404: the current bytes are served with revalidation, so the query string
// is a cache-buster, not a hard dependency.

// immutableCache is the lifetime for a URL whose version matches its bytes.
const immutableCache = "public, max-age=31536000, immutable"

// revalidateCache is the lifetime for a URL that is unversioned or carries a
// version the deployment no longer has. The browser may keep the bytes but
// must check the ETag before reusing them.
const revalidateCache = "no-cache"

// asset is one static file's served bytes and their content version.
type asset struct {
	body    []byte
	version string
	modTime time.Time
}

var (
	staticAssetsOnce sync.Once
	staticAssets     map[string]asset
	staticSub        fs.FS
)

// contentVersion is the short content hash used in ?v=. Twelve hex characters
// of SHA-256 keep collision risk irrelevant at this scale and the URL short.
func contentVersion(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:6])
}

// buildAssets reads every regular file under fsys, hashes it, and rewrites
// asset-to-asset references (fonts.css -> woff2) so a nested reference is
// versioned too. A stylesheet's own version is computed over its rewritten
// bytes, so changing a font changes the font URL, the stylesheet URL, and
// every page that links it.
func buildAssets(fsys fs.FS) map[string]asset {
	raw := map[string][]byte{}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return nil
		}
		raw[p] = b
		return nil
	})

	versions := make(map[string]string, len(raw))
	for p, b := range raw {
		versions[p] = contentVersion(b)
	}

	out := make(map[string]asset, len(raw))
	for p, b := range raw {
		body := b
		if strings.HasSuffix(p, ".css") {
			body = versionAssetRefs(b, versions)
		}
		out[p] = asset{body: body, version: contentVersion(body)}
	}
	return out
}

const staticRefPrefix = "/web/static/"

// versionAssetRefs rewrites every /web/static/<path> reference inside an asset
// to /web/static/<path>?v=<hash>. The scan stops at the first character that
// cannot belong to a path, so `url(...)`, quoted and unquoted forms, and
// references already carrying ?v= all round-trip unchanged.
func versionAssetRefs(src []byte, versions map[string]string) []byte {
	var out bytes.Buffer
	rest := src
	for {
		i := bytes.Index(rest, []byte(staticRefPrefix))
		if i < 0 {
			out.Write(rest)
			return out.Bytes()
		}
		out.Write(rest[:i+len(staticRefPrefix)])
		rest = rest[i+len(staticRefPrefix):]

		n := 0
		for n < len(rest) && isAssetPathByte(rest[n]) {
			n++
		}
		name := string(rest[:n])
		out.WriteString(name)
		rest = rest[n:]

		if v, ok := versions[name]; ok && !bytes.HasPrefix(rest, []byte("?v=")) {
			out.WriteString("?v=")
			out.WriteString(v)
		}
	}
}

// isAssetPathByte reports whether b may appear in an embedded asset path.
// Deliberately excludes '?', '#', quotes, whitespace and parentheses so the
// rewrite never consumes surrounding CSS/JS syntax.
func isAssetPathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.', b == '/', b == '-', b == '_':
		return true
	}
	return false
}

// consoleAssets builds (once) the versioned table for the embedded assets.
func consoleAssets() map[string]asset {
	staticAssetsOnce.Do(func() {
		sub, err := fs.Sub(StaticFS, "static")
		if err != nil {
			panic(err)
		}
		staticSub = sub
		staticAssets = buildAssets(sub)
	})
	return staticAssets
}

// StaticURL returns the content-addressed URL of one embedded asset. Templates
// call it through the `static` helper so no hand-written /web/static/... URL
// can drift unversioned. An unknown name falls back to the bare path (the
// handler still serves it with revalidation).
func StaticURL(name string) string {
	name = strings.TrimPrefix(name, staticRefPrefix)
	name = strings.TrimPrefix(name, "/")
	if a, ok := consoleAssets()[name]; ok {
		return staticRefPrefix + name + "?v=" + a.version
	}
	return staticRefPrefix + name
}

// StaticHandler serves the console's own CSS, fonts and JS. htmx is vendored
// locally (htmx.min.js) alongside the CSS/font stack so the design and the
// SPA wiring render on any host without a CDN dependency.
//
// The handler serves precomputed bytes rather than delegating to a file server
// so it can set the ETag of the content it actually sends and choose the cache
// policy from the version in the URL.
func StaticHandler() gin.HandlerFunc {
	assets := consoleAssets()
	fallback := http.FileServer(http.FS(staticSub))
	return func(c *gin.Context) {
		rel := strings.TrimPrefix(c.Request.URL.Path, StaticBase)
		rel = strings.TrimPrefix(rel, "/")

		a, ok := assets[rel]
		if !ok {
			// Directories, unknown paths and traversal attempts keep the file
			// server's own semantics (404, redirect, listing) but must never
			// be cached as if they were versioned.
			c.Header("Cache-Control", revalidateCache)
			c.Request.URL.Path = strings.TrimPrefix(c.Request.URL.Path, StaticBase)
			fallback.ServeHTTP(c.Writer, c.Request)
			return
		}

		// ServeContent answers If-None-Match from this ETag with a 304 and
		// handles Range/HEAD; the ETag is of the bytes in a.body, which for a
		// stylesheet includes its rewritten font URLs.
		c.Header("ETag", `"`+a.version+`"`)
		if c.Query("v") == a.version {
			c.Header("Cache-Control", immutableCache)
		} else {
			c.Header("Cache-Control", revalidateCache)
		}
		http.ServeContent(c.Writer, c.Request, path.Base(rel), a.modTime, bytes.NewReader(a.body))
	}
}
