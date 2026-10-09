package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
)

// TestServiceStateCell covers the UpdateState → STATE cell mapping:
// only "paused" and "updating" get a marker; "" and "completed" stay neutral.
func TestServiceStateCell(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		errMsg     string
		want       string
		notContain []string
	}{
		{name: "no update in flight", state: "", want: "-"},
		{name: "completed update", state: "completed", want: "-"},
		{name: "updating", state: "updating", want: "UPDATING"},
		{name: "paused without error", state: "paused", want: "PAUSED"},
		{
			name:   "paused with error",
			state:  "paused",
			errMsg: "update paused due to failure of task x",
			want:   "PAUSED: update paused due to failure of task x",
		},
		{
			name:       "paused with long error is truncated",
			state:      "paused",
			errMsg:     strings.Repeat("e", stateCellMaxError) + "TAIL-BEYOND-LIMIT",
			want:       "PAUSED: " + strings.Repeat("e", stateCellMaxError) + "…",
			notContain: []string{"TAIL-BEYOND-LIMIT"},
		},
		{
			name:   "paused error whitespace flattened",
			state:  "paused",
			errMsg: "update paused\ndue to\tfailure",
			want:   "PAUSED: update paused due to failure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := serviceStateCell(services.ServiceSummary{
				UpdateState: tc.state,
				UpdateError: tc.errMsg,
			})
			if got != tc.want {
				t.Fatalf("serviceStateCell(%q, %q) = %q, want %q", tc.state, tc.errMsg, got, tc.want)
			}
			for _, bad := range tc.notContain {
				if strings.Contains(got, bad) {
					t.Fatalf("serviceStateCell(%q, %q) = %q, must not contain %q", tc.state, tc.errMsg, got, bad)
				}
			}
		})
	}
}

// TestPrintServices_StateColumn drives printServices directly and checks the
// rendered tabwriter table: a paused service shows PAUSED plus its error, an
// updating one shows UPDATING, and healthy rows keep a neutral STATE cell.
func TestPrintServices_StateColumn(t *testing.T) {
	cmd, out, _ := newTestCmd("list", nil, func(*cobra.Command, []string) error { return nil })

	list := []services.ServiceSummary{
		{
			Name: "demo_web", Stack: "demo", Replicas: 1, Desired: 1,
			Image: "nginx:1.27", Mode: "replicated", Updated: 1700000000,
			UpdateState: "paused",
			UpdateError: "update paused due to failure of task abc123",
		},
		{
			Name: "demo_worker", Stack: "demo", Replicas: 2, Desired: 2,
			Image: "worker:v2", Mode: "replicated", Updated: 1700000000,
			UpdateState: "updating",
		},
		{
			Name: "demo_db", Stack: "demo", Replicas: 1, Desired: 1,
			Image: "postgres:16", Mode: "replicated", Updated: 1700000000,
			UpdateState: "completed",
		},
		{
			Name: "sidecar", Stack: "", Replicas: 0, Desired: 0,
			Image: "busybox", Mode: "global", Updated: 1700000000,
		},
	}

	printServices(cmd, list)
	got := out.String()

	if !strings.Contains(got, "STATE") {
		t.Fatalf("header missing STATE column:\n%s", got)
	}
	lines := linesByName(got)
	if len(lines) != len(list) {
		t.Fatalf("want %d body rows, got %d:\n%s", len(list), len(lines), got)
	}

	paused := lines["demo_web"]
	if !strings.Contains(paused, "PAUSED: update paused due to failure of task abc123") {
		t.Fatalf("paused row missing PAUSED marker + error:\n%s", paused)
	}

	updating := lines["demo_worker"]
	if !strings.Contains(updating, "UPDATING") || strings.Contains(updating, "PAUSED") {
		t.Fatalf("updating row wrong:\n%s", updating)
	}

	for _, healthy := range []string{"demo_db", "sidecar"} {
		row := lines[healthy]
		if strings.Contains(row, "PAUSED") || strings.Contains(row, "UPDATING") {
			t.Fatalf("%s row must carry no update marker:\n%s", healthy, row)
		}
		// every cell except STATE is space-free here, so the token right
		// before the trailing RFC3339 timestamp is the STATE cell
		f := strings.Fields(row)
		if len(f) < 3 {
			t.Fatalf("%s row unparseable:\n%s", healthy, row)
		}
		if f[len(f)-2] != "-" {
			t.Fatalf("%s STATE cell = %q, want %q:\n%s", healthy, f[len(f)-2], "-", row)
		}
		// the pre-existing columns must be intact
		wantUpdated := time.Unix(1700000000, 0).Format(time.RFC3339)
		if f[len(f)-1] != wantUpdated {
			t.Fatalf("%s UPDATED cell = %q, want %q:\n%s", healthy, f[len(f)-1], wantUpdated, row)
		}
	}

	// one-shot long errors must not overflow the cell
	longCmd, longOut, _ := newTestCmd("list", nil, func(*cobra.Command, []string) error { return nil })
	printServices(longCmd, []services.ServiceSummary{{
		Name: "big", Stack: "s", Image: "img", Mode: "replicated", Updated: 1700000000,
		UpdateState: "paused",
		UpdateError: strings.Repeat("x", 500),
	}})
	longRow := linesByName(longOut.String())["big"]
	if !strings.Contains(longRow, "PAUSED: "+strings.Repeat("x", stateCellMaxError)+"…") {
		t.Fatalf("long error not truncated to %d runes:\n%s", stateCellMaxError, longRow)
	}
	if strings.Count(longRow, "x") != stateCellMaxError {
		t.Fatalf("truncated row has %d error runes, want %d:\n%s",
			strings.Count(longRow, "x"), stateCellMaxError, longRow)
	}
}

// TestPrintServices_PlatformColumn drives printServices directly and checks the
// PLATFORM column: a platform-managed service (io.pmcluster.platform=true) is
// marked "platform", a customer app service stays neutral ("-"), and the
// header carries the column.
func TestPrintServices_PlatformColumn(t *testing.T) {
	cmd, out, _ := newTestCmd("list", nil, func(*cobra.Command, []string) error { return nil })

	list := []services.ServiceSummary{
		{Name: "demo_web", Stack: "demo", Replicas: 1, Desired: 1, Image: "nginx", Mode: "replicated", Updated: 1700000000},
		{Name: "infra_traefik", Stack: "infra", Replicas: 1, Desired: 1, Image: "traefik", Mode: "global", Updated: 1700000000, Platform: true},
	}

	printServices(cmd, list)
	got := out.String()

	if !strings.Contains(got, "PLATFORM") {
		t.Fatalf("header missing PLATFORM column:\n%s", got)
	}

	rows := linesByName(got)
	if strings.Contains(rows["demo_web"], "platform") {
		t.Errorf("app row must not be marked platform:\n%s", rows["demo_web"])
	}
	if !strings.Contains(rows["infra_traefik"], "platform") {
		t.Errorf("platform row must be marked:\n%s", rows["infra_traefik"])
	}
}

// TestPlatformCell checks the marker mapping directly.
func TestPlatformCell(t *testing.T) {
	if got := platformCell(services.ServiceSummary{Platform: true}); got != "platform" {
		t.Fatalf("platformCell(platform) = %q, want platform", got)
	}
	if got := platformCell(services.ServiceSummary{}); got != "-" {
		t.Fatalf("platformCell(app) = %q, want -", got)
	}
}

// TestTruncateRunes checks the boundary of the helper directly.
func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("short", 10); got != "short" {
		t.Fatalf("truncateRunes(short, 10) = %q", got)
	}
	if got := truncateRunes("0123456789", 4); got != "0123…" {
		t.Fatalf("truncateRunes = %q, want %q", got, "0123…")
	}
	if got := truncateRunes("héllo", 4); got != "héll…" {
		t.Fatalf("rune (not byte) truncation failed: %q", got)
	}
}

// linesByName maps the service name (first column) to its rendered line.
func linesByName(out string) map[string]string {
	rows := map[string]string{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "NAME") {
			continue
		}
		name := strings.Fields(ln)[0]
		rows[name] = ln
	}
	return rows
}
