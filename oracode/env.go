package oracode

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const workspaceEnvOverride = "ORACODE_ENV_FILE"

func loadWorkspaceEnv(workspaceRoot string) error {
	candidates := []string{}
	if override := strings.TrimSpace(os.Getenv(workspaceEnvOverride)); override != "" {
		candidates = append(candidates, override)
	}
	candidates = append(candidates,
		filepath.Join(workspaceRoot, ".env"),
		filepath.Join(workspaceRoot, "api", ".env"),
		filepath.Join(workspaceRoot, "watchdog", ".env"),
		filepath.Join(workspaceRoot, "devops", ".env"),
		filepath.Join(filepath.Dir(workspaceRoot), ".env"),
		filepath.Join(filepath.Dir(workspaceRoot), "devops", ".env"),
	)

	for _, path := range candidates {
		if err := loadEnvFile(path); err == nil {
			break
		}
	}
	applyDBDSNFallback()
	return nil
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if existing := os.Getenv(key); existing == "" {
			_ = os.Setenv(key, val)
		}
	}
	return scanner.Err()
}

func applyDBDSNFallback() {
	if strings.TrimSpace(os.Getenv("ORACODE_DB_DSN")) != "" {
		return
	}
	host := strings.TrimSpace(firstEnv("DB_HOST", "POSTGRES_HOST", "PGHOST", "DATABASE_HOST"))
	port := strings.TrimSpace(firstEnv("DB_PORT", "POSTGRES_PORT", "PGPORT"))
	user := strings.TrimSpace(firstEnv("DB_USER", "POSTGRES_USER", "PGUSER"))
	pass := strings.TrimSpace(firstEnv("DB_PASSWORD", "POSTGRES_PASSWORD", "PGPASSWORD"))
	name := strings.TrimSpace(firstEnv("DB_NAME", "POSTGRES_DB", "PGDATABASE"))

	switch {
	case strings.TrimSpace(firstEnv("DATABASE_URL", "POSTGRES_URL", "DB_DSN")) != "":
		_ = os.Setenv("ORACODE_DB_DSN", firstEnv("DATABASE_URL", "POSTGRES_URL", "DB_DSN"))
	case host != "" && user != "" && pass != "" && name != "":
		if port == "" {
			port = "5432"
		}
		u := &url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(user, pass),
			Host:   netHostPort(host, port),
			Path:   name,
		}
		q := u.Query()
		if q.Get("sslmode") == "" {
			q.Set("sslmode", "disable")
		}
		u.RawQuery = q.Encode()
		_ = os.Setenv("ORACODE_DB_DSN", u.String())
	}
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			return val
		}
	}
	return ""
}

func netHostPort(host, port string) string {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" {
		return host
	}
	if port == "" {
		return host
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

func resolveWorkspaceDSN(workspaceRoot string) string {
	_ = loadWorkspaceEnv(workspaceRoot)
	return strings.TrimSpace(os.Getenv("ORACODE_DB_DSN"))
}
