## 1. Durable records

- [ ] 1.1 Write migration `0000xx_subagent_runs` (up/down): table per design D1 with unique (session_id, spawn_call_id); verify migrate applies both ways
- [ ] 1.2 Add `subagent.Recorder` interface + no-op default; PG implementation (`internal/subagent/recorder_pg.go`) with `Start`/`Finish`/`FindCompleted`
- [ ] 1.3 Recorder PG tests on dev Postgres (unique random session ids, delete only created rows): lifecycle transitions, unique-key upsert semantics, usage persistence

## 2. Spawn integration

- [ ] 2.1 Wire Recorder into `SpawnTool`: record start before the child runs and finish (status/outcome/result/usage) on every exit path incl. depth/budget/cancel/gated/error
- [ ] 2.2 Outcome codes: emit `outcome: <code>` as first content line on error results; `completed` recorded on the row only; map every existing failure mode to its code
- [ ] 2.3 Idempotent replay: before starting a child, `FindCompleted(session, callID)`; matching prompt+type returns the stored result without running a loop or charging budget; mismatch logs and runs fresh
- [ ] 2.4 Unit tests: each outcome code on its path; replay hit (no provider call), non-completed re-issue runs fresh, mismatch runs fresh; recorder failure doesn't fail the spawn

## 3. Budgets and throttling

- [ ] 3.1 Config: `SUBAGENT_TYPE_BUDGETS` parsing in `internal/config` (per-type total/concurrent), plumbed to the spawn tool in `main.go`
- [ ] 3.2 Per-type budget enforcement in `SpawnTool` (semaphore wait with ctx cancel, total counter), outcome `budget_exhausted` naming type and cap; tests for cap, queue, cancel-while-waiting, unconfigured type
- [ ] 3.3 Activity coalescing in `activityEmitter`: per-child buffer, timer flush (configurable window), lifecycle signals flush-then-send immediately, terminal flush; tests for merge ordering, no content loss, immediate lifecycle signals

## 4. Inspection API + panel

- [ ] 4.1 `GET /api/sessions/{id}/subagent-runs[/{subID}]` behind `RequireAuth` with own-session-or-platform-admin authorization (non-owner opacity); tests for owner/admin/non-owner
- [ ] 4.2 Web: typed client + subagent card drill-down (status, outcome, usage, timing, collapsed result) fetched on expand
- [ ] 4.3 `pnpm exec tsc -b`, `pnpm lint`, `pnpm build` clean

## 5. Verification

- [ ] 5.1 `go build ./... && go vet ./... && go test ./...` green
- [ ] 5.2 `openspec validate first-class-subagent-runs` passes
- [ ] 5.3 Manual smoke: run a chat with parallel subagents (records appear, panel drill-down works), interrupt and resume the parent (completed spawn replays without re-execution), exceed a per-type budget (error names type and cap)
