import { err, ok, type Result } from "./result.ts";

export const AGENTS = ["claude", "codex"] as const;
export type Agent = (typeof AGENTS)[number];

export const NUMBER_MIN = 1000;
export const NUMBER_MAX = 9999;
export const SESSION_TTL_MS = 30 * 24 * 60 * 60 * 1000;

export type SessionMeta = {
	number: number;
	post_id: string;
	agent: Agent;
	session_id: string | null;
	title: string;
	repo: string | null;
	branch: string | null;
	holder: string | null;
	generation: number;
	lease_expires_at: string | null;
	/** 今の持ち主に返してほしいと頼んだ Commander。貸し出しが変わると null に戻る */
	release_requested_by: string | null;
	/** 最後に記録を PUT した時刻。まだ PUT が無ければ null */
	pushed_at: string | null;
	expires_at: string;
	updated_at: string;
};

export type Lease = {
	generation: number;
	lease_expires_at: string;
	release_requested_by: string | null;
};

export type ReleaseRequest = {
	holder: string;
	generation: number;
	lease_expires_at: string;
	release_requested_by: string;
};

export type CreateInput = {
	post_id: string;
	agent: Agent;
	title: string;
	repo?: string | null;
	branch?: string | null;
};

export type PatchInput = {
	title?: string;
	repo?: string | null;
	branch?: string | null;
	session_id?: string | null;
};

export type LeaseInput = {
	holder: string;
	mode: "acquire" | "takeover";
	ttl_seconds: number;
};

export type FencedInput = {
	holder: string;
	generation: number;
	ttl_seconds?: number;
};

export type NotFound = { kind: "not_found" };
export type LeaseHeld = {
	kind: "lease_held";
	holder: string;
	lease_expires_at: string;
};
export type LeaseMismatch = {
	kind: "lease_mismatch";
	holder: string | null;
	generation: number;
	lease_expires_at: string | null;
};
export type LeaseFree = { kind: "lease_free" };
export type Exhausted = { kind: "exhausted" };

type Row = {
	number: number;
	post_id: string;
	agent: Agent;
	session_id: string | null;
	title: string;
	repo: string | null;
	branch: string | null;
	holder: string | null;
	generation: number;
	lease_expires_at: number | null;
	lease_ttl_seconds: number | null;
	release_requested_by: string | null;
	pushed_at: number | null;
	expires_at: number;
	updated_at: number;
};

export type RegistryOptions = {
	now?: () => number;
	/** NUMBER_MIN〜NUMBER_MAX の整数を返す */
	randomNumber?: () => number;
};

const SCHEMA = `
CREATE TABLE IF NOT EXISTS sessions (
	number INTEGER PRIMARY KEY,
	post_id TEXT NOT NULL,
	agent TEXT NOT NULL,
	session_id TEXT,
	title TEXT NOT NULL,
	repo TEXT,
	branch TEXT,
	holder TEXT,
	generation INTEGER NOT NULL DEFAULT 0,
	lease_expires_at INTEGER,
	lease_ttl_seconds INTEGER,
	release_requested_by TEXT,
	pushed_at INTEGER,
	expires_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_post_id ON sessions (post_id);
CREATE INDEX IF NOT EXISTS sessions_expires_at ON sessions (expires_at);
`;

const iso = (ms: number) => new Date(ms).toISOString();

const toMeta = (row: Row): SessionMeta => ({
	number: row.number,
	post_id: row.post_id,
	agent: row.agent,
	session_id: row.session_id,
	title: row.title,
	repo: row.repo,
	branch: row.branch,
	holder: row.holder,
	generation: row.generation,
	lease_expires_at:
		row.lease_expires_at === null ? null : iso(row.lease_expires_at),
	release_requested_by: row.release_requested_by,
	pushed_at: row.pushed_at === null ? null : iso(row.pushed_at),
	expires_at: iso(row.expires_at),
	updated_at: iso(row.updated_at),
});

const defaultRandomNumber = () =>
	NUMBER_MIN + Math.floor(Math.random() * (NUMBER_MAX - NUMBER_MIN + 1));

/** 番号・meta・貸し出し・世代を SQLite の表 1 枚で持つ。呼び出しはすべて同期で、Durable Object の中で 1 つずつ走る前提。 */
export const createRegistry = (
	sql: SqlStorage,
	options: RegistryOptions = {},
) => {
	const now = options.now ?? Date.now;
	const randomNumber = options.randomNumber ?? defaultRandomNumber;
	sql.exec(SCHEMA);
	// 列を足す前に作られた表には無いので足す
	const columns = new Set(
		sql
			.exec<{ name: string }>("PRAGMA table_info(sessions)")
			.toArray()
			.map((c) => c.name),
	);
	for (const [name, type] of [
		["release_requested_by", "TEXT"],
		["pushed_at", "INTEGER"],
	] as const) {
		if (!columns.has(name)) {
			sql.exec(`ALTER TABLE sessions ADD COLUMN ${name} ${type}`);
		}
	}

	const live = (number: number): Row | null => {
		const row = sql
			.exec<Row>(
				"SELECT * FROM sessions WHERE number = ? AND expires_at > ?",
				number,
				now(),
			)
			.toArray()[0];
		return row ?? null;
	};

	const touch = (number: number, at: number) => {
		sql.exec(
			"UPDATE sessions SET updated_at = ?, expires_at = ? WHERE number = ?",
			at,
			at + SESSION_TTL_MS,
			number,
		);
	};

	const pickNumber = (): number | null => {
		// 期限切れで掃除待ちの行も避ける。掃除の前に同じ番号を渡すと、R2 に残った前の記録を新しい番号が引き継いでしまう
		const taken = new Set(
			sql
				.exec<{ number: number }>("SELECT number FROM sessions")
				.toArray()
				.map((r) => r.number),
		);
		const size = NUMBER_MAX - NUMBER_MIN + 1;
		if (taken.size >= size) return null;
		for (let i = 0; i < 32; i++) {
			const n = randomNumber();
			if (!taken.has(n)) return n;
		}
		// 埋まりが多いと乱数の引き直しが終わらないので、空きの一覧から選ぶ
		const free: number[] = [];
		for (let n = NUMBER_MIN; n <= NUMBER_MAX; n++) {
			if (!taken.has(n)) free.push(n);
		}
		return free[Math.floor(Math.random() * free.length)] ?? null;
	};

	const mismatch = (row: Row): LeaseMismatch => ({
		kind: "lease_mismatch",
		holder: row.holder,
		generation: row.generation,
		lease_expires_at:
			row.lease_expires_at === null ? null : iso(row.lease_expires_at),
	});

	// 世代が合えば、貸し出しの期限が過ぎていても誰にも取られていない。持ち主と世代の一致だけで通す
	const fenced = (
		number: number,
		holder: string,
		generation: number,
	): Result<Row, NotFound | LeaseMismatch> => {
		const row = live(number);
		if (!row) return err({ kind: "not_found" });
		if (row.holder !== holder || row.generation !== generation) {
			return err(mismatch(row));
		}
		return ok(row);
	};

	return {
		create(
			input: CreateInput,
		): Result<{ meta: SessionMeta; created: boolean }, Exhausted> {
			const at = now();
			const existing = sql
				.exec<Row>(
					"SELECT * FROM sessions WHERE post_id = ? AND expires_at > ? LIMIT 1",
					input.post_id,
					at,
				)
				.toArray()[0];
			if (existing) return ok({ meta: toMeta(existing), created: false });
			const number = pickNumber();
			if (number === null) return err({ kind: "exhausted" });
			sql.exec(
				`INSERT INTO sessions (number, post_id, agent, title, repo, branch, generation, expires_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`,
				number,
				input.post_id,
				input.agent,
				input.title,
				input.repo ?? null,
				input.branch ?? null,
				at + SESSION_TTL_MS,
				at,
			);
			const row = live(number);
			if (!row) throw new Error("inserted session is missing");
			return ok({ meta: toMeta(row), created: true });
		},

		get(number: number): SessionMeta | null {
			const row = live(number);
			return row ? toMeta(row) : null;
		},

		findByPostId(postId: string): SessionMeta | null {
			const row = sql
				.exec<Row>(
					"SELECT * FROM sessions WHERE post_id = ? AND expires_at > ? LIMIT 1",
					postId,
					now(),
				)
				.toArray()[0];
			return row ? toMeta(row) : null;
		},

		update(number: number, patch: PatchInput): Result<SessionMeta, NotFound> {
			const row = live(number);
			if (!row) return err({ kind: "not_found" });
			const next = {
				title: patch.title ?? row.title,
				repo: patch.repo !== undefined ? patch.repo : row.repo,
				branch: patch.branch !== undefined ? patch.branch : row.branch,
				session_id:
					patch.session_id !== undefined ? patch.session_id : row.session_id,
			};
			sql.exec(
				"UPDATE sessions SET title = ?, repo = ?, branch = ?, session_id = ? WHERE number = ?",
				next.title,
				next.repo,
				next.branch,
				next.session_id,
				number,
			);
			touch(number, now());
			const updated = live(number);
			if (!updated) throw new Error("updated session is missing");
			return ok(toMeta(updated));
		},

		lease(
			number: number,
			input: LeaseInput,
		): Result<Lease, NotFound | LeaseHeld> {
			const at = now();
			const row = live(number);
			if (!row) return err({ kind: "not_found" });
			const held =
				row.holder !== null &&
				row.lease_expires_at !== null &&
				row.lease_expires_at > at;
			if (input.mode === "acquire" && held) {
				return err({
					kind: "lease_held",
					holder: row.holder as string,
					lease_expires_at: iso(row.lease_expires_at as number),
				});
			}
			const generation = row.generation + 1;
			const expiresAt = at + input.ttl_seconds * 1000;
			sql.exec(
				"UPDATE sessions SET holder = ?, generation = ?, lease_expires_at = ?, lease_ttl_seconds = ?, release_requested_by = NULL WHERE number = ?",
				input.holder,
				generation,
				expiresAt,
				input.ttl_seconds,
				number,
			);
			touch(number, at);
			return ok({
				generation,
				lease_expires_at: iso(expiresAt),
				release_requested_by: null,
			});
		},

		/** 今の持ち主に返してほしいと頼む。持ち主は renew の応答でそれを知り、区切りで release する */
		requestRelease(
			number: number,
			requester: string,
		): Result<ReleaseRequest, NotFound | LeaseFree | LeaseMismatch> {
			const at = now();
			const row = live(number);
			if (!row) return err({ kind: "not_found" });
			if (
				row.holder === null ||
				row.lease_expires_at === null ||
				row.lease_expires_at <= at
			) {
				return err({ kind: "lease_free" });
			}
			if (row.holder === requester) return err(mismatch(row));
			sql.exec(
				"UPDATE sessions SET release_requested_by = ? WHERE number = ?",
				requester,
				number,
			);
			return ok({
				holder: row.holder,
				generation: row.generation,
				lease_expires_at: iso(row.lease_expires_at),
				release_requested_by: requester,
			});
		},

		renew(
			number: number,
			input: FencedInput,
		): Result<Lease, NotFound | LeaseMismatch> {
			const checked = fenced(number, input.holder, input.generation);
			if (!checked.ok) return checked;
			const at = now();
			const ttl = input.ttl_seconds ?? checked.value.lease_ttl_seconds ?? 0;
			const expiresAt = at + ttl * 1000;
			sql.exec(
				"UPDATE sessions SET lease_expires_at = ?, lease_ttl_seconds = ? WHERE number = ?",
				expiresAt,
				ttl,
				number,
			);
			touch(number, at);
			return ok({
				generation: input.generation,
				lease_expires_at: iso(expiresAt),
				release_requested_by: checked.value.release_requested_by,
			});
		},

		release(
			number: number,
			input: FencedInput,
		): Result<null, NotFound | LeaseMismatch> {
			const checked = fenced(number, input.holder, input.generation);
			if (!checked.ok) return checked;
			sql.exec(
				"UPDATE sessions SET holder = NULL, lease_expires_at = NULL, lease_ttl_seconds = NULL, release_requested_by = NULL WHERE number = ?",
				number,
			);
			touch(number, now());
			return ok(null);
		},

		/** 記録の置き換えの前に、今の貸し出しと合うかを確かめる */
		checkLease(
			number: number,
			holder: string,
			generation: number,
		): Result<SessionMeta, NotFound | LeaseMismatch> {
			const checked = fenced(number, holder, generation);
			return checked.ok ? ok(toMeta(checked.value)) : checked;
		},

		/** 記録の書き込みも更新として期限を延ばす */
		touch(number: number): void {
			touch(number, now());
		},

		/** 正本の記録を置き換えた時刻を残す。forks は正本ではないので呼ばない */
		markPushed(number: number): void {
			const at = now();
			sql.exec(
				"UPDATE sessions SET pushed_at = ? WHERE number = ?",
				at,
				number,
			);
			touch(number, at);
		},

		expired(): number[] {
			return sql
				.exec<{ number: number }>(
					"SELECT number FROM sessions WHERE expires_at <= ? ORDER BY number",
					now(),
				)
				.toArray()
				.map((r) => r.number);
		},

		remove(number: number): void {
			sql.exec("DELETE FROM sessions WHERE number = ?", number);
		},

		/** 次に期限が来る時刻。行が無ければ null */
		nextExpiry(): number | null {
			const row = sql
				.exec<{ at: number | null }>(
					"SELECT MIN(expires_at) AS at FROM sessions",
				)
				.toArray()[0];
			return row?.at ?? null;
		},
	};
};

export type Registry = ReturnType<typeof createRegistry>;
