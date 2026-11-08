export { SessionRegistry } from "./registry-object.ts";

/** 台帳は 1 つだけ持つ */
export const registryOf = (env: Env) =>
	env.SESSION_REGISTRY.get(env.SESSION_REGISTRY.idFromName("registry"));

export default {
	async fetch(request, env): Promise<Response> {
		const { pathname } = new URL(request.url);
		if (request.method === "GET" && pathname === "/health") {
			return Response.json({ ok: true });
		}
		if (
			pathname === "/v1" ||
			pathname.startsWith("/v1/") ||
			pathname === "/mcp"
		) {
			return registryOf(env).fetch(request);
		}
		return Response.json({ error: "not_found" }, { status: 404 });
	},
} satisfies ExportedHandler<Env>;
