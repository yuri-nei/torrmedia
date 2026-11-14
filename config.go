package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// loadEnvFile reads KEY=VALUE settings from path into the process
// environment. The format is the one systemd's EnvironmentFile uses for
// config.env: one assignment per line, blank lines and lines starting with #
// are skipped, and a value may be wrapped in single or double quotes. Nothing
// is expanded or unescaped, so a password with $ or a path with spaces needs
// no shell quoting. A variable that is already set in the environment wins
// over the file, so a one-off override on the command line still works.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		if !ok || !validEnvKey(key) {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		value = unquote(strings.TrimSpace(value))
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}
	return sc.Err()
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// defaultViewedFile is where viewed marks live when TORRMEDIA_VIEWED_FILE is
// not set: the systemd state directory on Linux, the user's config directory
// elsewhere (~/Library/Application Support/torrmedia on macOS).
func defaultViewedFile() string {
	if runtime.GOOS == "linux" {
		return "/var/lib/torrmedia/viewed.json"
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "torrmedia", "viewed.json")
	}
	return "viewed.json"
}
