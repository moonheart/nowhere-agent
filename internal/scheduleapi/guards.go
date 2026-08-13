package scheduleapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/schedule"
)

// ---- DTO ----

// taskDTO is the wire form of a scheduled task.
type taskDTO struct {
	ID               string         `json:"id"`
	AgentDefName     string         `json:"agent_def_name,omitempty"`
	Prompt           string         `json:"prompt,omitempty"`
	ToolWhitelist    []string       `json:"tool_whitelist"`
	Cron             string         `json:"cron"`
	Timezone         string         `json:"timezone"`
	TargetSessionID  string         `json:"target_session_id,omitempty"`
	OnRunCompleted   string         `json:"on_run_completed"`
	Multitask        string         `json:"multitask_strategy"`
	WebhookURL       string         `json:"webhook_url"`
	EndTime          *time.Time     `json:"end_time,omitempty"`
	Enabled          bool           `json:"enabled"`
	NextRunAt        time.Time      `json:"next_run_at"`
	LastRunAt        *time.Time     `json:"last_run_at,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

func taskDTOOf(t schedule.Task) taskDTO {
	return taskDTO{
		ID:              t.ID,
		AgentDefName:    t.AgentDefName,
		Prompt:          t.Prompt,
		ToolWhitelist:   t.ToolWhitelist,
		Cron:            t.Cron,
		Timezone:        t.Timezone,
		TargetSessionID: t.TargetSessionID,
		OnRunCompleted:  string(t.OnRunCompleted),
		Multitask:       string(t.Multitask),
		WebhookURL:      t.WebhookURL,
		EndTime:         t.EndTime,
		Enabled:         t.Enabled,
		NextRunAt:       t.NextRunAt,
		LastRunAt:       t.LastRunAt,
		Metadata:        t.Metadata,
		CreatedAt:       t.CreatedAt,
		UpdatedAt:       t.UpdatedAt,
	}
}

func taskDTOs(ts []schedule.Task) []taskDTO {
	out := make([]taskDTO, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskDTOOf(t))
	}
	return out
}

// taskRequest is the create/update payload. The owner and team come from the
// authenticated caller and route, never the body.
type taskRequest struct {
	AgentDefName    string         `json:"agent_def_name"`
	Prompt          string         `json:"prompt"`
	ToolWhitelist   []string       `json:"tool_whitelist"`
	Cron            string         `json:"cron"`
	Timezone        string         `json:"timezone"`
	TargetSessionID string         `json:"target_session_id"`
	OnRunCompleted  string         `json:"on_run_completed"`
	Multitask       string         `json:"multitask_strategy"`
	WebhookURL      string         `json:"webhook_url"`
	EndTime         *time.Time     `json:"end_time"`
	Metadata        map[string]any `json:"metadata"`
}

// toTask maps the request onto a Task owned by userID, validating the schedule
// and enums (the store re-validates on write; this surfaces the error as 400).
func (req taskRequest) toTask(userID string) (schedule.Task, error) {
	t := schedule.Task{
		UserID:          userID,
		AgentDefName:    req.AgentDefName,
		Prompt:          req.Prompt,
		ToolWhitelist:   req.ToolWhitelist,
		Cron:            req.Cron,
		Timezone:        req.Timezone,
		TargetSessionID: req.TargetSessionID,
		OnRunCompleted:  schedule.OnRunCompleted(req.OnRunCompleted),
		Multitask:       schedule.MultitaskStrategy(req.Multitask),
		WebhookURL:      req.WebhookURL,
		EndTime:         req.EndTime,
		Enabled:         true,
		Metadata:        req.Metadata,
	}
	// Defaults for the enum fields, applied before validation so an omitted
	// field means the default rather than a validation error.
	if t.OnRunCompleted == "" {
		t.OnRunCompleted = schedule.OnRunKeep
	}
	if t.Multitask == "" {
		t.Multitask = schedule.MultitaskReject
	}
	if err := t.Validate(); err != nil {
		return schedule.Task{}, err
	}
	return t, nil
}

// ---- plumbing ----

// maxBodyBytes bounds a task-management request body (task prompts and
// metadata) at 1 MiB; larger bodies are rejected with 413 before decoding.
const maxBodyBytes = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return false
	}
	if len(body) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpx.JSON(w, status, v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	httpx.Error(w, status, msg)
}

// writeStoreError maps a store error onto a status code, so a not-found never
// leaks as a 500 and an invalid task surfaces as a 400.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, schedule.ErrNotFound):
		writeError(w, http.StatusNotFound, "task not found")
	case errors.Is(err, schedule.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "request failed")
	}
}

// storeUnavailable answers 503 when the handler has no store wired.
func (h *Handler) storeUnavailable(w http.ResponseWriter) bool {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "schedule store unavailable")
		return true
	}
	return false
}

// authorizeTask resolves a task and verifies the caller owns it, answering the
// response itself when not. Another owner's task is not-found, never forbidden,
// so ids cannot be probed.
func (h *Handler) authorizeTask(w http.ResponseWriter, r *http.Request, id string) (schedule.Task, bool) {
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id required")
		return schedule.Task{}, false
	}
	t, err := h.store.Get(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return schedule.Task{}, false
	}
	if t.UserID != caller(r).ID {
		writeError(w, http.StatusNotFound, "task not found")
		return schedule.Task{}, false
	}
	return t, true
}
