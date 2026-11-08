import { runInDurableObject } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { createRegistry, SESSION_TTL_MS } from "../src/registry.ts";
import { isolatedRegistry } from "./helpers.ts";

const T0 = Date.UTC(2026, 0, 1);
const input = (post_id: string) => ({
	post_id,
	agent: "claude" as const,
	title: "件名",
});

const clock = (start = T0) => {
	let at = start;
	return {
		now: () => at,
		advance: (ms: number) => {
			at += ms;
		},
	};
};

const sequence = (...numbers: number[]) => {
	let i = 0;
	return () => numbers[Math.min(i++, numbers.length - 1)] as number;
};

describe("番号の払い出し", () => {
	it("同じ post_id の生きている番号があればそれを返す", async () => {
		const run = isolatedRegistry({});
		await run((r) => {
			const first = r.create(input("p1"));
			const second = r.create(input("p1"));
			if (!first.ok || !second.ok) throw new Error("create failed");
			expect(first.value.created).toBe(true);
			expect(second.value.created).toBe(false);
			expect(second.value.meta.number).toBe(first.value.meta.number);
		});
	});

	it("生きている番号と重なる乱数は引き直す", async () => {
		const run = isolatedRegistry({ randomNumber: sequence(1234, 1234, 5678) });
		await run((r) => {
			const a = r.create(input("p1"));
			const b = r.create(input("p2"));
			if (!a.ok || !b.ok) throw new Error("create failed");
			expect(a.value.meta.number).toBe(1234);
			expect(b.value.meta.number).toBe(5678);
		});
	});

	it("乱数が埋まった番号ばかりでも、空いている番号を渡す", async () => {
		const run = isolatedRegistry({ randomNumber: () => 1000 });
		await run((r) => {
			const a = r.create(input("p1"));
			const b = r.create(input("p2"));
			if (!a.ok || !b.ok) throw new Error("create failed");
			expect(a.value.meta.number).toBe(1000);
			expect(b.value.meta.number).not.toBe(1000);
			expect(b.value.meta.number).toBeGreaterThanOrEqual(1000);
			expect(b.value.meta.number).toBeLessThanOrEqual(9999);
		});
	});

	it("期限は最後の更新から 30 日で、過ぎた post_id には新しい番号を渡す", async () => {
		const c = clock();
		const run = isolatedRegistry({
			now: c.now,
			randomNumber: sequence(2000, 3000),
		});
		await run((r) => {
			const a = r.create(input("p1"));
			if (!a.ok) throw new Error("create failed");
			expect(a.value.meta.expires_at).toBe(
				new Date(T0 + SESSION_TTL_MS).toISOString(),
			);
			c.advance(SESSION_TTL_MS - 1);
			r.update(2000, { title: "更新" });
			c.advance(SESSION_TTL_MS - 1);
			expect(r.get(2000)?.title).toBe("更新");
			c.advance(1);
			expect(r.get(2000)).toBeNull();
			expect(r.expired()).toEqual([2000]);
			const b = r.create(input("p1"));
			if (!b.ok) throw new Error("create failed");
			expect(b.value.meta.number).toBe(3000);
		});
	});

	it("掃除された番号はまた使う", async () => {
		const c = clock();
		const run = isolatedRegistry({
			now: c.now,
			randomNumber: sequence(4000, 4000),
		});
		await run((r) => {
			r.create(input("p1"));
			c.advance(SESSION_TTL_MS);
			for (const n of r.expired()) r.remove(n);
			const b = r.create(input("p2"));
			if (!b.ok) throw new Error("create failed");
			expect(b.value.meta.number).toBe(4000);
		});
	});
});

describe("貸し出し", () => {
	const setup = (c = clock()) => {
		const run = isolatedRegistry({ now: c.now, randomNumber: () => 1111 });
		return { run, c };
	};

	it("acquire は持ち主がいれば通らず、持ち主と期限を返す", async () => {
		const { run } = setup();
		await run((r) => {
			r.create(input("p1"));
			const a = r.lease(1111, {
				holder: "mac-a",
				mode: "acquire",
				ttl_seconds: 60,
			});
			expect(a).toEqual({
				ok: true,
				value: {
					generation: 1,
					lease_expires_at: new Date(T0 + 60_000).toISOString(),
					release_requested_by: null,
				},
			});
			const b = r.lease(1111, {
				holder: "gpu-box",
				mode: "acquire",
				ttl_seconds: 60,
			});
			expect(b).toEqual({
				ok: false,
				error: {
					kind: "lease_held",
					holder: "mac-a",
					lease_expires_at: new Date(T0 + 60_000).toISOString(),
				},
			});
		});
	});

	it("期限の切れた貸し出しは acquire で取れる", async () => {
		const { run, c } = setup();
		await run((r) => {
			r.create(input("p1"));
			r.lease(1111, { holder: "mac-a", mode: "acquire", ttl_seconds: 60 });
			c.advance(60_000);
			const b = r.lease(1111, {
				holder: "gpu-box",
				mode: "acquire",
				ttl_seconds: 60,
			});
			expect(b.ok && b.value.generation).toBe(2);
		});
	});

	it("takeover は持ち主がいても奪い、世代を 1 上げる", async () => {
		const { run } = setup();
		await run((r) => {
			r.create(input("p1"));
			r.lease(1111, { holder: "mac-a", mode: "acquire", ttl_seconds: 60 });
			const b = r.lease(1111, {
				holder: "gpu-box",
				mode: "takeover",
				ttl_seconds: 60,
			});
			expect(b.ok && b.value.generation).toBe(2);
			expect(r.get(1111)).toMatchObject({ holder: "gpu-box", generation: 2 });
		});
	});

	it("世代の合わない renew と返却は通らない", async () => {
		const { run } = setup();
		await run((r) => {
			r.create(input("p1"));
			r.lease(1111, { holder: "mac-a", mode: "acquire", ttl_seconds: 60 });
			r.lease(1111, { holder: "gpu-box", mode: "takeover", ttl_seconds: 60 });
			const stale = { holder: "mac-a", generation: 1 };
			const expected = {
				ok: false,
				error: {
					kind: "lease_mismatch",
					holder: "gpu-box",
					generation: 2,
					lease_expires_at: new Date(T0 + 60_000).toISOString(),
				},
			};
			expect(r.renew(1111, stale)).toEqual(expected);
			expect(r.release(1111, stale)).toEqual(expected);
			expect(r.checkLease(1111, "mac-a", 1)).toEqual(expected);
		});
	});

	it("renew は前の ttl で期限を延ばし、返却で持ち主が空く", async () => {
		const { run, c } = setup();
		await run((r) => {
			r.create(input("p1"));
			r.lease(1111, { holder: "mac-a", mode: "acquire", ttl_seconds: 60 });
			c.advance(30_000);
			const renewed = r.renew(1111, { holder: "mac-a", generation: 1 });
			expect(renewed.ok && renewed.value.lease_expires_at).toBe(
				new Date(T0 + 90_000).toISOString(),
			);
			expect(r.release(1111, { holder: "mac-a", generation: 1 }).ok).toBe(true);
			expect(r.get(1111)).toMatchObject({
				holder: null,
				lease_expires_at: null,
				generation: 1,
			});
			// 返した後の世代では、もう renew できない
			expect(r.renew(1111, { holder: "mac-a", generation: 1 }).ok).toBe(false);
		});
	});

	it("返してほしいと頼むと renew と meta に頼んだ側が載り、貸し出しが変わると消える", async () => {
		const { run } = setup();
		await run((r) => {
			r.create(input("p1"));
			r.lease(1111, { holder: "mac-a", mode: "acquire", ttl_seconds: 60 });
			expect(r.requestRelease(1111, "gpu-box")).toEqual({
				ok: true,
				value: {
					holder: "mac-a",
					generation: 1,
					lease_expires_at: new Date(T0 + 60_000).toISOString(),
					release_requested_by: "gpu-box",
				},
			});
			const renewed = r.renew(1111, { holder: "mac-a", generation: 1 });
			expect(renewed.ok && renewed.value.release_requested_by).toBe("gpu-box");
			expect(r.get(1111)?.release_requested_by).toBe("gpu-box");

			r.release(1111, { holder: "mac-a", generation: 1 });
			expect(r.get(1111)?.release_requested_by).toBeNull();

			r.lease(1111, { holder: "gpu-box", mode: "acquire", ttl_seconds: 60 });
			r.requestRelease(1111, "mac-a");
			r.lease(1111, { holder: "mac-b", mode: "takeover", ttl_seconds: 60 });
			expect(r.get(1111)?.release_requested_by).toBeNull();
		});
	});

	it("誰も持っていなければ lease_free、持ち主自身が頼めば lease_mismatch", async () => {
		const { run, c } = setup();
		await run((r) => {
			r.create(input("p1"));
			expect(r.requestRelease(1111, "gpu-box")).toEqual({
				ok: false,
				error: { kind: "lease_free" },
			});
			r.lease(1111, { holder: "mac-a", mode: "acquire", ttl_seconds: 60 });
			expect(r.requestRelease(1111, "mac-a")).toEqual({
				ok: false,
				error: {
					kind: "lease_mismatch",
					holder: "mac-a",
					generation: 1,
					lease_expires_at: new Date(T0 + 60_000).toISOString(),
				},
			});
			c.advance(60_000);
			expect(r.requestRelease(1111, "gpu-box")).toEqual({
				ok: false,
				error: { kind: "lease_free" },
			});
			expect(r.requestRelease(2222, "gpu-box")).toEqual({
				ok: false,
				error: { kind: "not_found" },
			});
		});
	});
});

describe("表の移行", () => {
	it("release_requested_by と pushed_at の無い表に列を足す", async () => {
		const stub = env.SESSION_REGISTRY.get(
			env.SESSION_REGISTRY.idFromName(`migrate-${Date.now()}`),
		);
		await runInDurableObject(stub, (_instance, state) => {
			const sql = state.storage.sql;
			sql.exec("DROP TABLE sessions");
			sql.exec(`CREATE TABLE sessions (
				number INTEGER PRIMARY KEY, post_id TEXT NOT NULL, agent TEXT NOT NULL,
				session_id TEXT, title TEXT NOT NULL, repo TEXT, branch TEXT, holder TEXT,
				generation INTEGER NOT NULL DEFAULT 0, lease_expires_at INTEGER,
				lease_ttl_seconds INTEGER, expires_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
			)`);
			const r = createRegistry(sql, { randomNumber: () => 4321 });
			r.create(input("p1"));
			expect(r.get(4321)?.release_requested_by).toBeNull();
			expect(r.get(4321)?.pushed_at).toBeNull();
		});
	});
});
