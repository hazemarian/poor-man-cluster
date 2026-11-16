package runners

import (
	"strings"
	"testing"
)

// TestParseDockerSize pins the units docker actually prints. They are DECIMAL
// (kB/MB/GB), not powers of two: a parser that assumed 1024 would report a 1.5 GB
// volume as 1.4 GB and disagree with `docker system df` in the very next column.
func TestParseDockerSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0B", 0},
		{"760B", 760},
		{"262.2kB", 262200},
		{"1.5GB", 1500000000},
		{"12MB", 12000000},
		{"2TB", 2000000000000},
		{"N/A", 0}, // docker says this for a volume it could not measure
		{"", 0},    // and this for one with no size yet
	}
	for _, c := range cases {
		got, err := ParseDockerSize(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q = %d, want %d", c.in, got, c.want)
		}
	}
	// An unknown unit must be an error, not a silent zero: "0 bytes" is a number an
	// operator would believe.
	if _, err := ParseDockerSize("5 elephants"); err == nil {
		t.Fatal("an unknown unit was accepted")
	}
}

func TestHumanBytesMatchesDockerUnits(t *testing.T) {
	for in, want := range map[int64]string{
		0:          "0B",
		760:        "760B",
		262200:     "262.2kB",
		1500000000: "1.5GB",
		5368709120: "5.4GB",
	} {
		if got := HumanBytes(in); got != want {
			t.Fatalf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestCacheUsageOfReportsEveryStoreIncludingEmptyOnes: a cache that exists and is empty
// and a cache that was not found are different, and only one of them is normal. Reporting
// the empty one keeps the page honest after a fresh start.
func TestCacheUsageOfReportsEveryStoreIncludingEmptyOnes(t *testing.T) {
	usage := CacheUsageOf([]VolumeSize{
		{Name: "pmc-w1-org-cache-go", Bytes: 300},
		{Name: "pmc-other-cache-go", Bytes: 999},
		{Name: "unrelated", Bytes: 999},
	}, "pmc-w1-org")

	if len(usage) != len(CacheStores)+1 {
		t.Fatalf("usage = %d entries, want one per cache store plus the data dir: %+v", len(usage), usage)
	}
	byName := map[string]CacheUsage{}
	for _, u := range usage {
		byName[u.Name] = u
	}
	if byName["go"].Bytes != 300 {
		t.Fatalf("go cache = %d, want 300", byName["go"].Bytes)
	}
	// npm was never created on this node: it must still be reported, at zero.
	if u, ok := byName["npm"]; !ok || u.Bytes != 0 {
		t.Fatalf("an absent cache store was omitted instead of reported as empty: %+v", byName)
	}
	// Another runner's volume must never be counted.
	for _, u := range usage {
		if strings.Contains(u.Volume, "other") {
			t.Fatalf("another runner's volume leaked in: %+v", u)
		}
	}
}

// TestCacheWarningsNameTheVolumeAndBothNumbers: the warning has to be actionable. "The
// cache is large" is not; "the go cache is 6.2GB (limit 5.4GB) in volume X" is.
func TestCacheWarningsNameTheVolumeAndBothNumbers(t *testing.T) {
	usage := []CacheUsage{
		{Name: "go", Volume: "pmc-w1-org-cache-go", Bytes: 6200000000, Limit: DefaultCacheLimitBytes},
		{Name: "npm", Volume: "pmc-w1-org-cache-npm", Bytes: 100, Limit: DefaultCacheLimitBytes},
	}
	warn := CacheWarnings(usage)
	if len(warn) != 1 {
		t.Fatalf("warnings = %v, want exactly the over-limit cache", warn)
	}
	for _, want := range []string{"go", "6.2GB", "5.4GB", "pmc-w1-org-cache-go"} {
		if !strings.Contains(warn[0], want) {
			t.Fatalf("the warning does not name %q: %s", want, warn[0])
		}
	}
	// Exactly at the limit is not over it: a cache that reaches its ceiling and stays
	// there must not warn forever.
	at := []CacheUsage{{Name: "go", Volume: "v", Bytes: DefaultCacheLimitBytes, Limit: DefaultCacheLimitBytes}}
	if len(CacheWarnings(at)) != 0 {
		t.Fatal("a cache exactly at its limit was reported as over it")
	}
}

func TestTotalCacheBytes(t *testing.T) {
	total := TotalCacheBytes([]CacheUsage{{Bytes: 1}, {Bytes: 2}, {Bytes: 0}})
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
}
