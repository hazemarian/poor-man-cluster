package runners

import "testing"

// TestAcceptedStatus pins the acceptance rule. The backup record route answers 204 No Content,
// so a client that accepts only 200 reports a record that DID land as "the record did not reach
// the leader" — measured on production (nxt-sw-4-m, 2026-11-16): the row was in the leader's
// table while the agent warned on every run.
func TestAcceptedStatus(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{200, true}, {201, true}, {202, true}, {204, true},
		{301, false}, {400, false}, {403, false}, {500, false}, {501, false},
	} {
		if got := acceptedStatus(tc.code); got != tc.want {
			t.Errorf("acceptedStatus(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}
