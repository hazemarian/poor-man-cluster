package backups

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The check this exists for: a store that answers 200 for a PUT and does not
// keep the object. S3ObjectExists has to say "absent" for a 404 and "present"
// for a 2xx, and it must not mistake a 500 for absence — an unreachable store is
// a different failure from a store that lost the object.
func TestS3ObjectExistsDistinguishesAbsentFromBroken(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantExists bool
		wantErr    bool
	}{
		{"present", http.StatusOK, true, false},
		{"absent", http.StatusNotFound, false, false},
		{"store error", http.StatusInternalServerError, false, true},
		{"forbidden", http.StatusForbidden, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				if !strings.HasPrefix(r.URL.Path, "/bucket/") {
					t.Errorf("path = %q, want a /bucket/<key> request", r.URL.Path)
				}
				if r.Header.Get("Authorization") == "" {
					t.Error("request was not signed")
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			cfg := S3Config{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "a", SecretKey: "b", Region: "us-east-1"}
			exists, err := S3ObjectExists(context.Background(), cfg, "backup-x.tar.gz")
			if gotMethod != http.MethodHead {
				t.Errorf("method = %q, want HEAD", gotMethod)
			}
			if exists != tc.wantExists {
				t.Errorf("exists = %v, want %v", exists, tc.wantExists)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// An unconfigured target must not silently answer "absent": that would let a
// caller report a missing object when the truth is that it never had a
// destination at all.
func TestS3ObjectExistsRefusesAnUnconfiguredTarget(t *testing.T) {
	if _, err := S3ObjectExists(context.Background(), S3Config{}, "k"); err == nil {
		t.Fatal("an unconfigured target must be an error, not a 'no'")
	}
}
