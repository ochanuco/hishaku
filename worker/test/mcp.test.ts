import { describe, expect, it } from "vitest";
import { digestLines } from "../src/digest.ts";
import { redact } from "../src/redact.ts";
import { call, createSession, isolatedRegistry, send } from "./helpers.ts";

type RpcResponse = {
	jsonrpc: "2.0";
	id: number | string | null;
	result?: Record<string, unknown>;
	error?: { code: number; message: string };
};

type ToolResult = {
	content: { type: "text"; text: string }[];
	structuredContent?: { sessions: Record<string, unknown>[] };
	isError?: boolean;
};

let seq = 0;
const rpc = async (method: string, params?: Record<string, unknown>) => {
	const res = await send("POST", "/mcp", {
		jsonrpc: "2.0",
		id: ++seq,
		method,
		params,
	});
	expect(res.status).toBe(200);
	return res.json<RpcResponse>();
};

const callTool = async (name: string, args: Record<string, unknown>) => {
	const res = await rpc("tools/call", { name, arguments: args });
	return res.result as ToolResult;
};

const textOf = (result: ToolResult) =>
	result.content.map((c) => c.text).join("");

const pushTranscript = async (number: number, lines: unknown[]) => {
	const holder = "mac-example";
	const leased = await send("POST", `/v1/sessions/${number}/lease`, {
		holder,
		mode: "acquire",
		ttl_seconds: 120,
	});
	const { generation } = await leased.json<{ generation: number }>();
	const res = await call(
		`/v1/sessions/${number}/transcript?holder=${holder}&generation=${generation}`,
		{ method: "PUT", body: lines.map((l) => JSON.stringify(l)).join("\n") },
	);
	expect(res.status).toBe(204);
};

// 架空の token。形だけが本物に似ている
const FAKE_GH = `ghp_${"a1B2".repeat(9)}`;
const FAKE_SK = `sk-ant-${"x9Y8".repeat(8)}`;

const claudeLines = [
	{ type: "summary", summary: "読まない行" },
	{
		type: "user",
		message: {
			role: "user",
			content: "README を直して<system-reminder>差し込み</system-reminder>",
		},
	},
	{
		type: "user",
		isMeta: true,
		message: { role: "user", content: "meta の行" },
	},
	{
		type: "assistant",
		message: {
			role: "assistant",
			content: [
				{ type: "thinking", thinking: "考えたこと" },
				{ type: "text", text: `見ます。token は ${FAKE_GH}` },
				{
					type: "tool_use",
					id: "toolu_1",
					name: "Bash",
					input: { command: "git status", description: "状態を見る" },
				},
				{
					type: "tool_use",
					id: "toolu_2",
					name: "Read",
					input: { file_path: "/repo/README.md" },
				},
			],
		},
	},
	{
		type: "user",
		message: {
			role: "user",
			content: [
				{
					type: "tool_result",
					tool_use_id: "toolu_1",
					content: "On branch main\nnothing to commit",
				},
				{
					type: "tool_result",
					tool_use_id: "toolu_2",
					is_error: true,
					content: [{ type: "text", text: "File does not exist.\n詳細" }],
				},
			],
		},
	},
	{
		type: "assistant",
		isSidechain: true,
		message: { role: "assistant", content: "subagent の発言" },
	},
	{
		type: "assistant",
		message: {
			role: "assistant",
			content: [{ type: "text", text: "直しました" }],
		},
	},
];

const codexLines = [
	{ type: "session_meta", payload: { id: "thread-1", cwd: "/repo" } },
	{
		type: "response_item",
		payload: {
			type: "message",
			role: "developer",
			content: [{ type: "input_text", text: "開発者の指示" }],
		},
	},
	{
		type: "response_item",
		payload: {
			type: "message",
			role: "user",
			content: [
				{ type: "input_text", text: "# AGENTS.md instructions for /repo" },
				{
					type: "input_text",
					text: "<environment_context>\n  <cwd>/repo</cwd>\n</environment_context>",
				},
				{ type: "input_text", text: "テストを直して" },
			],
		},
	},
	{
		type: "event_msg",
		payload: { type: "user_message", message: "テストを直して" },
	},
	{ type: "response_item", payload: { type: "reasoning", summary: [] } },
	{
		type: "response_item",
		payload: {
			type: "function_call",
			name: "shell",
			call_id: "call_1",
			arguments: JSON.stringify({ command: ["bash", "-lc", "pnpm test"] }),
		},
	},
	{
		type: "response_item",
		payload: {
			type: "function_call_output",
			call_id: "call_1",
			output: JSON.stringify({
				output: "FAIL test/a.test.ts\n長い出力",
				metadata: { exit_code: 1 },
			}),
		},
	},
	{
		type: "response_item",
		payload: {
			type: "custom_tool_call",
			name: "apply_patch",
			call_id: "call_2",
			input: "*** Begin Patch\n*** Update File: a.ts\n*** End Patch",
		},
	},
	{
		type: "response_item",
		payload: {
			type: "custom_tool_call_output",
			call_id: "call_2",
			output: "Exit code: 0\nWall time: 0.1 seconds\nOutput:\nSuccess",
		},
	},
	{
		type: "response_item",
		payload: {
			type: "message",
			role: "assistant",
			content: [{ type: "output_text", text: `直した。鍵は ${FAKE_SK}` }],
		},
	},
];

describe("POST /mcp", () => {
	it("initialize で版と道具の能力を返す", async () => {
		const res = await rpc("initialize", {
			protocolVersion: "2025-06-18",
			capabilities: {},
			clientInfo: { name: "test", version: "0" },
		});
		expect(res.result).toMatchObject({
			protocolVersion: "2025-06-18",
			capabilities: { tools: {} },
			serverInfo: { name: "hishaku" },
		});
	});

	it("知らない版を求められたら知っている新しい版で答える", async () => {
		const res = await rpc("initialize", { protocolVersion: "1999-01-01" });
		expect(res.result?.protocolVersion).toBe("2025-11-25");
	});

	it("通知には 202 で本文なしに返す", async () => {
		const res = await send("POST", "/mcp", {
			jsonrpc: "2.0",
			method: "notifications/initialized",
		});
		expect(res.status).toBe(202);
		expect(await res.text()).toBe("");
	});

	it("tools/list で読み取りだけの道具を 2 つ返す", async () => {
		const res = await rpc("tools/list");
		const tools = res.result?.tools as {
			name: string;
			inputSchema: { type: string };
			annotations: { readOnlyHint: boolean };
		}[];
		expect(tools.map((t) => t.name)).toEqual(["list_sessions", "get_session"]);
		for (const tool of tools) {
			expect(tool.inputSchema.type).toBe("object");
			expect(tool.annotations.readOnlyHint).toBe(true);
		}
	});

	it("知らないメソッドと道具は JSON-RPC のエラーで返す", async () => {
		expect((await rpc("resources/list")).error?.code).toBe(-32601);
		const res = await rpc("tools/call", { name: "nope", arguments: {} });
		expect(res.error?.code).toBe(-32602);
	});

	it("壊れた JSON は -32700、GET は 405、別の Origin は 403", async () => {
		const broken = await call("/mcp", { method: "POST", body: "{" });
		expect(broken.status).toBe(400);
		expect((await broken.json<RpcResponse>()).error?.code).toBe(-32700);
		expect((await call("/mcp")).status).toBe(405);
		const cross = await call("/mcp", {
			method: "POST",
			headers: { Origin: "https://evil.example.net" },
			body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "ping" }),
		});
		expect(cross.status).toBe(403);
	});

	it("引数が合わなければ isError の結果で返す", async () => {
		const result = await callTool("get_session", { number: 12 });
		expect(result.isError).toBe(true);
	});
});

describe("list_sessions", () => {
	it("件名の語で絞り込み、新しい順に返す", async () => {
		const word = `絞込${Date.now()}`;
		const first = await createSession({ title: `一つ目 ${word}` });
		const second = await createSession({
			title: `二つ目 ${word.toUpperCase()}`,
			repo: "example/app",
		});
		await createSession({ title: "関係ない件名" });
		// 先に作った方を後から更新すると、そちらが先頭に来る
		await new Promise((r) => setTimeout(r, 5));
		await send("PATCH", `/v1/sessions/${first.number}`, { branch: "dev/a" });

		const result = await callTool("list_sessions", { query: word });
		const rows = result.structuredContent?.sessions ?? [];
		expect(rows.map((r) => r.number)).toEqual([first.number, second.number]);
		expect(Object.keys(rows[0] ?? {}).sort()).toEqual(
			[
				"agent",
				"branch",
				"holder",
				"number",
				"pushed_at",
				"repo",
				"title",
				"updated_at",
			].sort(),
		);
		expect(JSON.parse(textOf(result))).toEqual(result.structuredContent);
	});

	it("limit で件数を絞る", async () => {
		await createSession();
		await createSession();
		const result = await callTool("list_sessions", { limit: 1 });
		expect(result.structuredContent?.sessions).toHaveLength(1);
	});

	it("台帳は更新の新しい順に並べ、期限切れを除く", async () => {
		let clock = 1_000_000;
		let next = 1000;
		const run = isolatedRegistry({
			now: () => clock,
			randomNumber: () => next++,
		});
		await run((registry) => {
			for (const title of ["a", "b", "c"]) {
				registry.create({ post_id: `p-${title}`, agent: "claude", title });
				clock += 1000;
			}
			registry.update(1000, { branch: "x" });
			expect(registry.list({ limit: 10 }).map((m) => m.title)).toEqual([
				"a",
				"c",
				"b",
			]);
			expect(
				registry.list({ limit: 10, query: "B" }).map((m) => m.title),
			).toEqual(["b"]);
			clock += 40 * 24 * 60 * 60 * 1000;
			expect(registry.list({ limit: 10 })).toEqual([]);
		});
	});
});

describe("get_session", () => {
	it("記録が無ければ meta だけを返す", async () => {
		const { number } = await createSession({ title: "記録なし" });
		const text = textOf(await callTool("get_session", { number }));
		expect(text).toContain(`"number": ${number}`);
		expect(text).toContain("記録はまだ預けられていない");
	});

	it("Claude の記録を要点に絞る", async () => {
		const { number } = await createSession({ agent: "claude" });
		await pushTranscript(number, claudeLines);
		const text = textOf(
			await callTool("get_session", { number: String(number) }),
		);
		const conversation = text.split("## 会話")[1] ?? "";
		expect(conversation.trim().split("\n\n")).toEqual([
			"[user] README を直して",
			"[assistant] 見ます。token は [redacted]",
			"[tool] Bash: git status 状態を見る",
			"[tool] Read: /repo/README.md",
			"[error] Read: File does not exist.",
			"[assistant] 直しました",
		]);
		expect(text).not.toContain("nothing to commit");
		expect(text).not.toContain(FAKE_GH);
	});

	it("Codex の記録を要点に絞る", async () => {
		const { number } = await createSession({ agent: "codex" });
		await pushTranscript(number, codexLines);
		const text = textOf(await callTool("get_session", { number }));
		const conversation = text.split("## 会話")[1] ?? "";
		expect(conversation.trim().split("\n\n")).toEqual([
			"[user] テストを直して",
			"[tool] shell: bash -lc pnpm test",
			"[error] shell: exit 1: FAIL test/a.test.ts",
			"[tool] apply_patch: *** Begin Patch *** Update File: a.ts *** End Patch",
			"[assistant] 直した。鍵は [redacted]",
		]);
		expect(text).not.toContain("Success");
		expect(text).not.toContain("長い出力");
	});

	it("上限を超えたら古い側を削り、削ったことを先に書く", async () => {
		const { number } = await createSession({ agent: "claude" });
		const lines = Array.from({ length: 50 }, (_, i) => ({
			type: i % 2 === 0 ? "user" : "assistant",
			message: { content: `発言${i} ${"あ".repeat(100)}` },
		}));
		await pushTranscript(number, lines);
		const text = textOf(
			await callTool("get_session", { number, max_chars: 1000 }),
		);
		const conversation = text.split("## 会話")[1] ?? "";
		expect(conversation).toMatch(/古い \d+ 件を省き/);
		expect(conversation.indexOf("省き")).toBeLessThan(
			conversation.indexOf("[user]") === -1
				? conversation.indexOf("[assistant]")
				: Math.min(
						conversation.indexOf("[user]"),
						conversation.indexOf("[assistant]"),
					),
		);
		expect(conversation).toContain("発言49");
		expect(conversation).not.toContain("発言0 ");
	});
});

describe("digestLines", () => {
	it("1 件だけで上限を超えたら頭を削って新しい側を残す", async () => {
		const line = JSON.stringify({
			type: "user",
			message: { content: `頭${"い".repeat(3000)}尾` },
		});
		const digest = await digestLines([line], "claude", 1000);
		expect(digest.truncated).toBe(true);
		expect(digest.text.length).toBe(1000);
		expect(digest.text.endsWith("尾")).toBe(true);
	});

	it("読めない行は飛ばす", async () => {
		const digest = await digestLines(["{", "", "null"], "codex", 1000);
		expect(digest).toEqual({ text: "", kept: 0, omitted: 0, truncated: false });
	});
});

describe("redact", () => {
	it("よく知られた token の形を伏せる", () => {
		const samples = [
			`github_pat_${"A1b2_".repeat(8)}`,
			`gho_${"Z9y8".repeat(9)}`,
			`ops_${"eyJh".repeat(8)}`,
			`sk-proj-${"AbCd".repeat(8)}`,
			`xoxb-${"1234-".repeat(4)}abcd`,
			"AKIAIOSFODNN7EXAMPLE",
		];
		for (const sample of samples) {
			expect(redact(`前 ${sample} 後`)).toBe("前 [redacted] 後");
		}
	});

	it("秘密鍵の塊を、END が無くても伏せる", () => {
		const block =
			"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\nBBBB\n-----END OPENSSH PRIVATE KEY-----";
		expect(redact(`前\n${block}\n後`)).toBe("前\n[redacted]\n後");
		expect(redact("-----BEGIN RSA PRIVATE KEY-----\nAAAA")).toBe("[redacted]");
	});

	it("名前で秘密と分かる値を伏せ、名前は残す", () => {
		expect(redact('-H "CF-Access-Client-Secret: abcdef0123456789" -H x')).toBe(
			'-H "CF-Access-Client-Secret: [redacted]" -H x',
		);
		expect(redact("Authorization: Bearer abc.def")).toBe(
			"Authorization: [redacted]",
		);
	});

	it("似ているだけの語は伏せない", () => {
		const text = "task-1234567890abcdefghijk と ghp_short";
		expect(redact(text)).toBe(text);
	});
});
