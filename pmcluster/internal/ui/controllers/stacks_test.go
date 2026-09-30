package controllers

import "testing"

func TestStackRowState(t *testing.T) {
	tests := []struct {
		name     string
		services int
		running  int64
		desired  int64
		want     string
	}{
		{"no services", 0, 0, 0, "none"},
		{"healthy", 2, 2, 2, "running"},
		{"degraded", 2, 1, 2, "degraded"},
		// A completed one-shot job (run_once migration) is excluded from
		// the desired/running totals by the caller, so a stack whose only
		// work is finished must read as running, never degraded.
		{"only completed jobs", 1, 0, 0, "running"},
		{"healthy plus completed jobs", 3, 2, 2, "running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := stackRow{Services: tt.services, Running: tt.running, Desired: tt.desired}
			if got := r.State(); got != tt.want {
				t.Errorf("State() = %q, want %q", got, tt.want)
			}
		})
	}
}
