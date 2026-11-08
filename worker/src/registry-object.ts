import { DurableObject } from "cloudflare:workers";
import { handleApi } from "./api.ts";
import { handleMcp } from "./mcp.ts";
import { createRegistry, type Registry } from "./registry.ts";
import { deleteSessionObjects } from "./transcripts.ts";

/**
 * 番号の台帳を 1 つだけ持つ Durable Object。貸し出しの排他は、ここが要求を 1 つずつ受けることで守る。
 * KV は書き込みの行き渡りが遅く、2 台が同時に借りられてしまうので使わない。
 */
export class SessionRegistry extends DurableObject<Env> {
	private readonly registry: Registry;
	private readonly queues = new Map<number, Promise<unknown>>();

	constructor(ctx: DurableObjectState, env: Env) {
		super(ctx, env);
		this.registry = createRegistry(ctx.storage.sql);
	}

	override async fetch(request: Request): Promise<Response> {
		if (new URL(request.url).pathname === "/mcp") {
			return handleMcp(request, {
				registry: this.registry,
				bucket: this.env.TRANSCRIPTS,
			});
		}
		return handleApi(request, {
			registry: this.registry,
			bucket: this.env.TRANSCRIPTS,
			serialize: (number, fn) => this.serialize(number, fn),
			afterWrite: () => this.scheduleCleanup(),
			now: Date.now,
		});
	}

	override async alarm(): Promise<void> {
		for (const number of this.registry.expired()) {
			// R2 を先に消す。途中で失敗しても行が残るので、alarm の再試行でまた消しにいける
			await deleteSessionObjects(this.env.TRANSCRIPTS, number);
			this.registry.remove(number);
		}
		await this.scheduleCleanup();
	}

	// R2 への書き込みを待つ間は Durable Object の入力ゲートが開くので、その間に takeover が割り込んで
	// 取られた側の記録が正本に入らないよう、番号ごとに順番待ちにする
	private serialize<T>(number: number, fn: () => Promise<T>): Promise<T> {
		const previous = this.queues.get(number) ?? Promise.resolve();
		const next = previous.then(fn, fn);
		const settled = next.catch(() => undefined);
		this.queues.set(number, settled);
		void settled.then(() => {
			if (this.queues.get(number) === settled) this.queues.delete(number);
		});
		return next;
	}

	private async scheduleCleanup(): Promise<void> {
		const next = this.registry.nextExpiry();
		if (next === null) {
			await this.ctx.storage.deleteAlarm();
			return;
		}
		if ((await this.ctx.storage.getAlarm()) !== next) {
			await this.ctx.storage.setAlarm(next);
		}
	}
}
