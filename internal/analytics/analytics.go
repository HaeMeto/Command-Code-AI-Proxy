// Package analytics owns the SQLite-backed request log and the
// aggregated views consumed by /analytics and /dashboard.
package analytics

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// MaxBodyBytes caps the per-column size of the captured request /
// response bodies so a runaway payload can't blow up the SQLite
// database. Anything larger is truncated with a marker suffix.
const MaxBodyBytes = 64 * 1024

// Entry is a single proxy request to be persisted.
type Entry struct {
	Method        string
	Path          string
	StatusCode    int
	DurationMs    int64
	Provider      string
	Model         string
	InputTokens   int64
	OutputTokens  int64
	CacheRead     int64
	CacheCreation int64
	MessageID     string
	StopReason    string
	IP            string
	UserAgent     string
	VersionHeader string
	ErrorMessage  string
	RequestBytes  int64
	ResponseBytes int64

	// Captured bodies (admin-gated; capped at MaxBodyBytes per column).
	ClientRequestBody    string // what the client sent to the proxy
	UpstreamRequestBody  string // what the proxy sent to CommandCode
	UpstreamResponseBody string // what CommandCode returned
	ClientResponseBody   string // what the proxy returned to the client
}

// DB wraps a *sql.DB with prepared statements.
type DB struct {
	db *sql.DB
}

// Open creates (or opens) the SQLite database at path and ensures the
// schema is in place.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("ensure dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

// Close releases the database handle.
func (d *DB) Close() error { return d.db.Close() }

func initSchema(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS requests (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at      TEXT NOT NULL,
	method          TEXT NOT NULL,
	path            TEXT NOT NULL,
	status_code     INTEGER,
	duration_ms     INTEGER,
	provider        TEXT,
	model           TEXT,
	input_tokens    INTEGER DEFAULT 0,
	output_tokens   INTEGER DEFAULT 0,
	cache_read      INTEGER DEFAULT 0,
	cache_creation  INTEGER DEFAULT 0,
	message_id      TEXT,
	stop_reason     TEXT,
	ip              TEXT,
	user_agent      TEXT,
	version_header  TEXT,
	error_message   TEXT,
	request_bytes   INTEGER,
	response_bytes  INTEGER,
	client_request_body     TEXT,
	upstream_request_body   TEXT,
	upstream_response_body  TEXT,
	client_response_body    TEXT
);
CREATE INDEX IF NOT EXISTS idx_requests_created_at ON requests(created_at);
CREATE INDEX IF NOT EXISTS idx_requests_model      ON requests(model);
CREATE INDEX IF NOT EXISTS idx_requests_provider   ON requests(provider);
`
	if _, err := db.Exec(ddl); err != nil {
		return err
	}
	// Migrate older databases that pre-date the body columns.
	for _, col := range []string{
		"client_request_body",
		"upstream_request_body",
		"upstream_response_body",
		"client_response_body",
	} {
		if err := ensureColumn(db, "requests", col, "TEXT"); err != nil {
			return fmt.Errorf("migrate %s: %w", col, err)
		}
	}
	return nil
}

// ensureColumn adds a column if it doesn't already exist on the table.
// Used to migrate older SQLite databases without dropping data.
func ensureColumn(db *sql.DB, table, col, def string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + col + " " + def)
	return err
}

// CapBody truncates body bytes to MaxBodyBytes with a clear marker so
// callers always pass it through before persisting.
func CapBody(b []byte) string {
	if len(b) <= MaxBodyBytes {
		return string(b)
	}
	return string(b[:MaxBodyBytes]) + fmt.Sprintf("\n\n... [truncated; original size %d bytes, kept first %d bytes]", len(b), MaxBodyBytes)
}

// Record persists a single Entry and returns its rowid. Errors are
// returned to the caller; the caller usually logs them but does not
// propagate to the client.
func (d *DB) Record(e Entry) (int64, error) {
	const q = `
INSERT INTO requests (
	created_at, method, path, status_code, duration_ms,
	provider, model, input_tokens, output_tokens,
	cache_read, cache_creation, message_id, stop_reason,
	ip, user_agent, version_header, error_message,
	request_bytes, response_bytes,
	client_request_body, upstream_request_body,
	upstream_response_body, client_response_body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	res, err := d.db.Exec(q,
		time.Now().UTC().Format(time.RFC3339Nano),
		valStr(e.Method, "POST"),
		valStr(e.Path, "/"),
		e.StatusCode, e.DurationMs,
		nullIfEmpty(e.Provider), nullIfEmpty(e.Model),
		e.InputTokens, e.OutputTokens,
		e.CacheRead, e.CacheCreation,
		nullIfEmpty(e.MessageID), nullIfEmpty(e.StopReason),
		nullIfEmpty(e.IP), nullIfEmpty(e.UserAgent), nullIfEmpty(e.VersionHeader),
		nullIfEmpty(e.ErrorMessage),
		e.RequestBytes, e.ResponseBytes,
		nullIfEmpty(e.ClientRequestBody), nullIfEmpty(e.UpstreamRequestBody),
		nullIfEmpty(e.UpstreamResponseBody), nullIfEmpty(e.ClientResponseBody),
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// UpdateClientResponse sets the client_response_body column for an
// already-recorded row. Used by the OpenAI handler which knows the
// final translated body only after writing the response.
func (d *DB) UpdateClientResponse(id int64, body string) error {
	_, err := d.db.Exec(
		`UPDATE requests SET client_response_body = ? WHERE id = ?`,
		nullIfEmpty(body), id,
	)
	return err
}

func valStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Totals are the headline numbers shown on the dashboard.
type Totals struct {
	TotalRequests     int64 `json:"total_requests"`
	SuccessCount      int64 `json:"success_count"`
	ErrorCount        int64 `json:"error_count"`
	TotalInputTokens  int64 `json:"total_input_tokens"`
	TotalOutputTokens int64 `json:"total_output_tokens"`
	TotalCacheRead    int64 `json:"total_cache_read"`
	TotalCacheCreate  int64 `json:"total_cache_creation"`
	AvgDurationMs     int64 `json:"avg_duration_ms"`
	MaxDurationMs     int64 `json:"max_duration_ms"`
	LastRequestAt     any   `json:"last_request_at"`
}

// ByModelEntry / ByProviderEntry are aggregate rows keyed by model/provider.
type ByModelEntry struct {
	Model         string `json:"model"`
	Count         int64  `json:"count"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	AvgDurationMs int64  `json:"avg_duration_ms"`
}

// ByProviderEntry mirrors ByModelEntry keyed by provider.
type ByProviderEntry struct {
	Provider      string `json:"provider"`
	Count         int64  `json:"count"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	AvgDurationMs int64  `json:"avg_duration_ms"`
}

// SeriesPoint is one bucket of a time series (hour or day).
type SeriesPoint struct {
	Bucket       string `json:"bucket"`
	Count        int64  `json:"count"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// Summary is the full /analytics payload.
type Summary struct {
	Totals       Totals            `json:"totals"`
	ByModel      []ByModelEntry    `json:"by_model"`
	ByProvider   []ByProviderEntry `json:"by_provider"`
	Last24hHours []SeriesPoint     `json:"last_24h_hours"`
	Last30dDays  []SeriesPoint     `json:"last_30d_days"`
}

// Summary returns the aggregated dashboard payload.
func (d *DB) Summary() (Summary, error) {
	var s Summary
	row := d.db.QueryRow(`
SELECT
	COUNT(*),
	COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(cache_read), 0),
	COALESCE(SUM(cache_creation), 0),
	COALESCE(CAST(AVG(duration_ms) AS INTEGER), 0),
	COALESCE(MAX(duration_ms), 0),
	MAX(created_at)
FROM requests
`)
	var lastAt sql.NullString
	if err := row.Scan(
		&s.Totals.TotalRequests,
		&s.Totals.SuccessCount,
		&s.Totals.ErrorCount,
		&s.Totals.TotalInputTokens,
		&s.Totals.TotalOutputTokens,
		&s.Totals.TotalCacheRead,
		&s.Totals.TotalCacheCreate,
		&s.Totals.AvgDurationMs,
		&s.Totals.MaxDurationMs,
		&lastAt,
	); err != nil {
		return s, err
	}
	if lastAt.Valid {
		s.Totals.LastRequestAt = lastAt.String
	}

	var err error
	if s.ByModel, err = d.byModel(); err != nil {
		return s, err
	}
	if s.ByProvider, err = d.byProvider(); err != nil {
		return s, err
	}
	if s.Last24hHours, err = d.hourly24h(); err != nil {
		return s, err
	}
	if s.Last30dDays, err = d.daily30d(); err != nil {
		return s, err
	}
	return s, nil
}

func (d *DB) byModel() ([]ByModelEntry, error) {
	rows, err := d.db.Query(`
SELECT
	COALESCE(model, '(unknown)'),
	COUNT(*),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(CAST(AVG(duration_ms) AS INTEGER), 0)
FROM requests
GROUP BY COALESCE(model, '(unknown)')
ORDER BY COUNT(*) DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ByModelEntry
	for rows.Next() {
		var e ByModelEntry
		if err := rows.Scan(&e.Model, &e.Count, &e.InputTokens, &e.OutputTokens, &e.AvgDurationMs); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) byProvider() ([]ByProviderEntry, error) {
	rows, err := d.db.Query(`
SELECT
	COALESCE(provider, '(unknown)'),
	COUNT(*),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(CAST(AVG(duration_ms) AS INTEGER), 0)
FROM requests
GROUP BY COALESCE(provider, '(unknown)')
ORDER BY COUNT(*) DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ByProviderEntry
	for rows.Next() {
		var e ByProviderEntry
		if err := rows.Scan(&e.Provider, &e.Count, &e.InputTokens, &e.OutputTokens, &e.AvgDurationMs); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) hourly24h() ([]SeriesPoint, error) {
	rows, err := d.db.Query(`
SELECT
	substr(created_at, 1, 13) AS hour,
	COUNT(*),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0)
FROM requests
WHERE datetime(created_at) >= datetime('now', '-24 hours')
GROUP BY hour
ORDER BY hour ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.Bucket, &p.Count, &p.InputTokens, &p.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) daily30d() ([]SeriesPoint, error) {
	rows, err := d.db.Query(`
SELECT
	substr(created_at, 1, 10) AS day,
	COUNT(*),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0)
FROM requests
WHERE datetime(created_at) >= datetime('now', '-30 days')
GROUP BY day
ORDER BY day ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.Bucket, &p.Count, &p.InputTokens, &p.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecentItem is one row in the recent table.
type RecentItem struct {
	ID            int64  `json:"id"`
	CreatedAt     string `json:"created_at"`
	Path          string `json:"path"`
	StatusCode    int    `json:"status_code"`
	DurationMs    int64  `json:"duration_ms"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CacheRead     int64  `json:"cache_read"`
	CacheCreation int64  `json:"cache_creation"`
	MessageID     string `json:"message_id"`
	StopReason    string `json:"stop_reason"`
	IP            string `json:"ip"`
	ErrorMessage  string `json:"error_message"`
}

// RequestDetail is the full row including the captured bodies. Returned
// by GetDetail for the dashboard's modal.
type RequestDetail struct {
	ID                   int64  `json:"id"`
	CreatedAt            string `json:"created_at"`
	Method               string `json:"method"`
	Path                 string `json:"path"`
	StatusCode           int    `json:"status_code"`
	DurationMs           int64  `json:"duration_ms"`
	Provider             string `json:"provider"`
	Model                string `json:"model"`
	InputTokens          int64  `json:"input_tokens"`
	OutputTokens         int64  `json:"output_tokens"`
	CacheRead            int64  `json:"cache_read"`
	CacheCreation        int64  `json:"cache_creation"`
	MessageID            string `json:"message_id"`
	StopReason           string `json:"stop_reason"`
	IP                   string `json:"ip"`
	UserAgent            string `json:"user_agent"`
	VersionHeader        string `json:"version_header"`
	ErrorMessage         string `json:"error_message"`
	RequestBytes         int64  `json:"request_bytes"`
	ResponseBytes        int64  `json:"response_bytes"`
	ClientRequestBody    string `json:"client_request_body"`
	UpstreamRequestBody  string `json:"upstream_request_body"`
	UpstreamResponseBody string `json:"upstream_response_body"`
	ClientResponseBody   string `json:"client_response_body"`
}

// GetDetail returns the full row for a given id, or sql.ErrNoRows.
func (d *DB) GetDetail(id int64) (RequestDetail, error) {
	var r RequestDetail
	row := d.db.QueryRow(`
SELECT id, created_at, method, path, status_code, duration_ms,
       COALESCE(provider, ''), COALESCE(model, ''),
       input_tokens, output_tokens, cache_read, cache_creation,
       COALESCE(message_id, ''), COALESCE(stop_reason, ''),
       COALESCE(ip, ''), COALESCE(user_agent, ''),
       COALESCE(version_header, ''),
       COALESCE(error_message, ''),
       COALESCE(request_bytes, 0), COALESCE(response_bytes, 0),
       COALESCE(client_request_body, ''),
       COALESCE(upstream_request_body, ''),
       COALESCE(upstream_response_body, ''),
       COALESCE(client_response_body, '')
FROM requests WHERE id = ?
`, id)
	if err := row.Scan(
		&r.ID, &r.CreatedAt, &r.Method, &r.Path, &r.StatusCode, &r.DurationMs,
		&r.Provider, &r.Model,
		&r.InputTokens, &r.OutputTokens, &r.CacheRead, &r.CacheCreation,
		&r.MessageID, &r.StopReason,
		&r.IP, &r.UserAgent, &r.VersionHeader,
		&r.ErrorMessage,
		&r.RequestBytes, &r.ResponseBytes,
		&r.ClientRequestBody, &r.UpstreamRequestBody,
		&r.UpstreamResponseBody, &r.ClientResponseBody,
	); err != nil {
		return r, err
	}
	return r, nil
}

// Recent returns the most recent N entries (descending by id).
func (d *DB) Recent(limit int) ([]RecentItem, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := d.db.Query(`
SELECT id, created_at, path, status_code, duration_ms,
       COALESCE(provider, ''), COALESCE(model, ''),
       input_tokens, output_tokens, cache_read, cache_creation,
       COALESCE(message_id, ''), COALESCE(stop_reason, ''),
       COALESCE(ip, ''), COALESCE(error_message, '')
FROM requests
ORDER BY id DESC
LIMIT ?
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentItem
	for rows.Next() {
		var r RecentItem
		if err := rows.Scan(
			&r.ID, &r.CreatedAt, &r.Path, &r.StatusCode, &r.DurationMs,
			&r.Provider, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.CacheRead, &r.CacheCreation,
			&r.MessageID, &r.StopReason,
			&r.IP, &r.ErrorMessage,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DistinctModels returns model names that have actually been used,
// ordered by recency of last use (most recent first), capped at limit.
// Empty / NULL models are excluded. Used by GET /v1/models to advertise
// any model the proxy has seen, in addition to the static OPENAI_MODELS
// env list.
func (d *DB) DistinctModels(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.Query(`
		SELECT model
		FROM requests
		WHERE model IS NOT NULL AND model != ''
		GROUP BY model
		ORDER BY MAX(id) DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
