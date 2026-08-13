# STATUS: dropped / not implemented

This change was proposed (2026-08-09) but never implemented: the repository
contains no `subagent_runs` table, migration, or code path for first-class
subagent run records. The shipped subagent feature (change `2026-07-24-subagent`,
archived) renders child runs transiently via `spawn_agent` tool parts with
nested replay, without a dedicated runs table.

All 15 tasks in `tasks.md` are unimplemented. The proposal/design/specs in
this directory describe intent only and MUST NOT be treated as current
requirements. Either implement the change or delete this directory.
