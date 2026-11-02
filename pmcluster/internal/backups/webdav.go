package backups

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
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

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
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
	resp, err := http.DefaultClient.Do(req)
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
