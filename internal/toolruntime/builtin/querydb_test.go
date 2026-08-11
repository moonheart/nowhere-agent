package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func randHexBuiltin() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// queryDBDsns points the tool at the dev Postgres (the same one the platform
// itself runs on) under a test-only name. The statement guard and read-only
// transaction are exercised against a real engine.
func queryDBDsns() map[string]string {
	return map[string]string{"testdb": "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"}
}

func testQueryDB(t *testing.T) *queryDBTool {
	t.Helper()
	tool := NewQueryDB(queryDBDsns(), QueryDBOptions{})
	if tool == nil {
		t.Skip("no postgres reachable")
	}
	return tool.(*queryDBTool)
}

func TestQueryDBStatementGuard(t *testing.T) {
	for _, stmt := range []string{
		"DROP TABLE users",
		"DELETE FROM users",
		"UPDATE users SET email='x'",
		"INSERT INTO users (email) VALUES ('x')",
		"CREATE TABLE x (id int)",
		"TRUNCATE users",
		"  -- comment\nDELETE FROM users",   // comment-then-write must still be refused
		"/* c */ DROP TABLE users",          // block-comment-then-write refused
		"WITH x AS (SELECT 1) DELETE FROM t", // WITH leading to DML refused? WITH is read-only-approved... see below
	} {
		if isReadOnlyStatement(stmt) {
			t.Errorf("isReadOnlyStatement(%q) = true, want false", stmt)
		}
	}
	for _, stmt := range []string{
		"SELECT 1",
		"select id from users limit 1",
		"  -- note\nSELECT 1",           // comment-then-read is fine
		"/* note */ SELECT 1",           // block-comment-then-read fine
		"(SELECT 1)",                    // parenthesized read
		"WITH x AS (SELECT 1) SELECT * FROM x", // WITH read
		"EXPLAIN SELECT 1",
		"SHOW TABLES",
		"VALUES (1, 2)",
		"TABLE users",
	} {
		if !isReadOnlyStatement(stmt) {
			t.Errorf("isReadOnlyStatement(%q) = false, want true", stmt)
		}
	}
}

func TestQueryDBRejectsWriteEndToEnd(t *testing.T) {
	tool := testQueryDB(t)
	ctx := context.Background()
	res, err := tool.Call(ctx, map[string]any{"db": "testdb", "sql": "DELETE FROM users WHERE email = 'never@exists.invalid'"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("write statement must come back as an error")
	}
	if !strings.Contains(res.Content, "read-only") {
		t.Fatalf("error should name the guard: %q", res.Content)
	}
}

func TestQueryDBUnknownDatabase(t *testing.T) {
	tool := testQueryDB(t)
	res, err := tool.Call(context.Background(), map[string]any{"db": "nope", "sql": "SELECT 1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "unknown database") {
		t.Fatalf("unknown db: %+v", res)
	}
}

func TestQueryDBMissingArgs(t *testing.T) {
	tool := testQueryDB(t)
	for _, args := range []map[string]any{
		{},
		{"db": "testdb"},
		{"sql": "SELECT 1"},
	} {
		res, err := tool.Call(context.Background(), args)
		if err != nil {
			t.Fatalf("call %v: %v", args, err)
		}
		if !res.IsError {
			t.Errorf("args %v: want error, got %q", args, res.Content)
		}
	}
}

func TestQueryDBReadsRealRows(t *testing.T) {
	tool := testQueryDB(t)
	// Seed a row through a direct connection, then read it back via the tool.
	ctx := context.Background()
	pool := tool.pools["testdb"]
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x')`, "qdb-"+randHexBuiltin()+"@test.dev"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer pool.ExecContext(ctx, `DELETE FROM users WHERE email LIKE 'qdb-%@test.dev'`)

	res, err := tool.Call(ctx, map[string]any{"db": "testdb", "sql": "SELECT email FROM users WHERE email LIKE 'qdb-%@test.dev' ORDER BY email LIMIT 1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("read returned error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "@test.dev") {
		t.Fatalf("result missing seeded row: %q", res.Content)
	}
}

func TestQueryDBDisabledWhenNoDSNs(t *testing.T) {
	if got := NewQueryDB(nil, QueryDBOptions{}); got != nil {
		t.Fatal("empty DSN map must disable the tool")
	}
	if got := NewQueryDB(map[string]string{}, QueryDBOptions{}); got != nil {
		t.Fatal("empty DSN map must disable the tool")
	}
}
