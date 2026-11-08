import { runDurableObjectAlarm } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { registryOf } from "../src/index.ts";
import { call, createSession, onRegistry, postId, send } from "./helpers.ts";

const putTranscript = (
	number: number,
	holder: string,
	generation: number,
	body: string,
) =>
	call(
		`/v1/sessions/${number}/transcript?holder=${holder}&generation=${generation}`,
		{ method: "PUT", body },
	);

const acquire = async (number: number, holder: string, mode = "acquire") =>
	send("POST", `/v1/sessions/${number}/lease`, {
		holder,
		mode,
		ttl_seconds: 120,
	});

describe("GET /health", () => {
	it("応答する", async () => {
		const res = await call("/health");
		expect(res.status).toBe(200);
	});
});

describe("POST /v1/sessions", () => {
	it("番号を払い出し、同じ post_id なら同じ番号を返す", async () => {
		const id = postId();
		const body = {
			post_id: id,
			agent: "codex",
			title: "件名",
			repo: "example/app",
		};
		const first = await send("POST", "/v1/sessions", body);
		expect(first.status).toBe(201);
		const meta = await first.json<Record<string, unknown>>();
		expect(meta).toMatchObject({
			post_id: id,
			agent: "codex",
			title: "件名",
			repo: "example/app",
			branch: null,
			session_id: null,
			holder: null,
			generation: 0,
			lease_expires_at: null,
		});
		expect(meta.number).toBeGreaterThanOrEqual(1000);
		expect(meta.number).toBeLessThanOrEqual(9999);

		const second = await send("POST", "/v1/sessions", body);
		expect(second.status).toBe(200);
		expect((await second.json<{ number: number }>()).number).toBe(meta.number);

		const found = await call(`/v1/sessions?post_id=${id}`);
		expect((await found.json<{ number: number }>()).number).toBe(meta.number);
	});

	it("入力が足りなければ 400", async () => {
		const res = await send("POST", "/v1/sessions", {
			post_id: postId(),
			agent: "other",
		});
		expect(res.status).toBe(400);
		expect(await res.json()).toMatchObject({ error: "invalid_request" });
	});

	it("知らない post_id は 404", async () => {
		const res = await call(`/v1/sessions?post_id=${postId()}`);
		expect(res.status).toBe(404);
	});
});

describe("GET・PATCH /v1/sessions/{number}", () => {
	it("meta を更新して返す", async () => {
		const { number } = await createSession();
		const res = await send("PATCH", `/v1/sessions/${number}`, {
			title: "新しい件名",
			branch: "dev/example",
			session_id: "00000000-0000-4000-8000-000000000000",
		});
		expect(res.status).toBe(200);
		const got = await call(`/v1/sessions/${number}`);
		expect(await got.json()).toMatchObject({
			number,
			title: "新しい件名",
			branch: "dev/example",
			session_id: "00000000-0000-4000-8000-000000000000",
		});
	});

	it("更新する項目が無ければ 400、無い番号は 404", async () => {
		const { number } = await createSession();
		expect((await send("PATCH", `/v1/sessions/${number}`, {})).status).toBe(
			400,
		);
		expect((await call("/v1/sessions/999")).status).toBe(404);
	});
});

describe("貸し出しの API", () => {
	it("acquire は持ち主がいれば 409 で持ち主と期限を返す", async () => {
		const { number } = await createSession();
		const first = await acquire(number, "mac-a");
		expect(first.status).toBe(200);
		const lease = await first.json<{
			generation: number;
			lease_expires_at: string;
		}>();
		expect(lease.generation).toBe(1);

		const second = await acquire(number, "gpu-box");
		expect(second.status).toBe(409);
		expect(await second.json()).toEqual({
			error: "lease_held",
			holder: "mac-a",
			lease_expires_at: lease.lease_expires_at,
		});
	});

	it("takeover で世代が上がり、取られた側の renew・DELETE・PUT は 409", async () => {
		const { number } = await createSession();
		await acquire(number, "mac-a");
		const taken = await acquire(number, "gpu-box", "takeover");
		expect(taken.status).toBe(200);
		expect((await taken.json<{ generation: number }>()).generation).toBe(2);

		const stale = { holder: "mac-a", generation: 1 };
		const renew = await send(
			"POST",
			`/v1/sessions/${number}/lease/renew`,
			stale,
		);
		expect(renew.status).toBe(409);
		expect(await renew.json()).toMatchObject({
			error: "lease_mismatch",
			holder: "gpu-box",
			generation: 2,
		});
		expect(
			(await send("DELETE", `/v1/sessions/${number}/lease`, stale)).status,
		).toBe(409);
		expect((await putTranscript(number, "mac-a", 1, "{}\n")).status).toBe(409);

		const current = { holder: "gpu-box", generation: 2 };
		const renewed = await send(
			"POST",
			`/v1/sessions/${number}/lease/renew`,
			current,
		);
		expect(renewed.status).toBe(200);
		expect(
			(await send("DELETE", `/v1/sessions/${number}/lease`, current)).status,
		).toBe(204);
		expect(await (await call(`/v1/sessions/${number}`)).json()).toMatchObject({
			holder: null,
			generation: 2,
		});
	});
});

describe("返してほしいと頼む API", () => {
	it("持ち主がいれば 200 で頼んだ側を返し、renew と meta に載る", async () => {
		const { number } = await createSession();
		await acquire(number, "mac-a");
		const requested = await send(
			"POST",
			`/v1/sessions/${number}/lease/request`,
			{ holder: "gpu-box" },
		);
		expect(requested.status).toBe(200);
		expect(await requested.json()).toMatchObject({
			holder: "mac-a",
			generation: 1,
			release_requested_by: "gpu-box",
		});
		const renewed = await send("POST", `/v1/sessions/${number}/lease/renew`, {
			holder: "mac-a",
			generation: 1,
		});
		expect(await renewed.json()).toMatchObject({
			generation: 1,
			release_requested_by: "gpu-box",
		});
		expect(await (await call(`/v1/sessions/${number}`)).json()).toMatchObject({
			release_requested_by: "gpu-box",
		});
	});

	it("誰も持っていなければ lease_free、持ち主自身なら lease_mismatch、holder が無ければ 400", async () => {
		const { number } = await createSession();
		const path = `/v1/sessions/${number}/lease/request`;
		const free = await send("POST", path, { holder: "gpu-box" });
		expect(free.status).toBe(409);
		expect(await free.json()).toEqual({ error: "lease_free" });

		await acquire(number, "mac-a");
		const self = await send("POST", path, { holder: "mac-a" });
		expect(self.status).toBe(409);
		expect(await self.json()).toMatchObject({ error: "lease_mismatch" });

		expect((await send("POST", path, {})).status).toBe(400);
		expect((await send("GET", path)).status).toBe(405);
	});
});

describe("記録の API", () => {
	it("記録が無ければ 404、置けば世代つきで返す", async () => {
		const { number } = await createSession();
		expect((await call(`/v1/sessions/${number}/transcript`)).status).toBe(404);
		await acquire(number, "docker01");
		const before = await (await call(`/v1/sessions/${number}`)).json<{
			pushed_at: string | null;
		}>();
		expect(before.pushed_at).toBeNull();

		const jsonl = '{"type":"user"}\n{"type":"assistant"}\n';
		const put = await putTranscript(number, "docker01", 1, jsonl);
		expect(put.status).toBe(204);
		const after = await (await call(`/v1/sessions/${number}`)).json<{
			post_id: string;
			pushed_at: string | null;
		}>();
		expect(after.pushed_at).toMatch(/^\d{4}-\d{2}-\d{2}T/);
		const byPost = await call(
			`/v1/sessions?post_id=${encodeURIComponent(after.post_id)}`,
		);
		expect(await byPost.json()).toMatchObject({ pushed_at: after.pushed_at });

		const got = await call(`/v1/sessions/${number}/transcript`);
		expect(got.status).toBe(200);
		expect(got.headers.get("X-Hishaku-Generation")).toBe("1");
		expect(new TextDecoder().decode(await got.arrayBuffer())).toBe(jsonl);
	});

	it("holder と generation が無ければ 400", async () => {
		const { number } = await createSession();
		const res = await call(`/v1/sessions/${number}/transcript`, {
			method: "PUT",
			body: "{}\n",
		});
		expect(res.status).toBe(400);
	});

	it("forks は取られた側の記録を枝として残し、key を返す", async () => {
		const { number } = await createSession();
		const fork = (body: string) =>
			call(`/v1/sessions/${number}/forks?holder=mac-a`, {
				method: "POST",
				body,
			});
		const first = await fork('{"n":1}\n');
		expect(first.status).toBe(201);
		const { key } = await first.json<{ key: string }>();
		expect(key).toMatch(
			new RegExp(`^sessions/${number}/forks/\\d{8}-\\d{6}\\.jsonl$`),
		);
		expect(await (await env.TRANSCRIPTS.get(key))?.text()).toBe('{"n":1}\n');

		// 同じ秒に預けても前の枝を上書きしない
		const second = await fork('{"n":2}\n');
		const { key: key2 } = await second.json<{ key: string }>();
		expect(key2).not.toBe(key);
		expect(await (await env.TRANSCRIPTS.get(key))?.text()).toBe('{"n":1}\n');
		// forks は正本ではないので、最後に預けた時刻は変えない
		expect(await (await call(`/v1/sessions/${number}`)).json()).toMatchObject({
			pushed_at: null,
		});
	});
});

describe("期限切れの掃除", () => {
	it("alarm で期限の過ぎた meta と R2 の記録を消し、生きているものは残す", async () => {
		const dead = await createSession();
		const alive = await createSession();
		await acquire(dead.number, "docker01");
		await putTranscript(dead.number, "docker01", 1, "{}\n");
		await call(`/v1/sessions/${dead.number}/forks`, {
			method: "POST",
			body: "{}\n",
		});
		await acquire(alive.number, "docker01");
		await putTranscript(alive.number, "docker01", 1, "{}\n");

		await onRegistry((sql) =>
			sql.exec(
				"UPDATE sessions SET expires_at = ? WHERE number = ?",
				Date.now() - 1,
				dead.number,
			),
		);
		expect(await runDurableObjectAlarm(registryOf(env))).toBe(true);

		expect((await call(`/v1/sessions/${dead.number}`)).status).toBe(404);
		const left = await env.TRANSCRIPTS.list({
			prefix: `sessions/${dead.number}/`,
		});
		expect(left.objects).toEqual([]);
		expect((await call(`/v1/sessions/${alive.number}/transcript`)).status).toBe(
			200,
		);
		const rows = await onRegistry((sql) =>
			sql
				.exec("SELECT number FROM sessions WHERE number = ?", dead.number)
				.toArray(),
		);
		expect(rows).toEqual([]);
	});
});
