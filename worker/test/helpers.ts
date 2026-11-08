import { runInDurableObject } from "cloudflare:test";
import { env, exports } from "cloudflare:workers";
import { registryOf } from "../src/index.ts";
import { createRegistry, type RegistryOptions } from "../src/registry.ts";

export const BASE = "https://hishaku.example.com";

let seq = 0;
/** テストごとに重ならない架空の投稿 ID */
export const postId = () => `post-${Date.now()}-${++seq}`;

export const call = (path: string, init?: RequestInit) =>
	exports.default.fetch(new Request(`${BASE}${path}`, init));

export const send = (method: string, path: string, body?: unknown) =>
	call(path, {
		method,
		headers: { "Content-Type": "application/json" },
		body: body === undefined ? undefined : JSON.stringify(body),
	});

export const createSession = async (
	input: Record<string, unknown> = {},
): Promise<{ number: number; post_id: string }> => {
	const res = await send("POST", "/v1/sessions", {
		post_id: postId(),
		agent: "claude",
		title: "テストの件名",
		...input,
	});
	if (res.status !== 201) throw new Error(`create failed: ${res.status}`);
	return res.json();
};

/** 本番と同じ台帳の Durable Object の SQL を直接触る */
export const onRegistry = <R>(fn: (sql: SqlStorage) => R | Promise<R>) =>
	runInDurableObject(registryOf(env), (_instance, state) =>
		fn(state.storage.sql),
	);

/** 時計と乱数を差し替えた台帳を、他のテストと分けた Durable Object の SQL に作る */
export const isolatedRegistry = (options: RegistryOptions) => {
	const stub = env.SESSION_REGISTRY.get(
		env.SESSION_REGISTRY.idFromName(`unit-${postId()}`),
	);
	return <R>(fn: (registry: ReturnType<typeof createRegistry>) => R) =>
		runInDurableObject(stub, (_instance, state) =>
			fn(createRegistry(state.storage.sql, options)),
		);
};
