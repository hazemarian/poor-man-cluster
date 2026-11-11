package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func gzipEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Gzip())
	big := strings.Repeat("body { color: red; }\n", 200)
	r.GET("/css", func(c *gin.Context) { c.Data(200, "text/css; charset=utf-8", []byte(big)) })
	r.GET("/html", func(c *gin.Context) { c.Header("Content-Type", "text/html; charset=utf-8"); c.String(200, big) })
	r.GET("/font", func(c *gin.Context) { c.Data(200, "font/woff2", []byte(big)) })
	r.GET("/tiny", func(c *gin.Context) { c.Header("Content-Length", "2"); c.Data(200, "text/plain", []byte("ok")) })
	r.GET("/empty", func(c *gin.Context) { c.Status(204) })
	r.GET("/redirect", func(c *gin.Context) { c.Redirect(302, "/css") })
	return r
}

func get(r http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGzipCompressesTextWhenAsked(t *testing.T) {
	r := gzipEngine()
	for _, p := range []string{"/css", "/html"} {
		w := get(r, p, map[string]string{"Accept-Encoding": "gzip"})
		if w.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s: not compressed (headers %v)", p, w.Header())
		}
		if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("%s: missing Vary: Accept-Encoding", p)
		}
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatalf("%s: bad gzip: %v", p, err)
		}
		out, _ := io.ReadAll(zr)
		if !strings.HasPrefix(string(out), "body { color: red; }") || len(out) != 200*len("body { color: red; }\n") {
			t.Errorf("%s: body did not round-trip (%d bytes)", p, len(out))
		}
	}
}

func TestGzipLeavesTheRestAlone(t *testing.T) {
	r := gzipEngine()
	gz := map[string]string{"Accept-Encoding": "gzip"}
	if w := get(r, "/css", nil); w.Header().Get("Content-Encoding") != "" {
		t.Error("compressed without Accept-Encoding")
	}
	if w := get(r, "/font", gz); w.Header().Get("Content-Encoding") != "" {
		t.Error("compressed a font")
	}
	if w := get(r, "/tiny", gz); w.Header().Get("Content-Encoding") != "" {
		t.Error("compressed a tiny body")
	}
	if w := get(r, "/empty", gz); w.Header().Get("Content-Encoding") != "" || w.Code != 204 {
		t.Errorf("touched a 204: %d %v", w.Code, w.Header())
	}
	if w := get(r, "/css", map[string]string{"Accept-Encoding": "gzip", "Upgrade": "websocket"}); w.Header().Get("Content-Encoding") != "" {
		t.Error("compressed a websocket upgrade")
	}
	if w := get(r, "/redirect", gz); w.Code != 302 || w.Header().Get("Location") != "/css" {
		t.Errorf("redirect broken: %d %v", w.Code, w.Header())
	}
}
