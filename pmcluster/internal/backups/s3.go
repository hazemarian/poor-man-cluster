package backups

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// S3Config is the offsite object-store destination for volume backups. It
// mirrors the backup_s3_* cluster settings; any S3-compatible endpoint works
// (Cloudflare R2, MinIO, AWS S3, ...) because requests are signed with AWS
// Signature V4 and sent path-style.
type S3Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
}

// Configured reports whether a complete offsite destination is set (endpoint,
// bucket and both keys). Region may be "auto" (R2's accepted value).
func (c S3Config) Configured() bool {
	return c.Endpoint != "" && c.Bucket != "" && c.AccessKey != "" && c.SecretKey != ""
}

// emptyPayloadSHA is the SHA-256 of the empty body — every SigV4 GET signs an
// empty payload.
const emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// s3HTTPClient is injectable so tests can point the fetch at a recording
// server without any network.
var s3HTTPClient = http.DefaultClient

// signV4Headers returns the Authorization + date + content-sha256 headers for
// a SigV4-signed GET of object key inside cfg.Bucket (path-style URL).
func signV4Headers(cfg S3Config, key string, now time.Time) http.Header {
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	u, _ := url.Parse(cfg.Endpoint)
	host := u.Host
	canonicalURI := "/" + cfg.Bucket + "/" + escapePath(key)

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + emptyPayloadSHA + "\n"
	signedHeaders := "host;x-amz-content-sha256"

	canonicalRequest := "GET\n" + canonicalURI + "\n\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + emptyPayloadSHA
	creqHash := sha256.Sum256([]byte(canonicalRequest))

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(creqHash[:])

	kDate := hmacSHA256([]byte("AWS4"+cfg.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	h := http.Header{}
	h.Set("x-amz-date", amzDate)
	h.Set("x-amz-content-sha256", emptyPayloadSHA)
	h.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+cfg.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
	return h
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(data))
	return m.Sum(nil)
}

// escapePath percent-encodes each path segment (like AWS's S3 SDK), so object
// keys containing slashes or spaces survive the URL.
func escapePath(key string) string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// fetchS3Object downloads object key from the configured endpoint into dst
// (a temp file), returning the local path. The download is signed with AWS
// Signature V4 so any S3-compatible store accepts it.
func fetchS3Object(ctx context.Context, cfg S3Config, key, dst string) error {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("parse s3 endpoint: %w", err)
	}
	u.Path = "/" + cfg.Bucket + "/" + escapePath(key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	for k, v := range signV4Headers(cfg, key, time.Now()) {
		req.Header[k] = v
	}

	resp, err := s3HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3 get %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("s3 get %s: %s: %s", key, resp.Status, strings.TrimSpace(string(body)))
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// s3ObjectKey is the object name a backup run's archive is stored under:
// the archive filename itself (offen uploads with the same BACKUP_FILENAME it
// writes locally, so basename of the archive path is the object key).
func s3ObjectKey(archivePath string) string {
	return filepath.Base(archivePath)
}
