package store

import "testing"

// TestStackSwarmSecretNames pins the two rules a live session taught:
//
//   - a stack's secrets are stored with scope "service" and the stack in the row, so a query for
//     scope "stack" returns nothing and the shell mounts nothing;
//   - the same query returns the stackless service-scope secrets shared by every stack, and those
//     must never be mounted into ONE stack's shell.
func TestStackSwarmSecretNames(t *testing.T) {
	rows := []*SecretMeta{
		{Scope: "service", Stack: "aarma", Name: "aarma_db_password", Hash: "43dd4a0c11"},
		{Scope: "service", Stack: "aarma", Name: "aarma_database_url", Hash: "4fd63b9c22"},
		{Scope: "service", Stack: "", Name: "registry_password", Hash: "aaaa1111bb"},      // shared: exclude
		{Scope: "service", Stack: "other", Name: "other_db_password", Hash: "cccc2222dd"}, // another stack: exclude
		{Scope: "cluster", Stack: "", Name: "offsite_key", Hash: "eeee3333ff"},            // platform: exclude
		nil,
	}
	got := StackSwarmSecretNames(rows, "aarma")
	want := []string{"aarma_db_password_43dd4a0c", "aarma_database_url_4fd63b9c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mount %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStackSwarmConfigNames(t *testing.T) {
	rows := []*ConfigRow{
		{Scope: "service", Stack: "aarma", Name: "aarma_admin_phones", Hash: "0318b337aa"},
		{Scope: "service", Stack: "", Name: "shared_yaml", Hash: "11112222bb"},
	}
	got := StackSwarmConfigNames(rows, "aarma")
	if len(got) != 1 || got[0] != "aarma_admin_phones_0318b337" {
		t.Fatalf("got %v, want only this stack's config", got)
	}
}
