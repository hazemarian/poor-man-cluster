package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestGetSettingDefault_ReturnsValue(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := st.SettingDefault(ctx, "k", "fallback"); got != "v" {
		t.Errorf("SettingDefault = %q, want v", got)
	}
}

func TestGetSettingDefault_NotFoundSilentlyDefaults(t *testing.T) {
	st := openTestStore(t)
	var buf bytes.Buffer
	st.Log = zerolog.New(&buf)

	if got := st.SettingDefault(context.Background(), "missing", "fallback"); got != "fallback" {
		t.Errorf("SettingDefault = %q, want fallback", got)
	}
	if buf.Len() != 0 {
		t.Errorf("not-found must not log, got: %s", buf.String())
	}
}

func TestGetSettingDefault_LogsWarnOnRealError(t *testing.T) {
	st := openTestStore(t)
	var buf bytes.Buffer
	st.Log = zerolog.New(&buf)

	_ = st.Close() // force a genuine read failure (database is closed)

	if got := st.SettingDefault(context.Background(), "k", "fallback"); got != "fallback" {
		t.Errorf("SettingDefault = %q, want fallback even on a real error", got)
	}
	out := buf.String()
	if !strings.Contains(out, "get setting") || !strings.Contains(out, `"key":"k"`) {
		t.Errorf("expected a warn log naming the key, got: %s", out)
	}
}
