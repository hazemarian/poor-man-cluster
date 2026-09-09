package cluster

import (
	"testing"
)

func TestRequestedTLSMode(t *testing.T) {
	cases := []struct {
		in   UpInput
		want string
	}{
		{UpInput{ACMEEmail: "a@b.c"}, "acme"},
		{UpInput{CertPath: "/c.pem"}, "cert"},
		{UpInput{KeyPath: "/k.pem"}, "cert"},
		{UpInput{}, ""},
	}
	for i, c := range cases {
		if got := requestedTLSMode(c.in); got != c.want {
			t.Errorf("case %d: requestedTLSMode = %q, want %q", i, got, c.want)
		}
	}
}

func TestMergeTLSState_NoStored(t *testing.T) {
	in := UpInput{ACMEEmail: "a@b.c"}
	merged, err := mergeTLSState(in, tlsState{})
	if err != nil {
		t.Fatalf("mergeTLSState: %v", err)
	}
	if merged != in {
		t.Error("input should be unchanged when no stored state")
	}
}

func TestMergeTLSState_ReusesStoredPathsWhenFlagsOmitted(t *testing.T) {
	in := UpInput{}
	state := tlsState{Mode: "cert", CertPath: "/c.pem", KeyPath: "/k.pem"}
	merged, err := mergeTLSState(in, state)
	if err != nil {
		t.Fatalf("mergeTLSState: %v", err)
	}
	if merged.CertPath != "/c.pem" || merged.KeyPath != "/k.pem" {
		t.Errorf("stored cert/key not filled in: %+v", merged)
	}
}

func TestMergeTLSState_SameModeAllowsFlags(t *testing.T) {
	in := UpInput{CertPath: "/new.pem"}
	state := tlsState{Mode: "cert", CertPath: "/old.pem", KeyPath: "/k.pem"}
	merged, err := mergeTLSState(in, state)
	if err != nil {
		t.Fatalf("mergeTLSState: %v", err)
	}
	if merged.CertPath != "/new.pem" {
		t.Errorf("supplied cert should win, got %q", merged.CertPath)
	}
	if merged.KeyPath != "/k.pem" {
		t.Errorf("missing key should be filled from store, got %q", merged.KeyPath)
	}
}

func TestMergeTLSState_RefusesModeFlipUnlessForced(t *testing.T) {
	// Stored=acme but operator requests cert without --force-tls-mode.
	in := UpInput{CertPath: "/c.pem", KeyPath: "/k.pem"}
	state := tlsState{Mode: "acme", ACMEEmail: "a@b.c"}
	if _, err := mergeTLSState(in, state); err == nil {
		t.Fatal("expected mode-flip error")
	}
	in.ForceTLSMode = true
	if _, err := mergeTLSState(in, state); err != nil {
		t.Fatalf("force should allow the flip: %v", err)
	}
}
