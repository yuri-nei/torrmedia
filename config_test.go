package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	keys := []string{"TM_CFG_PLAIN", "TM_CFG_DOUBLE", "TM_CFG_SINGLE", "TM_CFG_DOLLAR", "TM_CFG_EMPTY", "TM_CFG_SET"}
	for _, k := range keys {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	t.Setenv("TM_CFG_SET", "from-env")

	path := filepath.Join(t.TempDir(), "config.env")
	content := strings.Join([]string{
		"# comment",
		"",
		"TM_CFG_PLAIN=:8091",
		`TM_CFG_DOUBLE="/Users/me/Library/Application Support/torrmedia"`,
		"TM_CFG_SINGLE='it''s'",
		"  TM_CFG_DOLLAR = pa$$word  ",
		"TM_CFG_EMPTY=",
		"TM_CFG_SET=from-file",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"TM_CFG_PLAIN":  ":8091",
		"TM_CFG_DOUBLE": "/Users/me/Library/Application Support/torrmedia",
		"TM_CFG_SINGLE": "it''s",
		"TM_CFG_DOLLAR": "pa$$word",
		"TM_CFG_EMPTY":  "",
		"TM_CFG_SET":    "from-env",
	}
	for k, v := range want {
		got, ok := os.LookupEnv(k)
		if !ok || got != v {
			t.Errorf("%s = %q (set=%t), want %q", k, got, ok, v)
		}
	}
}

func TestLoadEnvFileRejectsMalformedLines(t *testing.T) {
	for _, line := range []string{"JUSTTEXT", "1BAD=x", "BAD KEY=x", "=x"} {
		path := filepath.Join(t.TempDir(), "config.env")
		if err := os.WriteFile(path, []byte("OK_KEY=1\n"+line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Unsetenv("OK_KEY")
		err := loadEnvFile(path)
		os.Unsetenv("OK_KEY")
		if err == nil || !strings.Contains(err.Error(), ":2:") {
			t.Errorf("line %q: err = %v, want a line-2 error", line, err)
		}
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	if err := loadEnvFile(filepath.Join(t.TempDir(), "absent.env")); err == nil {
		t.Fatal("missing file was accepted")
	}
}
