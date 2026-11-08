import * as v from "valibot";
import { digestLines, readLines } from "./digest.ts";
import { redact } from "./redact.ts";
import {
	NUMBER_MAX,
	NUMBER_MIN,
	type Registry,
	type SessionMeta,
} from "./registry.ts";
import { err, ok, type Result } from "./result.ts";
import { transcriptKey } from "./transcripts.ts";

export type McpDeps = { registry: Registry; bucket: R2Bucket };

// 新しい順。相手が求めた版を知っていればそれで、知らなければ先頭の版で答える
export const PROTOCOL_VERSIONS = ["2025-11-25", "2025-06-18", "2025-03-26"];

const SERVER_INFO = { name: "hishaku", version: "1.0.0" };

const DEFAULT_LIMIT = 20;
const MAX_LIMIT = 100;
const DEFAULT_MAX_CHARS = 20000;
const MIN_MAX_CHARS = 1000;
const MAX_MAX_CHARS = 200000;

const Message = v.object({
	jsonrpc: v.literal("2.0"),
	id: v.optional(v.nullable(v.union([v.string(), v.number()]))),
	method: v.optional(v.string()),
	params: v.optional(v.record(v.string(), v.unknown())),
});

const ListArgs = v.object({
	limit: v.optional(
		v.pipe(v.number(), v.integer(), v.minValue(1), v.maxValue(MAX_LIMIT)),
		DEFAULT_LIMIT,
	),
	query: v.optional(v.pipe(v.string(), v.maxLength(200))),
});

const SessionNumber = v.union([
	v.pipe(
		v.number(),
		v.integer(),
		v.minValue(NUMBER_MIN),
		v.maxValue(NUMBER_MAX),
	),
	v.pipe(v.string(), v.regex(/^\d{4}$/), v.transform(Number)),
]);

const GetArgs = v.object({
	number: SessionNumber,
	max_chars: v.optional(
		v.pipe(
			v.number(),
			v.integer(),
			v.minValue(MIN_MAX_CHARS),
			v.maxValue(MAX_MAX_CHARS),
		),
		DEFAULT_MAX_CHARS,
	),
});

const READ_ONLY = {
	readOnlyHint: true,
	destructiveHint: false,
	idempotentHint: true,
	openWorldHint: false,
};

export const TOOLS = [
	{
		name: "list_sessions",
		title: "預かっているセッションの一覧",
		description:
			"hishaku が預かっている生きているセッションを、最後に更新された順に返す。各行は number・title・agent・repo・branch・holder・pushed_at・updated_at。",
		inputSchema: {
			type: "object",
			properties: {
				limit: {
					type: "integer",
					minimum: 1,
					maximum: MAX_LIMIT,
					default: DEFAULT_LIMIT,
					description: "返す件数",
				},
				query: {
					type: "string",
					maxLength: 200,
					description: "件名に含む語で絞り込む（大文字と小文字は区別しない）",
				},
			},
			additionalProperties: false,
		},
		annotations: READ_ONLY,
	},
	{
		name: "get_session",
		title: "セッションの会話を読む",
		description:
			"番号のセッションの meta と、正本の記録を要点に絞った会話を返す。残すのは人と agent の発言の本文、ツール呼び出しの名前と入力の要約、ツールのエラーの 1 行。ツールの出力は省く。max_chars を超える分は古い側から削る。よく知られた形の token は [redacted] に伏せるが、完全ではない。",
		inputSchema: {
			type: "object",
			properties: {
				number: {
					type: "integer",
					minimum: NUMBER_MIN,
					maximum: NUMBER_MAX,
					description: "4 桁の番号",
				},
				max_chars: {
					type: "integer",
					minimum: MIN_MAX_CHARS,
					maximum: MAX_MAX_CHARS,
					default: DEFAULT_MAX_CHARS,
					description: "会話の部分の上限の文字数",
				},
			},
			required: ["number"],
			additionalProperties: false,
		},
		annotations: READ_ONLY,
	},
] as const;

type ToolResult = {
	content: { type: "text"; text: string }[];
	structuredContent?: Record<string, unknown>;
	isError?: boolean;
};

const textResult = (text: string): ToolResult => ({
	content: [{ type: "text", text }],
});

const toolError = (text: string): ToolResult => ({
	...textResult(text),
	isError: true,
});

type RpcError = { code: number; message: string };

const issuesText = (issues: v.BaseIssue<unknown>[]) =>
	issues
		.map((i) => `${v.getDotPath(i) ?? "arguments"}: ${i.message}`)
		.join("; ");

const listRow = (meta: SessionMeta) => ({
	number: meta.number,
	title: meta.title,
	agent: meta.agent,
	repo: meta.repo,
	branch: meta.branch,
	holder: meta.holder,
	pushed_at: meta.pushed_at,
	updated_at: meta.updated_at,
});

const listSessions = (args: unknown, deps: McpDeps): ToolResult => {
	const parsed = v.safeParse(ListArgs, args);
	if (!parsed.success) return toolError(issuesText(parsed.issues));
	const sessions = deps.registry.list(parsed.output).map(listRow);
	const structured = JSON.parse(redact(JSON.stringify({ sessions })));
	return {
		content: [{ type: "text", text: JSON.stringify(structured, null, 2) }],
		structuredContent: structured,
	};
};

const getSession = async (
	args: unknown,
	deps: McpDeps,
): Promise<ToolResult> => {
	const parsed = v.safeParse(GetArgs, args);
	if (!parsed.success) return toolError(issuesText(parsed.issues));
	const { number, max_chars } = parsed.output;
	const meta = deps.registry.get(number);
	if (!meta)
		return toolError(`${number} は預かっていない（期限切れか未払い出し）`);
	const header = `# ${number}\n\n\`\`\`json\n${redact(JSON.stringify(meta, null, 2))}\n\`\`\``;
	const object = await deps.bucket.get(transcriptKey(number));
	if (!object) return textResult(`${header}\n\n記録はまだ預けられていない。`);
	const digest = await digestLines(
		readLines(object.body),
		meta.agent,
		max_chars,
	);
	const notes: string[] = [];
	if (digest.omitted > 0) {
		notes.push(
			`上限 ${max_chars} 文字を超えたので、古い ${digest.omitted} 件を省き、新しい ${digest.kept} 件を残した。`,
		);
	}
	if (digest.truncated)
		notes.push("残した 1 件も上限を超えたので、頭を削った。");
	const body = digest.text || "（読み取れる発言が無い）";
	return textResult([header, "## 会話", ...notes, body].join("\n\n"));
};

const callTool = async (
	params: Record<string, unknown>,
	deps: McpDeps,
): Promise<Result<ToolResult, RpcError>> => {
	const args = params.arguments ?? {};
	switch (params.name) {
		case "list_sessions":
			return ok(listSessions(args, deps));
		case "get_session":
			return ok(await getSession(args, deps));
	}
	return err({ code: -32602, message: `知らない道具: ${String(params.name)}` });
};

const dispatch = async (
	method: string,
	params: Record<string, unknown>,
	deps: McpDeps,
): Promise<Result<unknown, RpcError>> => {
	switch (method) {
		case "initialize": {
			const requested = params.protocolVersion;
			const version =
				typeof requested === "string" && PROTOCOL_VERSIONS.includes(requested)
					? requested
					: PROTOCOL_VERSIONS[0];
			return ok({
				protocolVersion: version,
				capabilities: { tools: { listChanged: false } },
				serverInfo: SERVER_INFO,
				instructions:
					"hishaku が預かっている agent のセッションを番号で読む。list_sessions で番号を探し、get_session で会話の要点を読む。",
			});
		}
		case "ping":
			return ok({});
		case "tools/list":
			return ok({ tools: TOOLS });
		case "tools/call":
			return callTool(params, deps);
	}
	return err({ code: -32601, message: `知らないメソッド: ${method}` });
};

const rpcError = (id: string | number | null, error: RpcError, status = 200) =>
	Response.json({ jsonrpc: "2.0", id, error }, { status });

/**
 * MCP の Streamable HTTP を、状態を持たない形で受ける。
 * 道具はどれも読み取りだけで、サーバーから送りたいものも無いので、SSE の流れもセッションも持たない。
 */
export const handleMcp = async (
	request: Request,
	deps: McpDeps,
): Promise<Response> => {
	if (request.method !== "POST") {
		return new Response(null, { status: 405, headers: { Allow: "POST" } });
	}
	// Access を通っていても、ブラウザから別のサイト経由で叩かれないよう Origin を見る
	const origin = request.headers.get("origin");
	if (origin !== null && new URL(origin).host !== new URL(request.url).host) {
		return rpcError(null, { code: -32600, message: "Origin が違う" }, 403);
	}
	let body: unknown;
	try {
		body = await request.json();
	} catch {
		return rpcError(
			null,
			{ code: -32700, message: "JSON として読めない" },
			400,
		);
	}
	const parsed = v.safeParse(Message, body);
	if (!parsed.success) {
		return rpcError(
			null,
			{ code: -32600, message: issuesText(parsed.issues) },
			400,
		);
	}
	const { id, method, params } = parsed.output;
	// 通知と、こちらが出していない要求への応答は、受け取ったとだけ返す
	if (id === undefined || id === null || method === undefined) {
		return new Response(null, { status: 202 });
	}
	const result = await dispatch(method, params ?? {}, deps);
	if (!result.ok) return rpcError(id, result.error);
	return Response.json({ jsonrpc: "2.0", id, result: result.value });
};
