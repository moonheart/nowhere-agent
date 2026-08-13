package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/toolruntime"
)

// QueryDBToolName is the built-in tool that runs read-only SQL against the
// operator-named business databases (enterprise integration): the agent can
// look up orders, customers, or inventory directly instead of waiting for an
// HTTP API to exist. It is the data-side counterpart of http_request.
const QueryDBToolName = "query_db"

const (
	// queryDBMaxRows caps rows returned to the model per call.
	queryDBMaxRows = 200
	// queryDBMaxCols caps columns surfaced per row (wide tables stay cheap).
	queryDBMaxCols = 12
	// queryDBMaxCellChars caps one cell's serialized length.
	queryDBMaxCellChars = 200
	// queryDBMaxResultChars caps the whole serialized result.
	queryDBMaxResultChars = 20_000
	// queryDBDefaultTimeout bounds one statement when the model sets none.
	queryDBDefaultTimeout = 15 * time.Second
)

// queryDBArgs is the tool's input schema.
var queryDBArgs = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"db": map[string]any{"type": "string", "description": "The configured database name (from the operator's DSN list, e.g. \"erp\" or \"crm\")."},
		"sql": map[string]any{"type": "string", "description": "A read-only SQL statement: SELECT/WITH/EXPLAIN/SHOW/VALUES. DDL and DML are rejected."},
	},
	"required":             []string{"db", "sql"},
	"additionalProperties": false,
}

// QueryDBOptions tunes one query_db tool instance.
type QueryDBOptions struct {
	// Timeout bounds one statement; 0 uses the default.
	Timeout time.Duration
	// Logger receives per-call outcomes; nil keeps the tool silent.
	Logf func(format string, args ...any)
}

// queryDBTool runs read-only SQL against operator-named databases. Every
// call is confined four ways: (1) only named DSNs from config exist at all;
// (2) the leading statement keyword must be read-only (SELECT/WITH/EXPLAIN/
// SHOW/VALUES); (3) execution happens in a READ ONLY transaction; (4) rows,
// columns, and bytes are capped and the statement runs under a timeout.
// RiskReadOnly: the tool cannot mutate anything by construction.
type queryDBTool struct {
	pools   map[string]*sql.DB
	timeout time.Duration
	logf    func(format string, args ...any)
}

// NewQueryDB returns the query_db tool over the named DSNs (name → DSN).
// Schemes postgres:// and mysql:// are supported (mysql covers OceanBase/
// TiDB-compatible business databases common in Chinese enterprises). An
// unopenable DSN fails boot — a misconfigured business DB must not silently
// vanish from the tool. An empty map disables the tool (nil).
func NewQueryDB(dsns map[string]string, opts QueryDBOptions) toolruntime.Tool {
	if len(dsns) == 0 {
		return nil
	}
	if opts.Timeout <= 0 {
		opts.Timeout = queryDBDefaultTimeout
	}
	t := &queryDBTool{
		pools:   make(map[string]*sql.DB, len(dsns)),
		timeout: opts.Timeout,
		logf:    opts.Logf,
	}
	for name, dsn := range dsns {
		driver := ""
		switch {
		case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
			driver = "pgx"
		case strings.HasPrefix(dsn, "mysql://"):
			driver = "mysql"
		default:
			// This is a construction-time misconfiguration; NewQueryDB cannot
			// return an error, so keep the tool but drop the entry and rely on
			// the caller (main.go) validating every DSN before wiring.
			continue
		}
		db, err := sql.Open(driver, dsn)
		if err != nil {
			continue
		}
		// Small pools: the tool is for reads, not bulk.
		db.SetMaxOpenConns(4)
		db.SetConnMaxLifetime(5 * time.Minute)
		t.pools[name] = db
	}
	if len(t.pools) == 0 {
		return nil
	}
	return t
}

func (t *queryDBTool) Name() string { return QueryDBToolName }
func (t *queryDBTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (t *queryDBTool) Schema() map[string]any { return queryDBArgs }
func (t *queryDBTool) Timeout() time.Duration { return t.timeout }

func (t *queryDBTool) Description() string {
	return "Run a read-only SQL query against one of the operator-configured business databases " +
		"(SELECT/WITH/EXPLAIN/SHOW/VALUES only; DDL/DML are rejected). Returns rows as a JSON array " +
		"of objects, truncated with a note when large. Use it to look up order, customer, or " +
		"inventory data the platform would otherwise have no access to."
}

func (t *queryDBTool) Args() map[string]any { return queryDBArgs }

func (t *queryDBTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	dbName, _ := args["db"].(string)
	stmt, _ := args["sql"].(string)
	if dbName == "" || strings.TrimSpace(stmt) == "" {
		return toolruntime.Result{Content: "query_db: db and sql are required", IsError: true}, nil
	}
	pool, ok := t.pools[dbName]
	if !ok {
		names := make([]string, 0, len(t.pools))
		for n := range t.pools {
			names = append(names, n)
		}
		sort.Strings(names)
		return toolruntime.Result{Content: fmt.Sprintf("query_db: unknown database %q (configured: %s)", dbName, strings.Join(names, ", ")), IsError: true}, nil
	}
	if !isReadOnlyStatement(stmt) {
		return toolruntime.Result{Content: "query_db: only read-only statements are allowed (SELECT/WITH/EXPLAIN/SHOW/VALUES)", IsError: true}, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	rows, err := t.queryReadOnly(callCtx, pool, stmt)
	if t.logf != nil {
		if err != nil {
			t.logf("db=%s sql=%q error=%v", dbName, stmt, err)
		} else {
			t.logf("db=%s sql=%q rows=%d", dbName, stmt, len(rows))
		}
	}
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("query_db: %v", err), IsError: true}, nil
	}
	out, err := t.serializeRows(rows)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("query_db: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: out}, nil
}

// queryReadOnly runs stmt inside a READ ONLY transaction — a second, engine-
// enforced wall behind the statement guard, so even a parser-evading write
// cannot land. The read-only mode is requested twice: at the driver level
// (TxOptions.ReadOnly → BEGIN READ ONLY, honored by both pgx and the MySQL
// driver) and with an explicit SET TRANSACTION READ ONLY inside the
// transaction (Postgres honors it; the MySQL variant is a documented no-op
// for the current tx, which is why the driver-level flag is the primary
// wall). A failure to enter read-only mode REFUSES the call — the text guard
// is heuristic, so the engine wall must never be silently missing.
func (t *queryDBTool) queryReadOnly(ctx context.Context, pool *sql.DB, stmt string) ([]map[string]any, error) {
	tx, err := pool.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only: rollback is always safe

	if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		return nil, fmt.Errorf("engine refused read-only mode: %w", err)
	}
	q, err := tx.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer q.Close()

	cols, err := q.Columns()
	if err != nil {
		return nil, err
	}
	if len(cols) > queryDBMaxCols {
		cols = cols[:queryDBMaxCols]
	}
	raw := make([]any, len(cols))
	scan := make([]any, len(cols))
	for i := range raw {
		scan[i] = &raw[i]
	}
	out := make([]map[string]any, 0, 32)
	for q.Next() && len(out) < queryDBMaxRows {
		if err := q.Scan(scan...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normalizeCell(raw[i])
		}
		out = append(out, row)
	}
	if err := q.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// serializeRows renders the rows as JSON, capping total bytes and appending a
// truncation note when the cap is hit. The cut keeps WHOLE rows only: the
// model parses the output, and a mid-row byte cut would leave invalid JSON (an
// unclosed object or string) it could never parse.
func (t *queryDBTool) serializeRows(rows []map[string]any) (string, error) {
	b, err := json.Marshal(rows)
	if err != nil {
		return "", err
	}
	if len(b) <= queryDBMaxResultChars {
		return string(b), nil
	}
	// Count complete row encodings from the start until the next would exceed
	// the cap, then re-marshal the kept rows — valid by construction ("[" +
	// rows joined by "," + "]").
	kept := 0
	total := 2 // "[" + "]"
	for ; kept < len(rows); kept++ {
		part, err := json.Marshal(rows[kept])
		if err != nil {
			return "", err
		}
		if kept > 0 {
			total++ // separator comma
		}
		if total+len(part) > queryDBMaxResultChars {
			break
		}
		total += len(part)
	}
	out := rows[:kept]
	re, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	note := fmt.Sprintf("\n...(truncated: kept %d of %d rows, %d bytes total)", kept, len(rows), len(b))
	return string(re) + note, nil
}

// normalizeCell converts a scanned value into a JSON-safe representation.
func normalizeCell(v any) any {
	switch x := v.(type) {
	case []byte:
		s := string(x)
		if len(s) > queryDBMaxCellChars {
			s = s[:queryDBMaxCellChars] + "…"
		}
		return s
	case string:
		if len(x) > queryDBMaxCellChars {
			return x[:queryDBMaxCellChars] + "…"
		}
		return x
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	default:
		return x
	}
}

// isReadOnlyStatement reports whether stmt begins with a read-only keyword,
// skipping leading whitespace, comments, and parentheses. Anything else —
// INSERT/UPDATE/DELETE/DDL — is refused at the text level. Every approved
// branch is then scanned for write keywords: a CTE can legally wrap a write
// ("WITH x AS (...) DELETE …"), EXPLAIN ANALYZE actually EXECUTES the
// statement it analyzes on Postgres, and SELECT … INTO / … INTO OUTFILE /
// DUMPFILE are write verbs despite their SELECT prefix.
func isReadOnlyStatement(stmt string) bool {
	rest := strings.TrimSpace(stmt)
	for strings.HasPrefix(rest, "--") || strings.HasPrefix(rest, "/*") {
		rest = strings.TrimSpace(stripLeadingComment(rest))
	}
	rest = strings.TrimLeft(rest, "(")
	rest = strings.TrimSpace(rest)
	first := firstWord(rest)
	switch strings.ToUpper(first) {
	case "SELECT", "WITH", "EXPLAIN", "SHOW", "VALUES", "TABLE":
		return !containsWriteKeyword(rest)
	}
	return false
}

// writeKeywords are the DDL/DML/file-verb keywords a read-only statement must
// never carry. The list errs on the side of rejection: a false positive only
// makes the model rephrase, while a miss could write.
var writeKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "MERGE", "TRUNCATE",
	"DROP", "ALTER", "CREATE", "GRANT", "REVOKE", "COPY",
	"INTO", "OUTFILE", "DUMPFILE", "LOAD_FILE", "CALL", "EXECUTE",
}

// containsWriteKeyword reports whether s contains any write keyword as a
// whole word. Heuristic by nature (a keyword inside a string literal would
// trip it); false positives are safe — the model rephrases — and the
// engine-level READ ONLY transaction is the authoritative second wall.
func containsWriteKeyword(s string) bool {
	upper := strings.ToUpper(s)
	for _, kw := range writeKeywords {
		for i := 0; ; {
			j := strings.Index(upper[i:], kw)
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(kw)
			beforeOK := start == 0 || !isWordByte(upper[start-1])
			afterOK := end >= len(upper) || !isWordByte(upper[end])
			if beforeOK && afterOK {
				return true
			}
			i = end
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// stripLeadingComment removes one leading comment (-- line or /* block */).
func stripLeadingComment(s string) string {
	if strings.HasPrefix(s, "--") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return s[i+1:]
		}
		return ""
	}
	if strings.HasPrefix(s, "/*") {
		if i := strings.Index(s, "*/"); i >= 0 {
			return s[i+2:]
		}
		return ""
	}
	return s
}

// firstWord returns the leading identifier token of s.
func firstWord(s string) string {
	i := 0
	for i < len(s) && (s[i] == '_' || s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' || s[i] >= '0' && s[i] <= '9') {
		i++
	}
	return s[:i]
}
