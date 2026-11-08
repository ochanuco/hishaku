export const sessionPrefix = (number: number) => `sessions/${number}/`;

export const transcriptKey = (number: number) =>
	`${sessionPrefix(number)}transcript.jsonl`;

const pad = (n: number) => String(n).padStart(2, "0");

/** UTC の yyyymmdd-hhmmss */
export const forkStamp = (ms: number) => {
	const d = new Date(ms);
	return `${d.getUTCFullYear()}${pad(d.getUTCMonth() + 1)}${pad(d.getUTCDate())}-${pad(d.getUTCHours())}${pad(d.getUTCMinutes())}${pad(d.getUTCSeconds())}`;
};

/** 同じ秒に 2 つ預けられても上書きしないよう、空いている名前を探す */
export const freeForkKey = async (
	bucket: R2Bucket,
	number: number,
	ms: number,
): Promise<string> => {
	const base = `${sessionPrefix(number)}forks/${forkStamp(ms)}`;
	for (let i = 1; ; i++) {
		const key = i === 1 ? `${base}.jsonl` : `${base}-${i}.jsonl`;
		if ((await bucket.head(key)) === null) return key;
	}
};

/** 番号の下にある記録（正本と枝）をすべて消す */
export const deleteSessionObjects = async (
	bucket: R2Bucket,
	number: number,
): Promise<void> => {
	const prefix = sessionPrefix(number);
	let cursor: string | undefined;
	do {
		const listed = await bucket.list({ prefix, cursor });
		const keys = listed.objects.map((o) => o.key);
		if (keys.length > 0) await bucket.delete(keys);
		cursor = listed.truncated ? listed.cursor : undefined;
	} while (cursor);
};
