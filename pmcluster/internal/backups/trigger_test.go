package backups

import (
	"reflect"
	"testing"
)

func TestParseArchivePaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single archive on one line",
			in:   "Successfully created backup at /archive/backup-node1-2026-05-10.tar.gz",
			want: []string{"/archive/backup-node1-2026-05-10.tar.gz"},
		},
		{
			name: "multiple archives across lines",
			in:   "Took /archive/a.tar.gz.\nThen /archive/b.tar.gz now done",
			want: []string{"/archive/a.tar.gz", "/archive/b.tar.gz"},
		},
		{
			name: "no archives",
			in:   "Backup pending; nothing yet.",
			want: nil,
		},
		{
			name: "ignores non-archive paths",
			in:   "/var/log/x.log /archive/c.tar.gz /tmp/y.tar.gz",
			want: []string{"/archive/c.tar.gz"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseArchivePaths(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseArchivePaths(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
