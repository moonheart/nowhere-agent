package session

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"nowhere-agent/internal/provider"
)

// MemStore is an in-memory Store for tests and early development.
type MemStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	runs     map[string]*Run    // runID
	bySess   map[string][]*Run  // sessionID -> runs
	events   map[string][]Event // runID -> events
	// dreamedSeq is each session's dreaming watermark (the in-memory analogue of
	// sessions.dreamed_seq, migration 000009): the messages.id consolidated up to.
	dreamedSeq map[string]int64
	// memoryInjectedAt is each session's memory-injection watermark (the
	// in-memory analogue of sessions.memory_injected_at, migration 000012).
	memoryInjectedAt map[string]time.Time
	// approvals is the in-memory analogue of the approvals table (migration
	// 000010): approvalID -> pending/decided tool-approval record.
	approvals map[string]*Approval
	// approvalSeq is a monotonic insertion counter giving interactions a stable
	// queue order independent of wall-clock timestamp ties (CreatedAt can collide
	// for a batch created in the same tick). Stamped on each new Approval.
	approvalSeq int64
	// batches is the in-memory analogue of the suspended_batches table
	// (migration 000019): runID -> suspended-batch snapshot.
	batches map[string]*SuspendedBatch
	// steps is the in-memory analogue of the run_steps table (migration
	// 000041): runID -> intent rows in per-run seq order.
	steps map[string][]RunStep
	// usageRecords is the in-memory analogue of the usage_records table
	// (migration 000041): runID -> ledger rows in insertion order.
	usageRecords map[string][]UsageRecord
}

// NewMemStore creates an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{
		sessions:         map[string]*Session{},
		runs:             map[string]*Run{},
		bySess:           map[string][]*Run{},
		events:           map[string][]Event{},
		dreamedSeq:       map[string]int64{},
		memoryInjectedAt: map[string]time.Time{},
		approvals:        map[string]*Approval{},
		batches:          map[string]*SuspendedBatch{},
		steps:            map[string][]RunStep{},
		usageRecords:     map[string][]UsageRecord{},
	}
}

func (m *MemStore) CreateSession(_ context.Context, userID, title string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Session{ID: uuid.NewString(), UserID: userID, Title: title, Status: SessionActive, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	m.sessions[s.ID] = s
	return *s, nil
}

func (m *MemStore) GetSession(_ context.Context, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, errors.New("session not found")
	}
	return *s, nil
}

func (m *MemStore) EndSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.Status = SessionEnded
	}
	return nil
}

func (m *MemStore) ListIdleSessions(_ context.Context, before time.Time) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.Status == SessionActive && s.UpdatedAt.Before(before) {
			out = append(out, *s)
		}
	}
	return out, nil
}

// ListEndedSessionsEndedBefore returns ids of ended sessions whose end time
// (UpdatedAt — MemStore stamps no dedicated ended_at) predates before, oldest
// first, capped at limit.
func (m *MemStore) ListEndedSessionsEndedBefore(_ context.Context, before time.Time, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, s := range m.sessions {
		if s.Status == SessionEnded && s.UpdatedAt.Before(before) {
			ids = append(ids, s.ID)
		}
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// ListUndreamedSessions is not supported by MemStore: eligibility needs a join
// against the message store (messages beyond the watermark), which the session
// MemStore cannot see. The production store (PGStore) answers it in SQL; tests
// drive dreaming with a fake EpisodeSource instead.
func (m *MemStore) ListUndreamedSessions(_ context.Context) ([]Session, error) {
	return nil, errors.New("MemStore.ListUndreamedSessions: not supported (needs the messages join; use PGStore)")
}

// ListUndreamedSessionsForUser is likewise unsupported, for the same reason.
func (m *MemStore) ListUndreamedSessionsForUser(_ context.Context, _ string) ([]Session, error) {
	return nil, errors.New("MemStore.ListUndreamedSessionsForUser: not supported (needs the messages join; use PGStore)")
}

func (m *MemStore) DreamedSeq(_ context.Context, id string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dreamedSeq[id], nil
}

func (m *MemStore) MarkDreamedSeq(_ context.Context, id string, seq int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if seq > m.dreamedSeq[id] {
		m.dreamedSeq[id] = seq
	}
	return nil
}

func (m *MemStore) MemoryInjectedAt(_ context.Context, id string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.memoryInjectedAt[id], nil
}

func (m *MemStore) MarkMemoryInjectedAt(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if at.After(m.memoryInjectedAt[id]) {
		m.memoryInjectedAt[id] = at
	}
	return nil
}

// SetSessionStateKV upserts one key in the session's in-memory state dictionary,
// preserving sibling keys (the in-memory analogue of the PG jsonb_set). The value
// is deep-copied so a caller mutating its buffer can't corrupt the stored state.
func (m *MemStore) SetSessionStateKV(_ context.Context, id, key string, value json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return errors.New("session not found")
	}
	if s.State == nil {
		s.State = map[string]json.RawMessage{}
	}
	cp := make(json.RawMessage, len(value))
	copy(cp, value)
	s.State[key] = cp
	return nil
}

// SessionStateKV returns the JSON value stored under key, or false if unset.
func (m *MemStore) SessionStateKV(_ context.Context, id, key string) (json.RawMessage, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, false, errors.New("session not found")
	}
	v, ok := s.State[key]
	if !ok {
		return nil, false, nil
	}
	cp := make(json.RawMessage, len(v))
	copy(cp, v)
	return cp, true, nil
}

// SessionState returns the session's whole state dictionary (deep-copied).
func (m *MemStore) SessionState(_ context.Context, id string) (map[string]json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, errors.New("session not found")
	}
	out := make(map[string]json.RawMessage, len(s.State))
	for k, v := range s.State {
		cp := make(json.RawMessage, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out, nil
}

func (m *MemStore) ListSessionsByUser(_ context.Context, userID, q string, limit int, cursor *SessionCursor) (SessionPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 25
	}
	needle := strings.ToLower(q)
	var out []Session
	for _, s := range m.sessions {
		if s.UserID == userID && s.Status == SessionActive && (needle == "" || strings.Contains(strings.ToLower(s.Title), needle)) {
			out = append(out, *s)
		}
	}
	// Most-recently-active first; id breaks ties. WALL-CLOCK only (UnixNano):
	// the sort and the keyset filter must share one basis. A cursor round-trips
	// through JSON (RFC3339Nano), which strips the monotonic reading, so a
	// monotonic-based sort could disagree with the wall-based filter when the
	// host clock is adjusted mid-run (NTP/VM time sync on CI) — a row would
	// straddle the page boundary and appear on BOTH pages. PG compares
	// timestamptz (wall), so this mirrors it exactly.
	sort.Slice(out, func(i, j int) bool {
		ui, uj := out[i].UpdatedAt.UnixNano(), out[j].UpdatedAt.UnixNano()
		if ui != uj {
			return ui > uj
		}
		return out[i].ID > out[j].ID
	})
	// Keyset: keep only rows strictly below the cursor in that ordering.
	if cursor != nil {
		cu := cursor.UpdatedAt.UnixNano()
		kept := out[:0]
		for _, s := range out {
			if su := s.UpdatedAt.UnixNano(); su < cu || (su == cu && s.ID < cursor.ID) {
				kept = append(kept, s)
			}
		}
		out = kept
	}
	page := SessionPage{}
	if len(out) > limit {
		last := out[limit-1]
		page.Sessions = out[:limit]
		page.NextCursor = &SessionCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
	} else {
		page.Sessions = out
	}
	return page, nil
}

func (m *MemStore) DeleteSessionForUser(_ context.Context, id, userID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.UserID != userID {
		return false, nil
	}
	s.Status = SessionEnded
	return true, nil
}

// DeleteSession hard-deletes a session from the in-memory store.
func (m *MemStore) DeleteSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; !ok {
		return ErrSessionNotFound
	}
	delete(m.sessions, id)
	return nil
}

// SessionIDsForUser returns every session id the user owns.
func (m *MemStore) SessionIDsForUser(_ context.Context, userID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, s := range m.sessions {
		if s.UserID == userID {
			out = append(out, s.ID)
		}
	}
	return out, nil
}

func (m *MemStore) CreateRun(_ context.Context, sessionID string, seq int) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &Run{ID: uuid.NewString(), SessionID: sessionID, Seq: seq, Status: RunQueued, CreatedAt: time.Now()}
	m.runs[r.ID] = r
	m.bySess[sessionID] = append(m.bySess[sessionID], r)
	return *r, nil
}

func (m *MemStore) UpdateRunStatus(_ context.Context, runID string, status RunStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok {
		r.Status = status
	}
	return nil
}

// SetRunUsage records the run's aggregate token usage. u is nil-safe (a no-op).
func (m *MemStore) SetRunAttribution(_ context.Context, runID, teamID, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok {
		r.TeamID = teamID
		r.Model = model
	}
	return nil
}

// SetRunUsage records the run's aggregate token usage. u is nil-safe (a no-op).
func (m *MemStore) SetRunUsage(_ context.Context, runID string, u *provider.Usage) error {
	if u == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok {
		cp := *u
		r.Usage = &cp
	}
	return nil
}

func (m *MemStore) ActiveRun(_ context.Context, sessionID string) (Run, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.bySess[sessionID] {
		if r.Status.Active() {
			return *r, true, nil
		}
	}
	return Run{}, false, nil
}

func (m *MemStore) NextRunSeq(_ context.Context, sessionID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bySess[sessionID]) + 1, nil
}

// FailStrandedRuns marks every non-terminal run failed (startup reconciliation).
// Runs are stateless and terminal on completion, so any queued/running row
// belongs to a dead worker.
func (m *MemStore) FailStrandedRuns(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.runs {
		if r.Status == RunQueued || r.Status == RunRunning {
			r.Status = RunFailed
			n++
		}
	}
	return n, nil
}

// StrandedRuns returns every non-terminal run, newest first.
func (m *MemStore) StrandedRuns(_ context.Context) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Run
	for _, r := range m.runs {
		if r.Status == RunQueued || r.Status == RunRunning {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// AppendRunStep writes one step intent (the in-memory analogue of PGStore's
// nextval provisioning: ids are negative and decrementing so they can never
// collide with PG-assigned positive ids).
func (m *MemStore) AppendRunStep(_ context.Context, runID string, kind StepKind, toolCallID string, resultMessageID *int64) (RunStep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	steps := m.steps[runID]
	attempt := 1
	for _, s := range steps {
		if s.StepKind == kind {
			attempt++
		}
	}
	st := RunStep{
		ID:          int64(len(steps) + 1),
		RunID:       runID,
		Seq:         len(steps) + 1,
		StepKind:    kind,
		Attempt:     attempt,
		ToolCallID:  toolCallID,
		CreatedAt:   time.Now(),
	}
	if kind == StepAssistant || kind == StepTool {
		if resultMessageID != nil {
			st.ResultMessageID = resultMessageID
		} else {
			id := int64(-(len(m.usageRecords) + len(steps) + 1))
			st.ResultMessageID = &id
		}
	}
	m.steps[runID] = append(steps, st)
	return st, nil
}

// AppendUsageRecord appends one ledger row.
func (m *MemStore) AppendUsageRecord(_ context.Context, rec UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec.ID = int64(len(m.usageRecords[rec.RunID]) + 1)
	rec.CreatedAt = time.Now()
	m.usageRecords[rec.RunID] = append(m.usageRecords[rec.RunID], rec)
	return nil
}

// SumUsage returns the run's aggregate usage as the sum of its ledger rows.
func (m *MemStore) SumUsage(_ context.Context, runID string) (*provider.Usage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := &provider.Usage{}
	for _, r := range m.usageRecords[runID] {
		u.InputTokens += r.Usage.InputTokens
		u.OutputTokens += r.Usage.OutputTokens
		u.CacheReadTokens += r.Usage.CacheReadTokens
		u.CacheWriteTokens += r.Usage.CacheWriteTokens
	}
	return u, nil
}

// LatestRunSteps returns the run's newest intent rows (newest first),
// excluding overflow_compact records (completed recovery records, never
// pending effects). ResultExists is always false in memory: the mem message
// store keeps no id correlation, and recovery tests drive PGStore for the join.
func (m *MemStore) LatestRunSteps(_ context.Context, runID string, limit int) ([]RunStep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 1
	}
	steps := m.steps[runID]
	out := make([]RunStep, 0, limit)
	for i := len(steps) - 1; i >= 0 && len(out) < limit; i-- {
		if steps[i].StepKind == StepOverflowCompact {
			continue
		}
		out = append(out, steps[i])
	}
	return out, nil
}

func (m *MemStore) RunsForSession(_ context.Context, sessionID string) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Run, 0, len(m.bySess[sessionID]))
	for _, r := range m.bySess[sessionID] {
		out = append(out, *r)
	}
	return out, nil
}

func (m *MemStore) AppendEvent(_ context.Context, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.CreatedAt = time.Now()
	m.events[e.RunID] = append(m.events[e.RunID], e)
	return nil
}

func (m *MemStore) EventsAfter(_ context.Context, runID string, after int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.events[runID] {
		if e.Offset > after {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out, nil
}

// Sessions returns all sessions (test/inspection helper).
func (m *MemStore) Sessions() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, *s)
	}
	return out
}

// RunsFor returns all runs for a session (test/inspection helper).
func (m *MemStore) RunsFor(sessionID string) []Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Run, 0, len(m.bySess[sessionID]))
	for _, r := range m.bySess[sessionID] {
		out = append(out, *r)
	}
	return out
}

// EventsFor returns all events for a run (test/inspection helper).
func (m *MemStore) EventsFor(runID string) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events[runID]))
	copy(out, m.events[runID])
	return out
}
