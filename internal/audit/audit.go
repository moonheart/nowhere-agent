// Package audit implements the platform's audit trail (enterprise-readiness
// P0): an append-only record of who did what, when, from where, and whether it
// succeeded. It is the compliance backbone for authentication events,
// administrative actions, and credential changes.
//
// The design is deliberately small:
//
//   - A Logger wraps a *sql.DB and exposes one method, Log, which INSERTs a
//     row. There is no update or delete path — the trail is append-only.
//   - Events are built with a fluent Event builder so call sites stay one
//     line and cannot accidentally record a secret (there is no field for one).
//   - Recording is best-effort: Log returns an error but never panics, and the
//     package provides a helper (LogAndReport) that routes failures to slog so
//     an audit write hiccup cannot take a request down. The audit trail must
//     never become a single point of failure for the action it records.
//
// Call sites pass the *http.Request so IP and User-Agent are captured
// uniformly; actor identity is resolved from the request context by identity.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Outcome is whether the recorded action succeeded.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
)

// Action is a dotted event name. The vocabulary is fixed here (rather than
// free-form strings at call sites) so reviews and queries can enumerate it.
// Convention: <area>.<entity>.<verb>.
type Action string

const (
	// Authentication.
	ActionAuthSignup Action = "auth.signup"
	ActionAuthLogin  Action = "auth.login"
	ActionAuthLogout Action = "auth.logout"

	// Self-service account management.
	ActionMePasswordChange Action = "me.password.change"
	ActionMeTokenRevoke    Action = "me.token.revoke"
	// Self-service file uploads (change user-image-uploads).
	ActionMeUploadDelete Action = "me.upload.delete"
	// Self-service data export (data governance).
	ActionMeExport Action = "me.export"

	// Platform administration of accounts.
	ActionAdminUserCreate        Action = "admin.user.create"
	ActionAdminUserUpdate        Action = "admin.user.update"
	ActionAdminUserDisable       Action = "admin.user.disable"
	ActionAdminUserEnable        Action = "admin.user.enable"
	ActionAdminUserResetPassword Action = "admin.user.reset_password"
	ActionAdminUserDelete        Action = "admin.user.delete"
	ActionAdminUserSetRole       Action = "admin.user.set_role"

	// Team administration.
	ActionTeamCreate       Action = "team.create"
	ActionTeamRename       Action = "team.rename"
	ActionTeamDelete       Action = "team.delete"
	ActionTeamMemberAdd    Action = "team.member.add"
	ActionTeamMemberRemove Action = "team.member.remove"
	ActionTeamMemberRole   Action = "team.member.set_role"

	// Provider credential administration (the secret is never recorded).
	ActionTeamKeySet    Action = "team.key.set"
	ActionTeamKeyDelete Action = "team.key.delete"

	// Provider registry administration (change provider-registry). The secret
	// itself is never recorded, only that a key was written or rotated.
	ActionProviderCreate      Action = "provider.create"
	ActionProviderUpdate      Action = "provider.update"
	ActionProviderDelete      Action = "provider.delete"
	ActionProviderSetDefault  Action = "provider.set_default"
	ActionProviderModelCreate Action = "provider.model.create"
	ActionProviderModelUpdate Action = "provider.model.update"
	ActionProviderModelDelete Action = "provider.model.delete"
	ActionProviderModelDefault Action = "provider.model.set_default"
	ActionTeamProviderAssign      Action = "team.provider.assign"
	ActionTeamProviderAssignClear Action = "team.provider.assign.clear"

	// Usage-budget administration (the quota that caps monthly spend).
	ActionQuotaSet   Action = "quota.set"
	ActionQuotaClear Action = "quota.clear"

	// Memory administration.
	ActionMemoryDelete    Action = "memory.delete"
	ActionMemoryDeprecate Action = "memory.deprecate"

	// Service-key administration (the token itself is never recorded).
	ActionServiceKeyCreate Action = "service_key.create"
	ActionServiceKeyRevoke Action = "service_key.revoke"
)

// Event is one auditable occurrence, built fluently and handed to Logger.Log.
// The zero value is unusable; start from audit.Event(action, outcome).
type Event struct {
	action  Action
	outcome Outcome

	actorID    string
	actorEmail string
	targetType string
	targetID   string
	ip         string
	ua         string
	detail     map[string]any
}

// New starts an event for an action and its outcome.
func New(action Action, outcome Outcome) Event {
	return Event{action: action, outcome: outcome}
}

// Success starts a successful event for an action.
func Success(action Action) Event { return New(action, OutcomeSuccess) }

// Failure starts a failed event for an action.
func Failure(action Action) Event { return New(action, OutcomeFailure) }

// Actor records who performed the action. id may be empty (anonymous).
func (e Event) Actor(id, email string) Event { e.actorID, e.actorEmail = id, email; return e }

// Target records the entity acted upon.
func (e Event) Target(typ, id string) Event { e.targetType, e.targetID = typ, id; return e }

// Client records the connection metadata. Prefer FromRequest, which fills both.
func (e Event) Client(ip, ua string) Event { e.ip, e.ua = ip, ua; return e }

// FromRequest fills actor connection metadata (IP, User-Agent) from the
// request. It does NOT resolve the actor — call sites supply identity explicitly
// so the trail is correct even where the actor is not the authenticated user
// (e.g. a failed login, where there is no session yet).
func (e Event) FromRequest(r *http.Request) Event {
	if r == nil {
		return e
	}
	e.ip = ClientIP(r)
	e.ua = r.UserAgent()
	return e
}

// Detail attaches a small action-specific payload. Keep it thin and NEVER put
// secrets, tokens, or passwords here — detail is persisted verbatim.
func (e Event) Detail(kv map[string]any) Event {
	if e.detail == nil {
		e.detail = map[string]any{}
	}
	for k, v := range kv {
		e.detail[k] = v
	}
	return e
}

// ClientIP extracts the best-effort client IP. It honours X-Forwarded-For (the
// platform sits behind a reverse proxy / gateway in a typical internal deploy)
// and falls back to the peer address. Only the first forwarded hop is trusted —
// the client can append arbitrary later hops, so the proxy-supplied origin is
// the leftmost entry.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Entry is a persisted audit row as the query API returns it.
type Entry struct {
	ID         int64           `json:"id"`
	CreatedAt  time.Time       `json:"created_at"`
	ActorID    string          `json:"actor_id,omitempty"`
	ActorEmail string          `json:"actor_email,omitempty"`
	Action     string          `json:"action"`
	Outcome    string          `json:"outcome"`
	TargetType string          `json:"target_type,omitempty"`
	TargetID   string          `json:"target_id,omitempty"`
	IP         string          `json:"ip,omitempty"`
	UA         string          `json:"ua,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
}

// Filter narrows a List query. Zero values are unbounded.
type Filter struct {
	Action string    // exact action match; empty = all
	Actor  string    // actor_id exact match; empty = all
	From   time.Time // created_at >= From; zero = unbounded
	To     time.Time // created_at <= To; zero = unbounded
	Limit  int       // max rows; <=0 uses DefaultLimit
	Offset int       // pagination offset
}

// DefaultLimit bounds an unfiltered query so a careless call cannot pull the
// whole trail into memory.
const DefaultLimit = 100

// MaxLimit caps any caller-supplied limit.
const MaxLimit = 500

// Logger records audit events to Postgres. It is safe for concurrent use.
type Logger struct {
	db *sql.DB
	// log receives write-failure reports; may be nil (falls back to slog.Default).
	log *slog.Logger
}

// NewLogger builds a Logger over db. log may be nil.
func NewLogger(db *sql.DB, log *slog.Logger) *Logger {
	if log == nil {
		log = slog.Default()
	}
	return &Logger{db: db, log: log}
}

// Log persists e. It returns the insert error; callers that must not fail on an
// audit hiccup should use LogAndReport instead.
func (l *Logger) Log(ctx context.Context, e Event) error {
	detail, err := json.Marshal(e.detail)
	if err != nil {
		detail = []byte("{}")
	}
	_, err = l.db.ExecContext(ctx, `
		INSERT INTO audit_log
			(actor_id, actor_email, action, outcome, target_type, target_id, ip, ua, detail)
		VALUES
			(NULLIF($1,''), NULLIF($2,''), $3, $4, NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), NULLIF($8,''), $9)`,
		e.actorID, e.actorEmail, string(e.action), string(e.outcome),
		e.targetType, e.targetID, e.ip, e.ua, detail)
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	return nil
}

// LogAndReport persists e and, on failure, logs the error rather than returning
// it. This is the standard call pattern: the audited action has already
// succeeded or failed on its own merits, and a broken audit sink must not
// change that outcome — only surface that the trail has a gap.
func (l *Logger) LogAndReport(ctx context.Context, e Event) {
	if err := l.Log(ctx, e); err != nil {
		l.log.Error("audit write failed; trail has a gap", "action", string(e.action), "err", err)
	}
}

// List returns audit entries matching f, newest first, plus the total count for
// pagination. It is the read side for the platform-admin console.
func (l *Logger) List(ctx context.Context, f Filter) ([]Entry, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	where := "WHERE 1=1"
	args := []any{}
	add := func(clause string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND "+clause, len(args))
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.Actor != "" {
		add("actor_id = $%d", f.Actor)
	}
	if !f.From.IsZero() {
		add("created_at >= $%d", f.From)
	}
	if !f.To.IsZero() {
		add("created_at <= $%d", f.To)
	}

	var total int
	if err := l.db.QueryRowContext(ctx,
		"SELECT count(*) FROM audit_log "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("audit count: %w", err)
	}

	query := `
		SELECT id, created_at, COALESCE(actor_id,''), COALESCE(actor_email,''),
		       action, outcome, COALESCE(target_type,''), COALESCE(target_id,''),
		       COALESCE(ip,''), COALESCE(ua,''), detail
		FROM audit_log ` + where + `
		ORDER BY id DESC
		LIMIT $` + fmt.Sprint(len(args)+1) + ` OFFSET $` + fmt.Sprint(len(args)+2)
	rows, err := l.db.QueryContext(ctx, query, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("audit list: %w", err)
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.ActorID, &e.ActorEmail,
			&e.Action, &e.Outcome, &e.TargetType, &e.TargetID, &e.IP, &e.UA, &e.Detail); err != nil {
			return nil, 0, fmt.Errorf("audit scan: %w", err)
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}
