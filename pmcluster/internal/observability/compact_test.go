package observability

import (
	"fmt"
	"strings"
	"testing"
)

func logRow(svc, sev, body, ts string) Row {
	return RowForLog(svc, sev, body, ts)
}

func TestTemplateFoldsVaryingValues(t *testing.T) {
	for in, want := range map[string]string{
		"request 4821 took 153ms":                       "request <NUM> took <DUR>",
		"conn from 10.0.3.7:5432 failed":                "conn from <IP> failed",
		"job 3f2a9c1e-1111-2222-3333-444455556666 done": "job <ID> done",
		"2026-10-18T20:18:15.102Z retrying":             "<TS> retrying",
		"order 0xdeadbeef01 paid":                       "order <ID> paid",
		"connection refused":                            "connection refused",
	} {
		if got := Template(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestCompactFoldsRepeatsIntoOneItemWithCountAndRange(t *testing.T) {
	var rows []Row
	for i := 0; i < 500; i++ {
		rows = append(rows, logRow("web", "ERROR", fmt.Sprintf("db timeout after %dms for user %d", 100+i, 9000+i), fmt.Sprintf("2026-10-18T20:%02d:00", i%60)))
	}
	in := Compact("why?", "s", "e", rows, false)
	if len(in.Evidence) != 1 {
		t.Fatalf("%d items for one repeated error", len(in.Evidence))
	}
	e := in.Evidence[0]
	if e.Count != 500 || e.Message != "db timeout after <DUR> for user <NUM>" || e.Example == "" || e.First == "" || e.First >= e.Last {
		t.Errorf("%+v", e)
	}
	if in.Stats.Records != 500 || in.Stats.Templates != 1 || in.Stats.OmittedItems != 0 {
		t.Errorf("stats %+v", in.Stats)
	}
}

func TestCompactOmitsExampleWhenMessageIsExact(t *testing.T) {
	in := Compact("q", "s", "e", []Row{logRow("web", "ERROR", "connection refused", "")}, false)
	if in.Evidence[0].Example != "" {
		t.Error("example duplicated an exact message")
	}
}

func TestCompactKeepsDistinctServicesAndSeveritiesApart(t *testing.T) {
	in := Compact("q", "s", "e", []Row{
		logRow("web", "ERROR", "boom 1", ""), logRow("api", "ERROR", "boom 2", ""), logRow("web", "INFO", "boom 3", ""),
	}, false)
	if len(in.Evidence) != 3 {
		t.Fatalf("%+v", in.Evidence)
	}
}

func TestCompactRanksErrorsFirstAndSurfacesRareInfoOverHeartbeats(t *testing.T) {
	var rows []Row
	for i := 0; i < 300; i++ {
		rows = append(rows, logRow("web", "INFO", fmt.Sprintf("GET /health %d", 200), ""))
	}
	rows = append(rows, logRow("web", "INFO", "migration started", ""), logRow("web", "ERROR", "disk full", ""), logRow("web", "WARN", "slow query", ""))
	in := Compact("q", "s", "e", rows, false)
	var sev []string
	for _, e := range in.Evidence {
		sev = append(sev, e.Severity+":"+e.Message)
	}
	want := []string{"ERROR:disk full", "WARN:slow query", "INFO:migration started", "INFO:GET /health <NUM>"}
	if strings.Join(sev, "|") != strings.Join(want, "|") {
		t.Fatalf("order %v", sev)
	}
}

func TestCompactBoundsListAndReportsWhatWasLeftOut(t *testing.T) {
	var rows []Row
	for i := 0; i < 100; i++ {
		rows = append(rows, logRow("web", "ERROR", fmt.Sprintf("distinct failure kind-%c%c", 'a'+i%26, 'a'+i/26), ""))
	}
	in := Compact("q", "s", "e", rows, false)
	if len(in.Evidence) != maxEvidence {
		t.Fatalf("%d items", len(in.Evidence))
	}
	if in.Stats.OmittedItems != 60 || in.Stats.OmittedRecords != 60 || !in.Truncated {
		t.Errorf("stats %+v truncated=%v", in.Stats, in.Truncated)
	}
}

func TestCompactMultilineKeepsFirstLineKeyAndBoundedExample(t *testing.T) {
	trace := "panic: nil map at 0x1234abcd5678\n" + strings.Repeat("goroutine 1 [running]:\n", 200)
	in := Compact("q", "s", "e", []Row{logRow("web", "ERROR", trace, ""), logRow("web", "ERROR", strings.Replace(trace, "1234abcd5678", "ffff0000aaaa", 1), "")}, false)
	if len(in.Evidence) != 1 || in.Evidence[0].Count != 2 {
		t.Fatalf("%+v", in.Evidence)
	}
	if n := len([]rune(in.Evidence[0].Example)); n > maxExampleRunes+1 || n == 0 {
		t.Errorf("example length %d", n)
	}
}

func TestCompactIDsAreStableAndCitable(t *testing.T) {
	rows := []Row{logRow("web", "ERROR", "boom 7", "")}
	a := Compact("q", "s", "e", rows, false)
	b := Compact("q", "s", "e", []Row{logRow("web", "ERROR", "boom 99", "")}, false)
	if a.Evidence[0].ID != b.Evidence[0].ID {
		t.Error("same template got different ids")
	}
}

func TestCompactInferSeverityFromMessageOnlyWhenLabelIsStreamDerived(t *testing.T) {
	for _, tc := range []struct{ label, line, want string }{
		{"ERROR", "2026-10-18T20:00:00Z\tinfo\treceiver/x.go:1\tstarting receiver", "INFO"},
		{"ERROR", `time=2026 level=warn msg="slow"`, "WARN"},
		{"ERROR", "I1018 20:00:00.1 server.go:12 listening", "INFO"},
		{"ERROR", "E1018 20:00:00.1 server.go:12 failed", "ERROR"},
		{"ERROR", "[INFO] started", "INFO"},
		{"ERROR", "2026-10-18 18:22:15.851 UTC [27] LOG:  checkpoint complete", "INFO"},
		{"ERROR", "2026-10-18 18:22:15.851 UTC [27] WARNING:  something odd", "WARN"},
		{"ERROR", "2026-10-18 18:22:15.851 UTC [27] ERROR:  relation does not exist", "ERROR"},
		{"ERROR", "2026-10-18 18:22:15.851 UTC [27] FATAL:  terminating connection", "FATAL"},
		{"ERROR", "panic: something broke", "ERROR"},
		{"ERROR", "user said level=info in a sentence of text", "INFO"},
		{"WARN", "level=info but the app said WARN", "WARN"},
		{"INFO", "level=error but the app said INFO", "INFO"},
	} {
		if got := inferSeverity(tc.label, tc.line); got != tc.want {
			t.Errorf("%q/%q: %s, want %s", tc.label, tc.line, got, tc.want)
		}
	}
}
