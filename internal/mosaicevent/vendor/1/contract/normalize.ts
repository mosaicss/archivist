// Fixture replay only. These functions neither launch harnesses nor implement runtime adapters.
export type Chunk = Record<string, unknown>;
type Row = { direction: "in" | "out"; sourceIndex: number; frame: Chunk };
const record = (value: unknown): Chunk => {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("expected object");
  return value as Chunk;
};
const str = (value: unknown): string => {
  if (typeof value !== "string") throw new Error("expected string");
  return value;
};
const start = (id: string): Chunk => ({
  type: "start", messageId: id, messageMetadata: { schemaVersion: "mosaic-event/1" },
});

/** Only data-only UI SSE is supported; heartbeats are comments. Require one terminal sentinel. */
export function parseSSE(wire: string): Chunk[] {
  const chunks: Chunk[] = [];
  let done = false;
  const normalized = wire.replace(/\r\n/g, "\n");
  if (!normalized.endsWith("\n\n")) throw new Error("unterminated SSE frame");
  for (const frame of normalized.split("\n\n")) {
    const lines = frame.split("\n").filter((line) => line && !line.startsWith(":"));
    if (!lines.length) continue;
    if (done) throw new Error("frame after DONE");
    if (lines.some((line) => !line.startsWith("data:"))) throw new Error("non UI SSE field");
    const data = lines.map((line) => line.slice(5).replace(/^ /, "")).join("\n");
    if (data === "[DONE]") { done = true; continue; }
    chunks.push(record(JSON.parse(data)));
  }
  if (!done || !chunks.length) throw new Error("missing DONE or empty stream");
  return chunks;
}

export function normalizeSSE(wire: string, id: string): Chunk[] {
  const chunks = parseSSE(wire);
  const stamped = chunks.map((chunk) => chunk.type === "start"
    ? { ...chunk, messageMetadata: { ...record(chunk.messageMetadata ?? {}), schemaVersion: "mosaic-event/1" } }
    : chunk);
  if (!stamped.some((chunk) => chunk.type === "start")) stamped.unshift(start(id));
  if (!stamped.some((chunk) => chunk.type === "finish" || chunk.type === "abort")) {
    stamped.push({ type: "finish" });
  }
  return stamped;
}

export function normalizeClaude(input: unknown): Chunk[] {
  const rows = input as Row[];
  const chunks: Chunk[] = [];
  let turn = 0;
  let messageId = "";
  const streamed = new Set<string>();
  const blocks = new Map<number, { kind: string; id: string; name?: string; input: string; initial: unknown }>();
  const close = (index: number): void => {
    const block = blocks.get(index);
    if (!block) throw new Error("missing Claude content block");
    if (block.kind === "tool_use") chunks.push({ type: "tool-input-available", toolCallId: block.id,
      toolName: block.name, input: block.input ? JSON.parse(block.input) : block.initial });
    else chunks.push({ type: `${block.kind}-end`, id: block.id });
    blocks.delete(index);
  };
  for (const row of rows) {
    const f = record(row.frame);
    if (row.direction === "out") {
      if (f.type === "user") { turn++; chunks.push(start(`claude-turn-${turn}`)); }
      if (f.type === "control_response") {
        const response = record(f.response), body = record(response.response);
        if (body.behavior !== "allow" && body.behavior !== "deny") throw new Error("unknown Claude decision");
        chunks.push({ type: "tool-approval-response", approvalId: response.request_id,
          approved: body.behavior === "allow", ...(body.message ? { reason: body.message } : {}) });
      }
      continue;
    }
    switch (f.type) {
      case "stream_event": {
        const e = record(f.event), index = e.index as number;
        switch (e.type) {
          case "message_start": messageId = str(record(e.message).id); streamed.add(messageId); break;
          case "content_block_start": {
            const b = record(e.content_block), kind = b.type === "thinking" ? "reasoning" : str(b.type);
            if (!["text", "reasoning", "tool_use"].includes(kind)) throw new Error(`unsupported Claude block ${kind}`);
            const id = kind === "tool_use" ? str(b.id) : `${messageId}:${index}`;
            blocks.set(index, { kind, id, name: b.name as string | undefined, input: "", initial: b.input ?? {} });
            chunks.push(kind === "tool_use" ? { type: "tool-input-start", toolCallId: id, toolName: b.name }
              : { type: `${kind}-start`, id });
            const text = kind === "reasoning" ? b.thinking : b.text;
            if (typeof text === "string" && text) chunks.push({ type: `${kind}-delta`, id, delta: text });
            break;
          }
          case "content_block_delta": {
            const b = blocks.get(index), d = record(e.delta);
            if (!b) throw new Error("delta without Claude block");
            if (d.type === "input_json_delta") {
              const delta = str(d.partial_json); b.input += delta;
              chunks.push({ type: "tool-input-delta", toolCallId: b.id, inputTextDelta: delta });
            } else if (d.type === "text_delta" || d.type === "thinking_delta") {
              chunks.push({ type: `${b.kind}-delta`, id: b.id, delta: str(d.text ?? d.thinking) });
            } else if (d.type !== "signature_delta") throw new Error("unsupported Claude delta");
            break;
          }
          case "content_block_stop": close(index); break;
          case "message_delta": case "message_stop": break;
          default: throw new Error(`unsupported Claude stream event ${String(e.type)}`);
        }
        break;
      }
      case "assistant": {
        const m = record(f.message);
        // Snapshots repeat partial output. A snapshot without partials remains visible.
        if (!streamed.has(str(m.id))) {
          for (const [index, value] of (m.content as Chunk[]).entries()) {
            const b = record(value), id = `${str(m.id)}:${index}`;
            if (b.type === "text" || b.type === "thinking") {
              const kind = b.type === "text" ? "text" : "reasoning";
              chunks.push({ type: `${kind}-start`, id }, { type: `${kind}-delta`, id,
                delta: str(b.text ?? b.thinking) }, { type: `${kind}-end`, id });
            } else if (b.type === "tool_use") chunks.push({ type: "tool-input-available",
              toolCallId: b.id, toolName: b.name, input: b.input });
            else throw new Error("unsupported Claude snapshot block");
          }
        }
        break;
      }
      case "control_request": {
        const request = record(f.request);
        if (request.subtype !== "can_use_tool") throw new Error("unsupported inbound Claude request");
        chunks.push({ type: "tool-approval-request", approvalId: f.request_id,
          toolCallId: request.tool_use_id, approvalDescriptor: request });
        break;
      }
      case "user": {
        const content = record(f.message).content;
        if (Array.isArray(content)) for (const value of content) {
          const b = record(value);
          if (b.type === "tool_result") chunks.push(b.is_error
            ? { type: "tool-output-error", toolCallId: b.tool_use_id, errorText: typeof b.content === "string" ? b.content : JSON.stringify(b.content) }
            : { type: "tool-output-available", toolCallId: b.tool_use_id, output: b.content });
          else if (b.type !== "text" || b.text !== "[Request interrupted by user]") throw new Error("unsupported inbound Claude user block");
        }
        break;
      }
      case "result": {
        // Interrupt captures omit block_stop. Close only content blocks; incomplete tool JSON is an error.
        for (const index of [...blocks.keys()]) close(index);
        const usage = record(f.usage), models = Object.keys(record(f.modelUsage ?? {}));
        chunks.push({ type: "data-usage", data: { inputTokens: usage.input_tokens,
          outputTokens: usage.output_tokens, cachedInputTokens: usage.cache_read_input_tokens,
          reasoningTokens: record(usage.output_tokens_details).thinking_tokens, model: models[0] ?? "claude-captured" } });
        if (f.is_error === true) chunks.push(f.terminal_reason === "aborted_streaming"
          ? { type: "abort", reason: "aborted_streaming" }
          : { type: "error", errorText: str(f.result ?? JSON.stringify(f.errors ?? [])) });
        else if (f.is_error !== false) throw new Error("missing Claude is_error");
        if (f.terminal_reason !== "aborted_streaming") chunks.push({ type: "finish", finishReason: f.is_error ? "error" : "stop" });
        break;
      }
      case "control_response": break; // Receipt for our outgoing interrupt, not a permission outcome.
      default: throw new Error(`unclassified retained Claude frame ${String(f.type)}`);
    }
  }
  if (blocks.size) throw new Error("unterminated Claude fixture");
  return chunks;
}

export function normalizeCodex(input: unknown): Chunk[] {
  const chunks: Chunk[] = [];
  const text = new Map<string, string>();
  const approvals = new Map<string | number, string>();
  for (const row of input as Row[]) {
    const f = record(row.frame);
    if (row.direction === "out") {
      if (!approvals.has(f.id as number)) throw new Error("unmatched Codex approval response");
      const decision = str(record(f.result).decision);
      if (!["accept", "acceptForSession", "decline", "cancel"].includes(decision)) throw new Error("unsupported captured decision");
      const approvalId = approvals.get(f.id as number)!;
      chunks.push({ type: "tool-approval-response", approvalId, approved: decision === "accept" || decision === "acceptForSession", reason: decision });
      continue;
    }
    const p = record(f.params), method = str(f.method);
    switch (method) {
      case "turn/started": chunks.push(start(str(record(p.turn).id))); break;
      case "item/started": case "item/completed": {
        const item = record(p.item), id = str(item.id), completed = method === "item/completed";
        switch (item.type) {
          case "userMessage": break; // Input is retained, not assistant output.
          case "agentMessage": {
            if (!completed) { text.set(id, ""); chunks.push({ type: "text-start", id }); }
            else {
              const seen = text.get(id);
              if (seen === undefined) throw new Error("Codex message completed without start");
              const final = str(item.text);
              if (seen && seen !== final) throw new Error("Codex text delta/snapshot mismatch");
              if (!seen && final) chunks.push({ type: "text-delta", id, delta: final });
              chunks.push({ type: "text-end", id }); text.delete(id);
            }
            break;
          }
          case "commandExecution": case "mcpToolCall": {
            if (!completed) chunks.push({ type: "tool-input-available", toolCallId: id,
              toolName: item.type === "commandExecution" ? "commandExecution" : `${str(item.server)}.${str(item.tool)}`,
              input: item.type === "commandExecution" ? { command: item.command, cwd: item.cwd } : item.arguments });
            else if (item.status === "declined") chunks.push({ type: "tool-output-denied", toolCallId: id });
            else if (item.error) chunks.push({ type: "tool-output-error", toolCallId: id, errorText: JSON.stringify(item.error) });
            else chunks.push({ type: "tool-output-available", toolCallId: id,
              output: item.type === "commandExecution" ? { stdout: item.aggregatedOutput, exitCode: item.exitCode, status: item.status, durationMs: item.durationMs } : item.result });
            break;
          }
          default: throw new Error(`unsupported captured Codex item ${String(item.type)}`);
        }
        break;
      }
      case "item/agentMessage/delta": {
        const id = str(p.itemId), delta = str(p.delta);
        if (!text.has(id)) throw new Error("Codex delta without item");
        text.set(id, text.get(id)! + delta); chunks.push({ type: "text-delta", id, delta }); break;
      }
      case "item/commandExecution/requestApproval": case "item/fileChange/requestApproval": {
        const approvalId = `${str(p.turnId)}:approval:${String(f.id)}`;
        approvals.set(f.id as number, approvalId);
        chunks.push({ type: "tool-approval-request", approvalId, toolCallId: p.itemId, approvalDescriptor: p }); break;
      }
      case "thread/tokenUsage/updated": {
        const usage = record(record(p.tokenUsage).total);
        chunks.push({ type: "data-usage", data: { inputTokens: usage.inputTokens, outputTokens: usage.outputTokens,
          cachedInputTokens: usage.cachedInputTokens, reasoningTokens: usage.reasoningOutputTokens, model: "codex-captured" } }); break;
      }
      case "turn/completed": {
        const turn = record(p.turn);
        for (const id of text.keys()) chunks.push({ type: "text-end", id });
        text.clear();
        if (turn.status === "interrupted") chunks.push({ type: "abort", reason: "interrupted" });
        else if (turn.status === "failed") chunks.push({ type: "error", errorText: JSON.stringify(turn.error) }, { type: "finish", finishReason: "error" });
        else if (turn.status === "completed") chunks.push({ type: "finish", finishReason: "stop" });
        else throw new Error("unknown Codex terminal status");
        break;
      }
      case "error": chunks.push({ type: "error", errorText: JSON.stringify(p) }); break;
      default: throw new Error(`unclassified retained Codex method ${method}`);
    }
  }
  if (text.size) throw new Error("unterminated Codex fixture");
  return chunks;
}
