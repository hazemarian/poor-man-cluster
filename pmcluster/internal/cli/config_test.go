package cli

import (
	"strings"
	"testing"
)

// TestReadConfigValue_TrimsPipedNewline verifies the echo/printf newline trap
// is gone: piped input must be stored exactly as typed (no trailing \n).
func TestReadConfigValue_TrimsPipedNewline(t *testing.T) {
	orig := configValueStdin
	defer func() { configValueStdin = orig }()
	configValueStdin = strings.NewReader("s3cr3t-value\n")
	got, err := readConfigValue("")
	if err != nil {
		t.Fatalf("readConfigValue: %v", err)
	}
	if got != "s3cr3t-value" {
		t.Fatalf("readConfigValue = %q, want %q (trailing newline must be trimmed)", got, "s3cr3t-value")
	}
}

// TestReadConfigValue_KeepsExplicitArg verifies the --value flag path is
// untouched (no trimming of user-provided values).
func TestReadConfigValue_KeepsExplicitArg(t *testing.T) {
	got, err := readConfigValue("explicit-value")
	if err != nil {
		t.Fatalf("readConfigValue: %v", err)
	}
	if got != "explicit-value" {
		t.Fatalf("readConfigValue = %q, want %q", got, "explicit-value")
	}
}

// TestConfigDeleteCommandRegistered verifies `pmcluster config delete` is part
// of the config command group (issue 3: config delete did not exist).
func TestConfigDeleteCommandRegistered(t *testing.T) {
	found := false
	for _, c := range configCmd.Commands() {
		if c.Name() == "delete" {
			found = true
			if c.Use != "delete <name>" {
				t.Fatalf("delete command Use = %q, want %q", c.Use, "delete <name>")
			}
			if c.Args == nil {
				t.Fatal("delete command must require exactly one argument")
			}
		}
	}
	if !found {
		t.Fatal("config delete command not registered under configCmd")
	}
}
