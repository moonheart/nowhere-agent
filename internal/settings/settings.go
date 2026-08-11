// Package settings provides runtime-settable platform configuration: keys the
// operator changes from the admin console instead of by editing env and
// restarting. Boot semantics: env values remain the defaults; a row in
// platform_settings overrides them for the life of the process (or until the
// console writes a new value).
//
// The Runtime keeps an in-memory snapshot loaded from Postgres at boot.
// Reads are lock-free after load (RWMutex); a write goes to Postgres first
// and then updates the snapshot, so the change applies on the next use with
// no restart. Multi-instance deployments converge on the next write from any
// console; a boot reload picks up rows written elsewhere.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
)

// ErrNotFound is returned when no row exists for a key.
var ErrNotFound = errors.New("settings: key not found")

// Store persists setting rows in Postgres.
type Store struct {
	db *sql.DB
}

// NewStore builds a Store over a database handle.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Get loads one row's raw JSON value.
func (s *Store) Get(ctx context.Context, key string) (json.RawMessage, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM platform_settings WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// List loads every row into a map.
func (s *Store) List(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM platform_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var raw []byte
		if err := rows.Scan(&k, &raw); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(raw)
	}
	return out, rows.Err()
}

// Set upserts one row. nil/JSON-null removes the row (back to the env default).
func (s *Store) Set(ctx context.Context, key string, value json.RawMessage) error {
	if len(value) == 0 || string(value) == "null" {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM platform_settings WHERE key = $1`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO platform_settings (key, value, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, []byte(value))
	return err
}

// Runtime is the in-memory settings view every read path consults.
type Runtime struct {
	store *Store
	log   *slog.Logger

	mu     sync.RWMutex
	values map[string]json.RawMessage
	// defaults holds the boot-time env-derived values, used only when no row
	// overrides them.
	defaults map[string]json.RawMessage
}

// NewRuntime builds a Runtime seeded with defaults (env values). Call Load
// before serving to pick up persisted rows.
func NewRuntime(store *Store, defaults map[string]json.RawMessage, log *slog.Logger) *Runtime {
	if log == nil {
		log = slog.Default()
	}
	return &Runtime{
		store: store, log: log,
		values:   map[string]json.RawMessage{},
		defaults: defaults,
	}
}

// Load refreshes the snapshot from the store (called at boot; also usable as
// a manual reload).
func (rt *Runtime) Load(ctx context.Context) error {
	rows, err := rt.store.List(ctx)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	rt.values = rows
	rt.mu.Unlock()
	return nil
}

// raw returns the effective value for key: the persisted row, else the
// default. Second return is false when neither exists.
func (rt *Runtime) raw(key string) (json.RawMessage, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if v, ok := rt.values[key]; ok {
		return v, true
	}
	v, ok := rt.defaults[key]
	return v, ok
}

// String returns the effective string value for key ("" when unset).
func (rt *Runtime) String(key string) string {
	raw, ok := rt.raw(key)
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// Int returns the effective integer value (0 when unset or malformed).
func (rt *Runtime) Int(key string) int {
	raw, ok := rt.raw(key)
	if !ok {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	return n
}

// Set writes a value (Postgres first, then the local snapshot). A JSON null
// removes the row, returning the key to its env default.
func (rt *Runtime) Set(ctx context.Context, key string, value json.RawMessage) error {
	if err := rt.store.Set(ctx, key, value); err != nil {
		return err
	}
	rt.mu.Lock()
	if len(value) == 0 || string(value) == "null" {
		delete(rt.values, key)
	} else {
		rt.values[key] = value
	}
	rt.mu.Unlock()
	rt.log.Info("platform setting updated", "key", key)
	return nil
}

// Keys returns the known runtime-setting keys (for validation and the admin
// surface).
func Keys() []string {
	return []string{
		KeyHTTPToolAllowlist,
		KeyQueryDBDsns,
		KeyWebhookURL,
		KeySystemLang,
		KeyRateLimitRPS,
		KeyRateLimitBurst,
	}
}

// Keys is the runtime's view of the known keys (the admin surface lists
// them with their current effective values).
func (rt *Runtime) Keys() []string { return Keys() }

// Key names (documented in the admin console page).
const (
	// KeyHTTPToolAllowlist is the comma-separated http_request host allowlist
	// (same syntax as HTTP_TOOL_ALLOWLIST).
	KeyHTTPToolAllowlist = "http_tool_allowlist"
	// KeyQueryDBDsns is the comma-separated name=dsn list for query_db (same
	// syntax as QUERY_DB_DSNS).
	KeyQueryDBDsns = "query_db_dsns"
	// KeyWebhookURL is the global run-completion notification target
	// (overrides WEBHOOK_URL; task/notify_url targets still win per run).
	KeyWebhookURL = "webhook_url"
	// KeySystemLang is the system-prompt language ("en" | "zh"; overrides
	// LLM_SYSTEM_LANG for NEW runs).
	KeySystemLang = "llm_system_lang"
	// KeyRateLimitRPS / KeyRateLimitBurst tune the per-IP HTTP limiter (0/0 =
	// disabled; overrides HTTP_RATE_LIMIT_*).
	KeyRateLimitRPS   = "rate_limit_rps"
	KeyRateLimitBurst = "rate_limit_burst"
)
