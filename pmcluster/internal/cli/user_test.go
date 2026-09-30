package cli

import (
	"context"
	"strings"
	"testing"
)

// tokenFromOutput pulls the one-time pmc_ bearer token out of
// `user create`'s stdout (the indented line after the "shown once" banner).
func tokenFromOutput(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "   pmc_") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no pmc_ token line in output:\n%s", out)
	return ""
}

// rowFields returns the whitespace-separated columns of the listing row
// whose name column matches.
func rowFields(rows []string, name string) []string {
	for _, r := range rows {
		f := strings.Fields(r)
		if len(f) >= 3 && f[1] == name {
			return f
		}
	}
	return nil
}

// TestUserCreateStackFlag covers `pmcluster user create <name> --stack`:
// the flag scopes the minted token to that stack (and says so), omitting it
// keeps the historical unscoped token, and `user list` shows the scope in
// its STACK column.
func TestUserCreateStackFlag(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()

	scopedCmd, scopedBuf, _ := newTestCmd("create <name>", map[string]string{"stack": ""}, runUserCreate)
	if err := scopedCmd.Flags().Set("stack", "demo"); err != nil {
		t.Fatalf("set --stack: %v", err)
	}
	if err := scopedCmd.RunE(scopedCmd, []string{"ci"}); err != nil {
		t.Fatalf("user create --stack: %v", err)
	}
	scopedText := scopedBuf.String()
	if !strings.Contains(scopedText, `scoped to stack "demo"`) {
		t.Errorf("create output missing scope note:\n%s", scopedText)
	}

	plainCmd, plainBuf, _ := newTestCmd("create <name>", map[string]string{"stack": ""}, runUserCreate)
	if err := plainCmd.RunE(plainCmd, []string{"admin"}); err != nil {
		t.Fatalf("user create: %v", err)
	}
	plainText := plainBuf.String()
	if !strings.Contains(plainText, "unscoped") {
		t.Errorf("create output missing unscoped note:\n%s", plainText)
	}

	// The scope must be real, not just prose: look the minted tokens up the
	// way the daemon's Bearer middleware does.
	st, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	scopedUser, err := st.UserByToken(ctx, tokenFromOutput(t, scopedText))
	if err != nil || scopedUser == nil {
		t.Fatalf("UserByToken (scoped): %v, user=%v", err, scopedUser)
	}
	if scopedUser.Stack != "demo" {
		t.Errorf("scoped token stack = %q, want demo", scopedUser.Stack)
	}
	plainUser, err := st.UserByToken(ctx, tokenFromOutput(t, plainText))
	if err != nil || plainUser == nil {
		t.Fatalf("UserByToken (unscoped): %v, user=%v", err, plainUser)
	}
	if plainUser.Stack != "" {
		t.Errorf("unscoped token stack = %q, want empty", plainUser.Stack)
	}

	// user list renders the STACK column ("-" = unscoped).
	listCmd, listBuf, _ := newTestCmd("list", nil, runUserList)
	if err := listCmd.RunE(listCmd, nil); err != nil {
		t.Fatalf("user list: %v", err)
	}
	listing := listBuf.String()
	if !strings.Contains(listing, "STACK") {
		t.Errorf("list output missing STACK header:\n%s", listing)
	}
	rows := strings.Split(listing, "\n")
	if f := rowFields(rows, "ci"); len(f) < 3 || f[2] != "demo" {
		t.Errorf("list ci row = %v, want STACK column %q (full output:\n%s)", f, "demo", listing)
	}
	if f := rowFields(rows, "admin"); len(f) < 3 || f[2] != "-" {
		t.Errorf("list admin row = %v, want STACK column %q (full output:\n%s)", f, "-", listing)
	}
}
