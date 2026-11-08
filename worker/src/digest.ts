import { redact } from "./redact.ts";
import type { Agent } from "./registry.ts";

export type Entry = {
	kind: "user" | "assistant" | "tool" | "error";
	text: string;
};

const SUMMARY_MAX = 200;
const ENTRY_SEPARATOR = "\n\n";

const oneLine = (text: string, max = SUMMARY_MAX) => {
	const flat = text.replace(/\s+/g, " ").trim();
	return flat.length > max ? `${flat.slice(0, max)}…` : flat;
};

const firstLine = (text: string) =>
	oneLine(
		text
			.split("\n")
			.map((l) => l.trim())
			.find((l) => l !== "") ?? "",
	);

const isRecord = (value: unknown): value is Record<string, unknown> =>
	typeof value === "object" && value !== null && !Array.isArray(value);

const parseJson = (text: string): unknown => {
	try {
		return JSON.parse(text);
	} catch {
		return undefined;
	}
};

// 入力の全体は長いので、何をしたかが分かる項目だけを拾う
const SUMMARY_KEYS = [
	"command",
	"cmd",
	"file_path",
	"path",
	"pattern",
	"url",
	"query",
	"skill",
	"description",
	"prompt",
];

export const summarizeInput = (input: unknown): string => {
	if (typeof input === "string") {
		const parsed = parseJson(input);
		return isRecord(parsed) ? summarizeInput(parsed) : oneLine(input);
	}
	if (!isRecord(input)) return oneLine(JSON.stringify(input) ?? "");
	const picked = SUMMARY_KEYS.flatMap((key) => {
		const value = input[key];
		if (typeof value === "string") return [value];
		if (Array.isArray(value)) return [value.map(String).join(" ")];
		return [];
	});
	return oneLine(picked.length > 0 ? picked.join(" ") : JSON.stringify(input));
};

const textOf = (content: unknown): string => {
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";
	return content
		.flatMap((item) =>
			isRecord(item) && typeof item.text === "string" ? [item.text] : [],
		)
		.join("\n");
};

/** tool_use の id から名前を引き、結果のエラーに名前を添える */
export type DigestContext = { toolNames: Map<string, string> };

const toolName = (ctx: DigestContext, id: unknown) =>
	(typeof id === "string" && ctx.toolNames.get(id)) || "tool";

// Claude Code が人の発言の形で差し込む付け足しは、会話の本文ではない
const stripClaudeInjected = (text: string) =>
	text
		.replace(/<system-reminder>[\s\S]*?<\/system-reminder>/g, "")
		.replace(/<local-command-[a-z]+>[\s\S]*?<\/local-command-[a-z]+>/g, "")
		.trim();

export const claudeEntries = (line: unknown, ctx: DigestContext): Entry[] => {
	if (!isRecord(line) || line.isMeta === true || line.isSidechain === true) {
		return [];
	}
	const kind = line.type;
	if (kind !== "user" && kind !== "assistant") return [];
	const message = line.message;
	if (!isRecord(message)) return [];
	const content = message.content;
	if (typeof content === "string") {
		const text = kind === "user" ? stripClaudeInjected(content) : content;
		return text.trim() ? [{ kind, text: text.trim() }] : [];
	}
	if (!Array.isArray(content)) return [];
	return content.flatMap((block): Entry[] => {
		if (!isRecord(block)) return [];
		if (block.type === "text" && typeof block.text === "string") {
			const text =
				kind === "user" ? stripClaudeInjected(block.text) : block.text;
			return text.trim() ? [{ kind, text: text.trim() }] : [];
		}
		if (block.type === "tool_use" && typeof block.name === "string") {
			if (typeof block.id === "string") ctx.toolNames.set(block.id, block.name);
			return [
				{
					kind: "tool",
					text: `${block.name}: ${summarizeInput(block.input)}`,
				},
			];
		}
		if (block.type === "tool_result" && block.is_error === true) {
			return [
				{
					kind: "error",
					text: `${toolName(ctx, block.tool_use_id)}: ${firstLine(textOf(block.content))}`,
				},
			];
		}
		return [];
	});
};

// Codex が人の発言として記録に入れる、AGENTS.md や環境の差し込み
const CODEX_INJECTED =
	/^(?:<(?:environment_context|user_instructions|permissions instructions|collaboration_mode|turn_aborted)\b|# AGENTS\.md instructions)/;

const codexError = (output: unknown): string | null => {
	const parsed = typeof output === "string" ? parseJson(output) : output;
	if (isRecord(parsed) && isRecord(parsed.metadata)) {
		const code = parsed.metadata.exit_code;
		if (typeof code === "number" && code !== 0) {
			return `exit ${code}: ${firstLine(textOf(parsed.output))}`;
		}
		return null;
	}
	const text = typeof output === "string" ? output : textOf(output);
	const code = /^(?:Exit code|Process exited with code):? (\d+)/m.exec(text);
	if (code && code[1] !== "0") {
		const body = text.split(/^Output:\s*$/m)[1] ?? "";
		return `exit ${code[1]}: ${firstLine(body)}`;
	}
	return null;
};

export const codexEntries = (line: unknown, ctx: DigestContext): Entry[] => {
	if (!isRecord(line)) return [];
	// 古い rollout は包みが無く、項目が行にそのまま載っている
	const item =
		line.type === "response_item"
			? line.payload
			: isRecord(line.payload)
				? null
				: line;
	if (!isRecord(item)) return [];
	switch (item.type) {
		case "message": {
			if (item.role !== "user" && item.role !== "assistant") return [];
			const kind = item.role;
			const content = Array.isArray(item.content) ? item.content : [];
			return content.flatMap((part): Entry[] => {
				if (!isRecord(part) || typeof part.text !== "string") return [];
				const text = part.text.trim();
				if (!text || (kind === "user" && CODEX_INJECTED.test(text))) return [];
				return [{ kind, text }];
			});
		}
		case "function_call":
		case "custom_tool_call": {
			if (typeof item.name !== "string") return [];
			if (typeof item.call_id === "string") {
				ctx.toolNames.set(item.call_id, item.name);
			}
			const input = item.type === "function_call" ? item.arguments : item.input;
			return [{ kind: "tool", text: `${item.name}: ${summarizeInput(input)}` }];
		}
		case "local_shell_call": {
			if (typeof item.call_id === "string") {
				ctx.toolNames.set(item.call_id, "shell");
			}
			return [{ kind: "tool", text: `shell: ${summarizeInput(item.action)}` }];
		}
		case "web_search_call":
			return [
				{ kind: "tool", text: `web_search: ${summarizeInput(item.action)}` },
			];
		case "function_call_output":
		case "custom_tool_call_output": {
			const error = codexError(item.output);
			return error
				? [{ kind: "error", text: `${toolName(ctx, item.call_id)}: ${error}` }]
				: [];
		}
	}
	return [];
};

const formatEntry = (entry: Entry) => `[${entry.kind}] ${redact(entry.text)}`;

export type Digest = {
	/** 残した発言を古い順につないだもの */
	text: string;
	kept: number;
	omitted: number;
	/** 1 件だけで上限を超え、その発言の頭を削ったか */
	truncated: boolean;
};

/**
 * 記録の行を要点に絞り、新しい側から maxChars に収まる分だけを残す。
 * 記録は大きくなりうるので、全体を持たずに上限の分だけを持って読み進める。
 */
export const digestLines = async (
	lines: AsyncIterable<string> | Iterable<string>,
	agent: Agent,
	maxChars: number,
): Promise<Digest> => {
	const ctx: DigestContext = { toolNames: new Map() };
	const toEntries = agent === "codex" ? codexEntries : claudeEntries;
	let kept: string[] = [];
	let head = 0;
	let size = 0;
	let omitted = 0;
	for await (const raw of lines) {
		if (!raw.trim()) continue;
		const parsed = parseJson(raw);
		if (parsed === undefined) continue;
		for (const entry of toEntries(parsed, ctx)) {
			const text = formatEntry(entry);
			kept.push(text);
			size += text.length + ENTRY_SEPARATOR.length;
			while (size > maxChars && kept.length - head > 1) {
				size -= (kept[head] as string).length + ENTRY_SEPARATOR.length;
				head++;
				omitted++;
			}
			if (head > 1024) {
				kept = kept.slice(head);
				head = 0;
			}
		}
	}
	const rest = kept.slice(head);
	let truncated = false;
	const last = rest.length === 1 ? (rest[0] as string) : null;
	if (last !== null && last.length > maxChars) {
		rest[0] = `…${last.slice(last.length - maxChars + 1)}`;
		truncated = true;
	}
	return {
		text: rest.join(ENTRY_SEPARATOR),
		kept: rest.length,
		omitted,
		truncated,
	};
};

/** R2 の本文を行に分けて流す */
export async function* readLines(
	body: ReadableStream<Uint8Array>,
): AsyncGenerator<string> {
	let buffer = "";
	for await (const chunk of body.pipeThrough(new TextDecoderStream())) {
		buffer += chunk;
		let index = buffer.indexOf("\n");
		while (index !== -1) {
			yield buffer.slice(0, index);
			buffer = buffer.slice(index + 1);
			index = buffer.indexOf("\n");
		}
	}
	if (buffer) yield buffer;
}
