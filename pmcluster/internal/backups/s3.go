package backups

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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
	return signV4HeadersQuery(cfg, key, nil, now)
}

// signV4HeadersQuery is signV4Headers with an optional canonical query string
// — ListObjectsV2 signs its `?list-type=2&prefix=...` parameters, so they
// must be part of the canonical request.
func signV4HeadersQuery(cfg S3Config, key string, query url.Values, now time.Time) http.Header {
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	u, _ := url.Parse(cfg.Endpoint)
	host := u.Host
	canonicalURI := "/" + cfg.Bucket + "/" + escapePath(key)
	canonicalQuery := canonicalQueryString(query)

	// x-amz-date MUST be part of the signed headers: strict SigV4 verifiers
	// (SeaweedFS) recompute the signature over exactly the headers listed in
	// SignedHeaders and reject a request that omits the date (403
	// SignatureDoesNotMatch). IONOS tolerated the omission; the in-cluster
	// store did not (BUG-029).
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + emptyPayloadSHA + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := "GET\n" + canonicalURI + "\n" + canonicalQuery + "\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + emptyPayloadSHA
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

// canonicalQueryString renders url.Values in the sorted, RFC3986-encoded form
// SigV4 requires for the canonical request's query string.
func canonicalQueryString(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range query[k] {
			parts = append(parts, sigV4Encode(k)+"="+sigV4Encode(v))
		}
	}
	return strings.Join(parts, "&")
}

// sigV4Encode percent-encodes a string per RFC3986 (unreserved -_.~ left
// as-is, space becomes %20), matching SigV4's canonicalisation rules.
func sigV4Encode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
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
// writes locally, so basename of the archive path is the object key). A row
// discovered from the store records the BARE object key as its archive path
// (e.g. "backup-<node>-<ts>.tar.gz"), whose basename is the key itself — so a
// store-only restore resolves to the same object as a local-path restore.
func s3ObjectKey(archivePath string) string {
	return filepath.Base(archivePath)
}

// s3Object is one object in a ListObjectsV2 result page.
type s3Object struct {
	Key          string
	LastModified time.Time
}

// listS3Objects lists every object key under prefix in cfg.Bucket via S3
// ListObjectsV2 (SigV4 GET ?list-type=2&prefix=), following continuation
// tokens across pages until IsTruncated is false.
func listS3Objects(ctx context.Context, cfg S3Config, prefix string) ([]s3Object, error) {
	var out []s3Object
	token := ""
	for {
		page, next, err := listS3ObjectsPage(ctx, cfg, prefix, token)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		token = next
	}
}

// listS3ObjectsPage performs a single ListObjectsV2 request and returns its
// objects plus the next continuation token ("" when the listing is complete).
func listS3ObjectsPage(ctx context.Context, cfg S3Config, prefix, token string) ([]s3Object, string, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, "", fmt.Errorf("parse s3 endpoint: %w", err)
	}
	u.Path = "/" + cfg.Bucket + "/"

	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("prefix", prefix)
	if token != "" {
		q.Set("continuation-token", token)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	for k, v := range signV4HeadersQuery(cfg, "", q, time.Now()) {
		req.Header[k] = v
	}

	resp, err := s3HTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("s3 list %s: %w", prefix, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("s3 list %s: %s: %s", prefix, resp.Status, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		IsTruncated           bool   `xml:"IsTruncated"`
		NextContinuationToken string `xml:"NextContinuationToken"`
		Contents              []struct {
			Key          string `xml:"Key"`
			LastModified string `xml:"LastModified"`
		} `xml:"Contents"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, "", fmt.Errorf("parse s3 list response: %w", err)
	}

	objects := make([]s3Object, 0, len(parsed.Contents))
	for _, c := range parsed.Contents {
		if c.Key == "" {
			continue
		}
		var mod time.Time
		if c.LastModified != "" {
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
				if t, err := time.Parse(layout, c.LastModified); err == nil {
					mod = t
					break
				}
			}
		}
		objects = append(objects, s3Object{Key: c.Key, LastModified: mod})
	}

	next := ""
	if parsed.IsTruncated && parsed.NextContinuationToken != "" {
		next = parsed.NextContinuationToken
	}
	return objects, next, nil
}

// FetchS3Object downloads object key from the configured endpoint into dst
// (a temp file), returning the local path. Exported so the storage-failover
// path (stacks.Service.Move with FromS3) can pull an archive that exists only
// offsite — when the source storage node is down its local archive is
// unreachable.
func FetchS3Object(ctx context.Context, cfg S3Config, key, dst string) error {
	return fetchS3Object(ctx, cfg, key, dst)
}
