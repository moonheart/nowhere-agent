// Plan-store unit tests: pure logic only (no DOM). planFromSessionState /
// planFromMetadata parse server echoes; reportPlan / resetPlan drive the
// module-level store that usePlan reads through useSyncExternalStore. The
// store reads are hook-only, so coverage stops at the parsers and the
// non-DOM state transitions (reportPlan/resetPlan no-throw contract).

import { describe, expect, it } from "vitest";
import {
  planFromMetadata,
  planFromSessionState,
  reportPlan,
  resetPlan,
} from "@/lib/plan";

const plan = {
  items: [
    { content: "first", status: "pending" as const },
    { content: "done", status: "completed" as const },
  ],
};

describe("planFromSessionState", () => {
  it("extracts the plan for the plan key", () => {
    expect(planFromSessionState({ key: "plan", value: plan })).toEqual(plan);
  });

  it("returns null for other keys", () => {
    expect(planFromSessionState({ key: "permission_mode", value: plan })).toBeNull();
  });

  it("returns null for malformed values", () => {
    expect(planFromSessionState({ key: "plan", value: { items: "nope" } })).toBeNull();
    expect(planFromSessionState({ key: "plan", value: null })).toBeNull();
    expect(planFromSessionState(undefined)).toBeNull();
  });
});

describe("planFromMetadata", () => {
  it("finds the newest session-state plan in unstable_data", () => {
    const metadata = {
      unstable_data: [
        { name: "other", data: 1 },
        { name: "session-state", data: { key: "plan", value: plan } },
      ],
    };
    expect(planFromMetadata(metadata)).toEqual(plan);
  });

  it("returns null when no session-state frame carries a plan", () => {
    const metadata = {
      unstable_data: [{ name: "session-state", data: { key: "other", value: plan } }],
    };
    expect(planFromMetadata(metadata)).toBeNull();
  });

  it("returns null for non-array metadata", () => {
    expect(planFromMetadata({})).toBeNull();
    expect(planFromMetadata(undefined)).toBeNull();
  });
});

describe("reportPlan / resetPlan", () => {
  it("accepts a valid plan and ignores empty reports", () => {
    reportPlan(plan);
    reportPlan(null);
    reportPlan(undefined);
    reportPlan({ items: [] });
  });

  it("clears without error after a report", () => {
    reportPlan(plan);
    resetPlan();
    resetPlan();
  });
});
