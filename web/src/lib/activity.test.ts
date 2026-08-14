// activity feed unit tests, DOM-free: the epoch guard, the tool-call upsert,
// and the subagent signal state machine (start/stream/tool/result/done/error/
// interrupted, toolCallId matching, depth fallback, run cap).

import { beforeEach, describe, expect, it } from "vitest";
import {
  activityEpoch,
  getActivity,
  reportSubagentActivity,
  reportToolCall,
  resetActivity,
  type SubagentSignal,
} from "@/lib/activity";

beforeEach(() => {
  resetActivity();
});

describe("epoch guard", () => {
  it("drops tool reports tagged with a stale epoch", () => {
    reportToolCall({ id: "t1", toolName: "read_file", argsText: "", status: "running" }, activityEpoch() - 1);
    expect(getActivity().tools).toHaveLength(0);
  });

  it("drops subagent signals from a stale epoch", () => {
    reportSubagentActivity(
      { agentType: "researcher", depth: 1, phase: "start" },
      activityEpoch() - 1,
    );
    expect(getActivity().subagents).toHaveLength(0);
  });

  it("resetActivity clears state and invalidates in-flight reports", () => {
    reportToolCall({ id: "t1", toolName: "grep", argsText: "", status: "running" });
    const epoch = activityEpoch();
    resetActivity();
    expect(activityEpoch()).toBe(epoch + 1);
    expect(getActivity().tools).toHaveLength(0);
    reportToolCall({ id: "t1", toolName: "grep", argsText: "", status: "running" });
    expect(getActivity().tools).toHaveLength(1);
  });
});

describe("reportToolCall", () => {
  it("appends a new call and updates it in place by id", () => {
    reportToolCall({ id: "t1", toolName: "read_file", argsText: "a", status: "running" });
    reportToolCall({ id: "t2", toolName: "grep", argsText: "b", status: "running" });
    reportToolCall({ id: "t1", toolName: "read_file", argsText: "a", result: "ok", status: "done" });
    const tools = getActivity().tools;
    expect(tools).toHaveLength(2);
    expect(tools[0].status).toBe("done");
    expect(tools[0].result).toBe("ok");
    expect(tools[1].status).toBe("running");
  });

  it("ids default to toolName:at when no id is given", () => {
    reportToolCall({ id: "", toolName: "plan_write", argsText: "", at: 42, status: "running" });
    expect(getActivity().tools[0].id).toBe("plan_write:42");
  });
});

describe("reportSubagentActivity", () => {
  const start: SubagentSignal = { agentType: "researcher", depth: 1, phase: "start" };

  it("streams text into the trailing part and merges same-kind deltas", () => {
    reportSubagentActivity(start);
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "stream", kind: "text", text: "hello " });
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "stream", kind: "text", text: "world" });
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "stream", kind: "thinking", text: "hmm" });
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "done" });
    const run = getActivity().subagents[0];
    expect(run.status).toBe("done");
    expect(run.parts).toEqual([
      { kind: "text", text: "hello world" },
      { kind: "thinking", text: "hmm" },
    ]);
  });

  it("tracks tool parts and fills their result by subToolCallId", () => {
    reportSubagentActivity(start);
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "tool", tool: "grep", subToolCallId: "tc1" });
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "result", subToolCallId: "tc1", result: { n: 2 } });
    const run = getActivity().subagents[0];
    expect(run.tools).toEqual(["grep"]);
    expect(run.parts[0]).toMatchObject({ kind: "tool", toolName: "grep", status: "done", result: { n: 2 } });
  });

  it("marks an interrupted run error with the blocking gate text", () => {
    reportSubagentActivity(start);
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "interrupted", tool: "ask_user", kind: "approval" });
    const run = getActivity().subagents[0];
    expect(run.status).toBe("error");
    const last = run.parts[run.parts.length - 1];
    expect(last.kind === "text" && last.text.includes("ask_user")).toBe(true);
  });

  it("matches parallel runs by toolCallId and falls back to most recent at depth", () => {
    reportSubagentActivity(start);
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "start", toolCallId: "sp1" });
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "stream", toolCallId: "sp1", text: "x" });
    // A signal without toolCallId targets the MOST RECENT running run at the
    // depth — the sp1 run, not the first one.
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "stream", text: "first" });
    const runs = getActivity().subagents;
    expect(runs).toHaveLength(2);
    expect(runs[0].parts).toHaveLength(0); // first run untouched
    expect(runs[1].parts[0]).toMatchObject({ kind: "text", text: "xfirst" });
    reportSubagentActivity({ agentType: "researcher", depth: 1, phase: "result", subToolCallId: "tc-none" });
    expect(getActivity().subagents).toHaveLength(2); // no stray append
  });

  it("caps the run list at 50", () => {
    for (let i = 0; i < 60; i++) {
      reportSubagentActivity({ agentType: "r", depth: 1, phase: "start" });
      reportSubagentActivity({ agentType: "r", depth: 1, phase: "done" });
    }
    expect(getActivity().subagents).toHaveLength(50);
  });
});
