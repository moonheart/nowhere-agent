// The two standalone skill pages, one per non-team scope tier. Team skills live
// as a tab inside TeamDetailPage instead of their own page. Each frame supplies
// the scope (whose skills these are); both are writable by whoever can reach them
// (any account for its own, a platform admin for the platform tier).
//   MySkillsPage       — the signed-in user's own skills
//   PlatformSkillsPage — system (global) skills

import { PageHeader } from "@/components/admin/common";
import { SkillEditor } from "@/components/admin/SkillEditor";

export function MySkillsPage() {
  return (
    <>
      <PageHeader
        title="My skills"
        description="Reusable skill packs for the agent: a SKILL.md plus resource and script files. Saving writes a new version; scripts run in the sandbox when the skill is loaded."
      />
      <SkillEditor base={{ kind: "me" }} canWrite />
    </>
  );
}

export function PlatformSkillsPage() {
  return (
    <>
      <PageHeader
        title="Platform skills"
        description="Global skills available to every account, the lowest-priority scope. A user or team skill of the same name overrides the platform one; editing here flags those overrides for review."
      />
      <SkillEditor base={{ kind: "platform" }} canWrite />
    </>
  );
}
