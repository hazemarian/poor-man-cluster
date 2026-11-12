package runners

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The caches a runner keeps between jobs are the one thing about it that grows without
// the platform removing anything, so they need a ceiling and a way to be seen.
//
// The build cache already has one: the inner daemon is pruned with
// `builder prune --keep-storage`. The Go module cache and the npm store do not — a
// repository with a large dependency tree can add gigabytes per week and nothing in the
// platform would notice until the disk is full, at which point the failure lands in an
// unrelated job as "no space left on device".
const (
	// DefaultCacheLimitBytes is when a package cache (go, npm) is called out. 5 GiB is
	// generous for a module store and small enough to notice before a node fills up.
	DefaultCacheLimitBytes int64 = 5 << 30
	// DefaultDataLimitBytes is when the inner daemon's data directory is called out.
	// It holds image layers plus a build cache the platform already caps at 10 GB, so
	// the ceiling is deliberately higher than that cap.
	DefaultDataLimitBytes int64 = 20 << 30
)

// CacheUsage is one persistent cache directory and how big it is.
type CacheUsage struct {
	// Name is the store ("go", "npm") or "dind-data" for the daemon's data dir.
	Name string `json:"name"`
	// Volume is the docker volume that holds it.
	Volume string `json:"volume"`
	Bytes  int64  `json:"bytes"`
	// Limit is the ceiling it is being compared against, in bytes.
	Limit int64 `json:"limit"`
}

// Over reports whether this cache has passed its ceiling.
func (c CacheUsage) Over() bool { return c.Limit > 0 && c.Bytes > c.Limit }

// CacheUsageOf lists the caches belonging to one runner and compares them with their
// ceilings. A volume that does not exist yet is reported as 0 bytes rather than omitted:
// "the cache exists and is empty" and "there is no cache" are different states, and only
// one of them is normal after a fresh start.
func CacheUsageOf(vols []VolumeSize, runnerName string) []CacheUsage {
	if runnerName == "" {
		return nil
	}
	byName := make(map[string]int64, len(vols))
	for _, v := range vols {
		byName[v.Name] = v.Bytes
	}
	out := make([]CacheUsage, 0, len(CacheStores)+1)
	for _, store := range CacheStores {
		name := runnerName + "-cache-" + store.Name
		out = append(out, CacheUsage{
			Name: store.Name, Volume: name,
			Bytes: byName[name], Limit: DefaultCacheLimitBytes,
		})
	}
	data := runnerName + "-dind-data"
	out = append(out, CacheUsage{
		Name: "dind-data", Volume: data,
		Bytes: byName[data], Limit: DefaultDataLimitBytes,
	})
	return out
}

// CacheWarnings turns the over-limit caches into sentences an operator can act on. It
// returns one line per cache, naming the volume and both numbers, because "cache is
// large" without a number is a warning nobody can decide about.
//
// The console renders these verbatim and the doctor repeats them, so the wording is
// written once, here.
func CacheWarnings(us []CacheUsage) []string {
	var out []string
	for _, u := range us {
		if !u.Over() {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s cache is %s (limit %s) in volume %s — prune it or raise the limit, or this node will fill up",
			u.Name, HumanBytes(u.Bytes), HumanBytes(u.Limit), u.Volume))
	}
	sort.Strings(out)
	return out
}

// TotalCacheBytes is what every cache of one runner adds up to, for a single row that says
// "this node is holding N of cache" without listing four numbers.
func TotalCacheBytes(us []CacheUsage) int64 {
	var total int64
	for _, u := range us {
		total += u.Bytes
	}
	return total
}

// HumanBytes formats a byte count the way docker does (decimal units), so a number here
// matches the one `docker system df` prints for the same volume.
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + string("kMGTPE"[exp]) + "B"
}

// ParseDockerSize reads the size strings `docker system df` prints ("0B", "262.2kB",
// "1.234GB"). They are decimal units, not powers of two: a parser that assumed 1024 would
// understate a 1.5 GB volume by 7 % and disagree with docker's own output.
func ParseDockerSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// Rare forms docker prints when a volume is shared or unknown.
	if s == "N/A" || s == "<nil>" {
		return 0, nil
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	mult := float64(1)
	switch strings.ToUpper(s[i:]) {
	case "", "B":
	case "KB":
		mult = 1e3
	case "MB":
		mult = 1e6
	case "GB":
		mult = 1e9
	case "TB":
		mult = 1e12
	case "PB":
		mult = 1e15
	default:
		return 0, fmt.Errorf("size %q: unknown unit", s)
	}
	return int64(num * mult), nil
}

// VolumeSize is one docker volume's name and size.
type VolumeSize struct {
	Name  string
	Bytes int64
}

// ReadCacheUsage asks the local docker daemon for the runner's cache volumes and compares
// them with their ceilings. Reading it from docker rather than from the filesystem is
// deliberate: the volumes are docker's property, a host `du` would have to know where
// they live, and on a node with several runners that path is the wrong question.
func ReadCacheUsage(ctx context.Context, d LocalDocker, runnerName string) ([]CacheUsage, error) {
	vols, err := d.VolumeSizes(ctx, runnerName)
	if err != nil {
		return nil, err
	}
	return CacheUsageOf(vols, runnerName), nil
}
