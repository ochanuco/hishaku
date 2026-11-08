import * as v from "valibot";
import {
	AGENTS,
	type LeaseFree,
	type LeaseHeld,
	type LeaseMismatch,
	type NotFound,
	NUMBER_MAX,
	NUMBER_MIN,
	type Registry,
} from "./registry.ts";
import { freeForkKey, transcriptKey } from "./transcripts.ts";

export const GENERATION_HEADER = "X-Hishaku-Generation";
export const MAX_LEASE_TTL_SECONDS = 24 * 60 * 60;

export type ApiDeps = {
	registry: Registry;
	bucket: R2Bucket;
	/** 同じ番号の貸し出しの変更と記録の書き込みを 1 つずつ通す */
	serialize: <T>(number: number, fn: () => Promise<T>) => Promise<T>;
	/** 書き込みの後に、期限切れの掃除の alarm を張り直す */
	afterWrite: () => Promise<void>;
	now: () => number;
};

const Holder = v.pipe(v.string(), v.minLength(1), v.maxLength(100));
const Generation = v.pipe(v.number(), v.integer(), v.minValue(1));
const TtlSeconds = v.pipe(
	v.number(),
	v.integer(),
	v.minValue(1),
	v.maxValue(MAX_LEASE_TTL_SECONDS),
);
const Title = v.pipe(v.string(), v.minLength(1), v.maxLength(200));
const Repo = v.nullable(v.pipe(v.string(), v.minLength(1), v.maxLength(200)));
const Branch = v.nullable(v.pipe(v.string(), v.minLength(1), v.maxLength(255)));
const SessionId = v.nullable(
	v.pipe(v.string(), v.minLength(1), v.maxLength(200)),
);
const PostId = v.pipe(v.string(), v.minLength(1), v.maxLength(100));

const CreateBody = v.object({
	post_id: PostId,
	agent: v.picklist(AGENTS),
	title: Title,
	repo: v.optional(Repo),
	branch: v.optional(Branch),
});

const PatchBody = v.pipe(
	v.object({
		title: v.optional(Title),
		repo: v.optional(Repo),
		branch: v.optional(Branch),
		session_id: v.optional(SessionId),
	}),
	v.check(
		(body) => Object.values(body).some((value) => value !== undefined),
		"更新する項目が 1 つも無い",
	),
);

const LeaseBody = v.object({
	holder: Holder,
	mode: v.picklist(["acquire", "takeover"]),
	ttl_seconds: TtlSeconds,
});

const RenewBody = v.object({
	holder: Holder,
	generation: Generation,
	ttl_seconds: v.optional(TtlSeconds),
});

const ReleaseBody = v.object({ holder: Holder, generation: Generation });

const RequestBody = v.object({ holder: Holder });

const FenceQuery = v.object({
	holder: Holder,
	generation: v.pipe(
		v.string(),
		v.regex(/^[1-9]\d*$/),
		v.transform(Number),
		Generation,
	),
});

const json = (status: number, body: unknown, headers?: HeadersInit) =>
	Response.json(body, { status, headers });

const notFound = () => json(404, { error: "not_found" });

const invalid = (issues: v.BaseIssue<unknown>[]) =>
	json(400, {
		error: "invalid_request",
		issues: issues.map((i) => ({
			path: v.getDotPath(i),
			message: i.message,
		})),
	});

const failure = (error: NotFound | LeaseHeld | LeaseFree | LeaseMismatch) => {
	switch (error.kind) {
		case "not_found":
			return notFound();
		case "lease_free":
			return json(409, { error: "lease_free" });
		case "lease_held":
			return json(409, {
				error: "lease_held",
				holder: error.holder,
				lease_expires_at: error.lease_expires_at,
			});
		case "lease_mismatch":
			return json(409, {
				error: "lease_mismatch",
				holder: error.holder,
				generation: error.generation,
				lease_expires_at: error.lease_expires_at,
			});
	}
};

const readJson = async <T extends v.GenericSchema>(
	request: Request,
	schema: T,
): Promise<
	{ ok: true; value: v.InferOutput<T> } | { ok: false; response: Response }
> => {
	let body: unknown;
	try {
		body = await request.json();
	} catch {
		return {
			ok: false,
			response: json(400, {
				error: "invalid_request",
				issues: [{ path: null, message: "JSON として読めない" }],
			}),
		};
	}
	const parsed = v.safeParse(schema, body);
	if (!parsed.success) return { ok: false, response: invalid(parsed.issues) };
	return { ok: true, value: parsed.output };
};

const parseNumber = (segment: string | undefined): number | null => {
	if (!segment || !/^\d{4}$/.test(segment)) return null;
	const n = Number(segment);
	return n >= NUMBER_MIN && n <= NUMBER_MAX ? n : null;
};

// R2 は長さの分からない stream を受けないので、記録の本文には Content-Length を求める
const hasLength = (request: Request) =>
	request.body !== null && request.headers.get("content-length") !== null;

const lengthRequired = () =>
	json(411, {
		error: "length_required",
		message: "Content-Length 付きの本文が要る",
	});

const NDJSON = "application/x-ndjson";

export const handleApi = async (
	request: Request,
	deps: ApiDeps,
): Promise<Response> => {
	const url = new URL(request.url);
	const segments = url.pathname.split("/").filter(Boolean);
	if (segments[0] !== "v1" || segments[1] !== "sessions") return notFound();
	const { registry } = deps;
	const method = request.method;

	if (segments.length === 2) {
		if (method === "POST") {
			const body = await readJson(request, CreateBody);
			if (!body.ok) return body.response;
			const created = registry.create(body.value);
			if (!created.ok) {
				return json(503, {
					error: "exhausted",
					message: "空いている番号が無い",
				});
			}
			if (created.value.created) await deps.afterWrite();
			return json(created.value.created ? 201 : 200, created.value.meta);
		}
		if (method === "GET") {
			const postId = url.searchParams.get("post_id");
			if (!postId) {
				return json(400, {
					error: "invalid_request",
					issues: [{ path: "post_id", message: "post_id が要る" }],
				});
			}
			const meta = registry.findByPostId(postId);
			return meta ? json(200, meta) : notFound();
		}
		return methodNotAllowed();
	}

	const number = parseNumber(segments[2]);
	if (number === null) return notFound();
	const rest = segments.slice(3).join("/");

	switch (`${method} ${rest}`) {
		case "GET ": {
			const meta = registry.get(number);
			return meta ? json(200, meta) : notFound();
		}
		case "PATCH ": {
			const body = await readJson(request, PatchBody);
			if (!body.ok) return body.response;
			const updated = registry.update(number, body.value);
			if (!updated.ok) return failure(updated.error);
			await deps.afterWrite();
			return json(200, updated.value);
		}
		case "GET transcript": {
			const meta = registry.get(number);
			if (!meta) return notFound();
			const object = await deps.bucket.get(transcriptKey(number));
			if (!object) return json(404, { error: "no_transcript" });
			return new Response(object.body, {
				headers: {
					"Content-Type": NDJSON,
					[GENERATION_HEADER]: String(meta.generation),
				},
			});
		}
		case "PUT transcript": {
			const query = v.safeParse(
				FenceQuery,
				Object.fromEntries(url.searchParams),
			);
			if (!query.success) return invalid(query.issues);
			if (!hasLength(request)) return lengthRequired();
			return deps.serialize(number, async () => {
				const checked = registry.checkLease(
					number,
					query.output.holder,
					query.output.generation,
				);
				if (!checked.ok) return failure(checked.error);
				await deps.bucket.put(transcriptKey(number), request.body, {
					httpMetadata: { contentType: NDJSON },
					customMetadata: {
						holder: query.output.holder,
						generation: String(query.output.generation),
					},
				});
				registry.markPushed(number);
				await deps.afterWrite();
				return new Response(null, {
					status: 204,
					headers: { [GENERATION_HEADER]: String(query.output.generation) },
				});
			});
		}
		case "POST forks": {
			if (!hasLength(request)) return lengthRequired();
			const holder = url.searchParams.get("holder");
			return deps.serialize(number, async () => {
				if (!registry.get(number)) return notFound();
				const key = await freeForkKey(deps.bucket, number, deps.now());
				await deps.bucket.put(key, request.body, {
					httpMetadata: { contentType: NDJSON },
					customMetadata: holder ? { holder } : undefined,
				});
				registry.touch(number);
				await deps.afterWrite();
				return json(201, { key });
			});
		}
		case "POST lease": {
			const body = await readJson(request, LeaseBody);
			if (!body.ok) return body.response;
			return deps.serialize(number, async () => {
				const leased = registry.lease(number, body.value);
				if (!leased.ok) return failure(leased.error);
				await deps.afterWrite();
				return json(200, leased.value);
			});
		}
		case "POST lease/renew": {
			const body = await readJson(request, RenewBody);
			if (!body.ok) return body.response;
			return deps.serialize(number, async () => {
				const renewed = registry.renew(number, body.value);
				if (!renewed.ok) return failure(renewed.error);
				await deps.afterWrite();
				return json(200, renewed.value);
			});
		}
		case "POST lease/request": {
			const body = await readJson(request, RequestBody);
			if (!body.ok) return body.response;
			return deps.serialize(number, async () => {
				const requested = registry.requestRelease(number, body.value.holder);
				if (!requested.ok) return failure(requested.error);
				return json(200, requested.value);
			});
		}
		case "DELETE lease": {
			const body = await readJson(request, ReleaseBody);
			if (!body.ok) return body.response;
			return deps.serialize(number, async () => {
				const released = registry.release(number, body.value);
				if (!released.ok) return failure(released.error);
				await deps.afterWrite();
				return new Response(null, { status: 204 });
			});
		}
	}
	const known = [
		"",
		"transcript",
		"forks",
		"lease",
		"lease/renew",
		"lease/request",
	];
	return known.includes(rest) ? methodNotAllowed() : notFound();
};

const methodNotAllowed = () => json(405, { error: "method_not_allowed" });
