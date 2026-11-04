package backups

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// putTimeout bounds one artifact upload and webdavClient bounds the rest.
//
// These are not decoration. With http.DefaultClient and a ten-minute request
// deadline, a store that accepted the TCP connection and then stopped answering
// held a production run for 22 minutes of wall clock and 1.6 s of CPU — the
// agent sat in epoll, the lock stayed held, and the node's next hourly run
// exited immediately as "already running". A blackhole has to fail fast.
var (
	putTimeout = 5 * time.Minute

	webdavClient = &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
)

// PutWebDAV uploads one file to a WebDAV collection with a plain PUT.
//
// The in-cluster store exposes WebDAV on :7333 and creates the bucket path on
// first PUT, which is why the platform's writers use it rather than the S3 API
// (the S3 API does not auto-create a bucket). Credentials may be embedded in
// the URL as userinfo; prefer PMCLUSTER_BACKUP_WEBDAV over the flag so they
// never reach a command line.
func PutWebDAV(ctx context.Context, base, localPath string) error {
	if base == "" {
		return fmt.Errorf("webdav: no endpoint")
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("webdav: parse endpoint: %w", err)
	}
	var user, pass string
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
		u.User = nil
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + url.PathEscape(filepath.Base(localPath))

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("webdav: open %s: %w", filepath.Base(localPath), err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("webdav: stat %s: %w", filepath.Base(localPath), err)
	}

	ctx, cancel := context.WithTimeout(ctx, putTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), io.NopCloser(f))
	if err != nil {
		return fmt.Errorf("webdav: build request: %w", err)
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := webdavClient.Do(req)
	if err != nil {
		return fmt.Errorf("webdav: put %s: %w", filepath.Base(localPath), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webdav: put %s: %s: %s", filepath.Base(localPath), resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
