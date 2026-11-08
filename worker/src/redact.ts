const REDACTED = "[redacted]";

// よく知られた形だけを拾う。形の決まっていない秘密（パスワード、独自の API キー）は素通りする
const PATTERNS: RegExp[] = [
	// 途中で切れた記録では END が無いので、そのときは末尾まで伏せる
	/-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)/g,
	/\bgithub_pat_[A-Za-z0-9_]{20,}/g,
	/\bgh[pousr]_[A-Za-z0-9]{20,}/g,
	/\bops_[A-Za-z0-9_-]{20,}/g,
	/\bsk-[A-Za-z0-9_-]{20,}/g,
	/\bxox[abposr]-[A-Za-z0-9-]{10,}/g,
	/\b(?:AKIA|ASIA)[0-9A-Z]{16}\b/g,
	/\bAIza[0-9A-Za-z_-]{35}\b/g,
	/\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}/g,
];

// 値の形は決まっていないが、名前で秘密と分かるもの
const NAMED =
	/((?:CF-Access-Client-Secret|Authorization|aws_secret_access_key|AWS_SECRET_ACCESS_KEY)["']?\s*[:=]\s*["']?)(?:Bearer\s+|Basic\s+)?[^\s"',]+/gi;

export const redact = (text: string): string => {
	let out = text;
	for (const pattern of PATTERNS) out = out.replace(pattern, REDACTED);
	return out.replace(NAMED, (_m, name: string) => `${name}${REDACTED}`);
};
