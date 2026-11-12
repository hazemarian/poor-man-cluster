package middleware

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Gzip compresses the console's text responses (HTML fragments, CSS, JS, SVG,
// JSON). The console is operated over mobile links as often as over fibre; its
// stylesheets alone are ~190 KB raw, and compression cuts a cold load by more
// than half.
//
// It stays out of the way of anything that is not plain text over HTTP/1.1
// request/response: websocket upgrades (the terminal), already-encoded or
// already-compressed bodies (fonts, images), HEAD requests, bodiless statuses
// and responses too small to be worth the CPU. The decision is made when the
// handler first writes, because only then is the Content-Type known.
func Gzip() gin.HandlerFunc {
	pool := sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression); return w }}
	return func(c *gin.Context) {
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") ||
			c.Request.Method == http.MethodHead ||
			c.GetHeader("Upgrade") != "" {
			c.Next()
			return
		}
		gw := &gzipWriter{ResponseWriter: c.Writer, pool: &pool}
		c.Writer = gw
		defer gw.close()
		c.Next()
	}
}

type gzipWriter struct {
	gin.ResponseWriter
	pool    *sync.Pool
	gz      *gzip.Writer
	decided bool
}

// compressible reports whether a Content-Type is plain text worth compressing.
func compressible(ct string) bool {
	ct = strings.ToLower(ct)
	for _, p := range []string{"text/", "application/javascript", "application/json", "application/xml", "image/svg+xml"} {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

func (w *gzipWriter) decide(status int) {
	if w.decided {
		return
	}
	w.decided = true
	h := w.Header()
	if status == http.StatusNoContent || status == http.StatusNotModified || (status >= 100 && status < 200) ||
		h.Get("Content-Encoding") != "" || !compressible(h.Get("Content-Type")) {
		return
	}
	if n := h.Get("Content-Length"); n != "" && len(n) < 4 { // under 1000 bytes: not worth it
		return
	}
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	w.gz = w.pool.Get().(*gzip.Writer)
	w.gz.Reset(w.ResponseWriter)
}

// WriteHeader only records the status (gin defers the real write); the decision
// waits for WriteHeaderNow/Write, by which point the handler has set its headers.
func (w *gzipWriter) WriteHeader(code int) { w.ResponseWriter.WriteHeader(code) }

// WriteHeaderNow is where gin commits the headers, so it is the last moment
// the encoding can still be chosen.
func (w *gzipWriter) WriteHeaderNow() {
	if !w.Written() {
		w.decide(w.Status())
	}
	w.ResponseWriter.WriteHeaderNow()
}

func (w *gzipWriter) Write(b []byte) (int, error) {
	if !w.Written() {
		w.decide(w.Status())
	}
	if w.gz != nil {
		w.ResponseWriter.WriteHeaderNow()
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *gzipWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *gzipWriter) Flush() {
	if !w.Written() {
		w.decide(w.Status())
	}
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	w.ResponseWriter.Flush()
}

func (w *gzipWriter) close() {
	if w.gz != nil {
		_ = w.gz.Close()
		w.pool.Put(w.gz)
		w.gz = nil
	}
}
