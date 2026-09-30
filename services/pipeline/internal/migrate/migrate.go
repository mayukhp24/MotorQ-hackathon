// Package migrate applies versioned SQL migrations to PostgreSQL and
// idempotent DDL to ClickHouse. It runs as a one-shot job before services
// start (docker compose `migrate`, Helm pre-install hook).
package migrate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect retries until the database accepts connections (fail after ~90 s).
func Connect(ctx context.Context, dsn string, log *slog.Logger) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 0; i < 45; i++ {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		log.Warn("waiting for postgres", "attempt", i, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("postgres unavailable: %w", lastErr)
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Postgres applies *.sql files in lexical order, each in its own
// transaction, recording versions in schema_migrations. A session advisory
// lock prevents two migrators racing.
func Postgres(ctx context.Context, pool *pgxpool.Pool, dir string, rolePasswords map[string]string, log *slog.Logger) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(90210)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(90210)`) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version := filepath.Base(f)
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		log.Info("applied postgres migration", "version", version)
	}
	// Service-account passwords come from the secret store, never from SQL files.
	for role, pw := range rolePasswords {
		if pw == "" {
			continue
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf(`ALTER ROLE %s WITH PASSWORD %s`, role, quoteLiteral(pw))); err != nil {
			return fmt.Errorf("set password for %s: %w", role, err)
		}
	}
	return nil
}

var varRe = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// Render substitutes ${VAR} placeholders; unknown variables are an error
// (fail fast rather than creating a table with a literal "${X}").
func Render(sql string, vars map[string]string) (string, error) {
	var missing []string
	out := varRe.ReplaceAllStringFunc(sql, func(m string) string {
		k := varRe.FindStringSubmatch(m)[1]
		v, ok := vars[k]
		if !ok {
			missing = append(missing, k)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("undefined template variables: %v", missing)
	}
	return out, nil
}

// SplitStatements splits on semicolons that end a line, dropping comment-only
// chunks. ClickHouse's HTTP interface accepts one statement per request.
func SplitStatements(sql string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		line = stripComment(line)
		cur.WriteString(line)
		cur.WriteByte('\n')
		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			if s := clean(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		}
	}
	if s := clean(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// stripComment removes a trailing "-- ..." comment outside single quotes.
func stripComment(line string) string {
	inQuote := false
	for i := 0; i < len(line)-1; i++ {
		switch {
		case line[i] == '\'':
			inQuote = !inQuote
		case !inQuote && line[i] == '-' && line[i+1] == '-':
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return line
}

func clean(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.TrimSuffix(strings.TrimSpace(strings.Join(keep, "\n")), ";")
}

// ClickHouse is a minimal HTTP client used by the migrator and seeder.
type ClickHouse struct {
	URL, User, Password string
	HTTP                *http.Client
}

func (c *ClickHouse) Exec(ctx context.Context, query string, body io.Reader) error {
	u, err := url.Parse(c.URL)
	if err != nil {
		return err
	}
	q := u.Query()
	if body != nil {
		q.Set("query", query)
	}
	u.RawQuery = q.Encode()
	if body == nil {
		body = strings.NewReader(query)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Password)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Query returns the raw TSV response.
func (c *ClickHouse) Query(ctx context.Context, query string) (string, error) {
	u, _ := url.Parse(c.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(query))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.User, c.Password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

// ClickHouseDir applies every *.sql file (statements are idempotent).
func ClickHouseDir(ctx context.Context, ch *ClickHouse, dir string, vars map[string]string, log *slog.Logger) error {
	for i := 0; i < 45; i++ {
		if _, err := ch.Query(ctx, "SELECT 1"); err == nil {
			break
		} else {
			log.Warn("waiting for clickhouse", "attempt", i, "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		sql, err := Render(string(raw), vars)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		for _, stmt := range SplitStatements(sql) {
			if err := ch.Exec(ctx, stmt, nil); err != nil {
				return fmt.Errorf("%s: %w\n%s", filepath.Base(f), err, firstLine(stmt))
			}
		}
		log.Info("applied clickhouse migration", "file", filepath.Base(f))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}
