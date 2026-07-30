// Client-side tools (general interrupt, client half). The browser owns a set of
// capabilities the server cannot perform — reading the clipboard, the user's
// timezone, geolocation — and declares them to the agent in the chat request
// body's `tools` field. When the model calls one, the backend suspends the run
// and streams a client_tool interaction; approval.ts auto-runs the matching
// capability here and POSTs the output, which the server validates against the
// declared outputSchema before folding it as the tool result.
//
// To add a capability: add an entry to CLIENT_TOOLS. The name is the tool name
// the model sees; description + inputSchema are its calling contract; run()
// executes it in the browser and returns {output} (validated) or {error} (folded
// as an is_error result the model can react to). Never shadow a built-in server
// tool name — the server skips client declarations that collide with built-ins.

// A ClientToolResult is what a capability returns: an output value (validated
// server-side against outputSchema) or an error string (folded as is_error).
export type ClientToolResult = { output?: unknown; error?: string };

// A ClientTool is one browser capability: the contract shown to the model plus
// the function that executes it locally.
export type ClientTool = {
  description: string;
  // inputSchema is the JSON Schema of the call arguments (shown to the model).
  inputSchema: Record<string, unknown>;
  // outputSchema, when set, is the JSON Schema the returned output must satisfy;
  // the server validates against it before folding (declare + validate trust).
  outputSchema?: Record<string, unknown>;
  run: (args: unknown) => Promise<ClientToolResult>;
};

// CLIENT_TOOLS is the registry of browser capabilities offered to the agent.
export const CLIENT_TOOLS: Record<string, ClientTool> = {
  get_clipboard: {
    description:
      "Read the text currently on the user's system clipboard. Use when the user refers to something they copied.",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
    outputSchema: {
      type: "object",
      properties: { text: { type: "string" } },
      required: ["text"],
    },
    run: async () => {
      if (!navigator.clipboard?.readText) {
        return { error: "clipboard API is unavailable in this browser" };
      }
      const text = await navigator.clipboard.readText();
      return { output: { text } };
    },
  },

  get_timezone: {
    description:
      "Get the user's local timezone and current local time from the browser. Use to answer time/date questions relative to where the user is.",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
    outputSchema: {
      type: "object",
      properties: {
        timezone: { type: "string" },
        localTime: { type: "string" },
      },
      required: ["timezone", "localTime"],
    },
    run: async () => {
      const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
      const localTime = new Date().toString();
      return { output: { timezone, localTime } };
    },
  },

  sleep: {
    description:
      "Sleep for the given number of seconds in the user's browser, then return. Useful for testing the client-tool round-trip: the run suspends while the browser waits and resumes when the sleep finishes.",
    inputSchema: {
      type: "object",
      properties: {
        seconds: { type: "number", description: "How many seconds to sleep (capped at 60)." },
      },
      required: ["seconds"],
      additionalProperties: false,
    },
    outputSchema: {
      type: "object",
      properties: { sleptSeconds: { type: "number" } },
      required: ["sleptSeconds"],
    },
    run: async (args) => {
      const seconds = Number((args as { seconds?: unknown })?.seconds);
      if (!Number.isFinite(seconds) || seconds < 0) {
        return { error: "seconds must be a non-negative number" };
      }
      // Cap the wait so a runaway value can't hang the browser tab; report the
      // duration actually slept so the model sees the truncation.
      const capped = Math.min(seconds, 60);
      await new Promise<void>((resolve) => {
        setTimeout(resolve, capped * 1000);
      });
      return { output: { sleptSeconds: capped } };
    },
  },
};

// clientToolDeclarations returns the `tools` object to merge into the chat
// request body: each capability's model-facing contract (description +
// inputSchema + outputSchema). The run() functions stay in the browser.
export function clientToolDeclarations(): Record<
  string,
  { description: string; inputSchema: Record<string, unknown>; outputSchema?: Record<string, unknown> }
> {
  const out: Record<
    string,
    { description: string; inputSchema: Record<string, unknown>; outputSchema?: Record<string, unknown> }
  > = {};
  for (const [name, t] of Object.entries(CLIENT_TOOLS)) {
    out[name] = {
      description: t.description,
      inputSchema: t.inputSchema,
      ...(t.outputSchema ? { outputSchema: t.outputSchema } : {}),
    };
  }
  return out;
}

// runClientTool executes a declared capability by name and returns its result.
// An unknown tool or a thrown error folds as an is_error tool result, so the
// model self-corrects rather than the run stalling.
export async function runClientTool(name: string, args: unknown): Promise<ClientToolResult> {
  const tool = CLIENT_TOOLS[name];
  if (!tool) {
    return { error: `unknown client tool ${JSON.stringify(name)}` };
  }
  try {
    return await tool.run(args);
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    return { error: `${name} failed: ${msg}` };
  }
}
