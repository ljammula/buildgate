// fill_responses_output.mjs <upstream base URL> <env name> -- <command> [args...]
//
// Runs <command> with <env name> set to a loopback proxy for <upstream base
// URL>. The proxy forwards every request unchanged and repairs one thing in
// an OpenAI Responses event stream: a terminal event whose response.output
// is empty gets the items the stream delivered as response.output_item.done.
//
// The ChatGPT Codex backend sends a response's items only as stream events
// and leaves the terminal event's output empty; the Copilot CLI reads a
// turn's tool calls and text from that output, so without this it ends every
// turn having seen nothing.
//
// This runs inside the worker, beside the agent. It holds no credential: the
// agent's headers carry the route's placeholders, which pass through here to
// the sandbox's supervisor like any other request of the worker's.

import http from "node:http";
import https from "node:https";
import { spawn } from "node:child_process";

const TERMINAL_EVENTS = new Set(["response.completed", "response.incomplete"]);
// Headers that describe one hop's connection, not the message. A reply's
// length and framing are this proxy's own, since it may rewrite the body.
const REQUEST_HOP_HEADERS = new Set(["connection", "host", "keep-alive"]);
const REPLY_HOP_HEADERS = new Set(["connection", "content-length", "keep-alive", "transfer-encoding"]);
const EVENT_END = /\r?\n\r?\n/;

// OutputFiller rewrites an event stream fed to it in arbitrary pieces.
class OutputFiller {
	constructor() {
		this.pending = "";
		this.items = [];
	}

	// push returns the events completed by text, repaired where needed.
	push(text) {
		this.pending += text;
		let out = "";
		for (;;) {
			const end = EVENT_END.exec(this.pending);
			if (end === null) {
				return out;
			}
			out += this.event(this.pending.slice(0, end.index)) + end[0];
			this.pending = this.pending.slice(end.index + end[0].length);
		}
	}

	// flush returns what push held back: a last event the stream ended
	// without a blank line after.
	flush() {
		const rest = this.pending;
		this.pending = "";
		return this.event(rest);
	}

	event(raw) {
		const lines = raw.split(/\r?\n/);
		const isData = (line) => line.startsWith("data:");
		let data;
		try {
			data = JSON.parse(lines.filter(isData).map((line) => line.slice(5).replace(/^ /, "")).join("\n"));
		} catch {
			return raw;
		}
		if (data === null || typeof data !== "object") {
			return raw;
		}
		if (data.type === "response.output_item.done" && Number.isInteger(data.output_index) && data.output_index >= 0) {
			this.items[data.output_index] = data.item;
			return raw;
		}
		const output = data.response?.output;
		const items = this.items.filter((item) => item !== undefined);
		if (!TERMINAL_EVENTS.has(data.type) || !Array.isArray(output) || output.length > 0 || items.length === 0) {
			return raw;
		}
		data.response.output = items;
		const firstData = lines.findIndex(isData);
		const kept = lines.filter((line, index) => !isData(line) || index === firstData);
		return kept.map((line) => (isData(line) ? "data: " + JSON.stringify(data) : line)).join("\n");
	}
}

function without(headers, dropped) {
	return Object.fromEntries(Object.entries(headers).filter(([name]) => !dropped.has(name)));
}

// forward sends req to the upstream and streams the reply into res. A
// failure before the reply's head is a 502; one after it cuts the reply
// short, so the client sees a broken stream and not a complete one.
function forward(upstream, req, res) {
	const target = new URL(upstream + req.url);
	const send = target.protocol === "https:" ? https.request : http.request;
	// The stream is read as text here, so it must not arrive compressed.
	const headers = { ...without(req.headers, REQUEST_HOP_HEADERS), "accept-encoding": "identity" };
	const out = send(target, { method: req.method, headers }, (reply) => {
		res.writeHead(reply.statusCode, without(reply.headers, REPLY_HOP_HEADERS));
		reply.on("error", () => res.destroy());
		// The ChatGPT Codex backend sends its event stream with no content
		// type, so only a reply that names another type is passed unread.
		const contentType = reply.headers["content-type"];
		if (contentType !== undefined && !contentType.includes("text/event-stream")) {
			reply.pipe(res);
			return;
		}
		const filler = new OutputFiller();
		const decoder = new TextDecoder();
		reply.on("data", (chunk) => res.write(filler.push(decoder.decode(chunk, { stream: true }))));
		reply.on("end", () => res.end(filler.push(decoder.decode()) + filler.flush()));
	});
	out.on("error", (err) => {
		if (res.headersSent) {
			res.destroy();
			return;
		}
		res.writeHead(502, { "content-type": "text/plain" });
		res.end(`fill_responses_output: ${err.message}\n`);
	});
	// A client that goes away stops the model call it no longer reads.
	res.on("close", () => out.destroy());
	req.pipe(out);
}

function main() {
	const [upstreamArg, envName, separator, command, ...args] = process.argv.slice(2);
	if (!upstreamArg || !envName || separator !== "--" || !command) {
		console.error("usage: fill_responses_output.mjs <upstream base URL> <env name> -- <command> [args...]");
		process.exit(2);
	}
	const upstream = upstreamArg.replace(/\/+$/, "");
	const server = http.createServer((req, res) => forward(upstream, req, res));
	server.listen(0, "127.0.0.1", () => {
		const child = spawn(command, args, {
			stdio: "inherit",
			env: { ...process.env, [envName]: `http://127.0.0.1:${server.address().port}` },
		});
		for (const signal of ["SIGINT", "SIGTERM"]) {
			process.on(signal, () => child.kill(signal));
		}
		child.on("error", (err) => {
			console.error(`fill_responses_output: ${command}: ${err.message}`);
			process.exit(127);
		});
		child.on("exit", (code, signal) => {
			process.exit(code ?? (signal ? 1 : 0));
		});
	});
}

main();
