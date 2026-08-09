// The two standalone agent-definition pages, one per non-team scope tier.
// Team definitions live as a tab inside TeamDetailPage instead of their own
// page (same layout as skills).
//   MyAgentDefsPage       — the signed-in user's own agent definitions
//   PlatformAgentDefsPage — system (global) agent definitions

import { PageHeader } from "@/components/admin/common";
import { AgentDefEditor } from "@/components/admin/AgentDefEditor";

export function MyAgentDefsPage() {
  return (
    <>
      <PageHeader
        title="My agents"
        description="Your own subagent types for spawn_agent: a markdown document whose frontmatter scopes tools, model, and skills, and whose body is the child's system prompt. A same-named definition overrides team and system ones for you."
      />
      <AgentDefEditor base={{ kind: "me" }} canWrite />
    </>
  );
}

export function PlatformAgentDefsPage() {
  return (
    <>
      <PageHeader
        title="Platform agents"
        description="Global subagent types available to every account, the lowest-priority scope above the built-ins. A user or team definition of the same name overrides the platform one."
      />
      <AgentDefEditor base={{ kind: "platform" }} canWrite />
    </>
  );
}
