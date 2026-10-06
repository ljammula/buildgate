"""fill_responses_output.mjs end to end: a real node process proxying a local
upstream that replays the ChatGPT Codex stream recorded for the meter
(internal/meter/testdata/chatgpt_codex_function_call.sse), whose terminal
event has an empty output."""

import json
import shutil
import subprocess
import sys
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
PROXY = REPO / "agent" / "pi" / "scripts" / "fill_responses_output.mjs"
RECORDED = (REPO / "internal" / "meter" / "testdata" / "chatgpt_codex_function_call.sse").read_bytes()
URL_ENV = "TEST_BASE_URL"

# Posts one request to the proxy and writes the status line and raw body out.
CLIENT = """
import os, sys, urllib.request, urllib.error
req = urllib.request.Request(os.environ["%s"] + "/responses", data=b'{"stream":true}', method="POST",
	headers={"authorization": "Bearer placeholder-token", "chatgpt-account-id": "placeholder-account", "content-type": "application/json"})
try:
	reply = urllib.request.urlopen(req)
except urllib.error.HTTPError as err:
	reply = err
sys.stdout.buffer.write(b"%%d\\n" %% reply.status + reply.read())
sys.exit(int(sys.argv[1]))
""" % URL_ENV


def events(body: bytes) -> list[tuple[str, dict]]:
	"""(event name, data) for each event of a stream."""
	out = []
	for block in body.replace(b"\r\n", b"\n").split(b"\n\n"):
		lines = block.decode().split("\n")
		data = [line[5:].lstrip(" ") for line in lines if line.startswith("data:")]
		if data:
			name = next((line[6:].strip() for line in lines if line.startswith("event:")), "")
			out.append((name, json.loads("\n".join(data))))
	return out


class Upstream:
	"""A local server that answers every request with one fixed reply, sent
	in small pieces, and keeps what it was asked."""

	def __init__(self, body: bytes, content_type="text/event-stream", status=200, piece=97, drop_after=None):
		self.requests: list[dict] = []
		upstream = self

		class Handler(BaseHTTPRequestHandler):
			def log_message(self, *args):
				pass

			def do_POST(self):
				sent = self.rfile.read(int(self.headers.get("content-length", "0")))
				upstream.requests.append({"path": self.path, "headers": {k.lower(): v for k, v in self.headers.items()}, "body": sent})
				self.send_response(status)
				if content_type is not None:
					self.send_header("content-type", content_type)
				sending = body
				if drop_after is not None:
					# Promise the whole body, send part, and hang up.
					self.send_header("content-length", str(len(body)))
					sending = body[:drop_after]
				self.end_headers()
				for start in range(0, len(sending), piece):
					self.wfile.write(sending[start:start + piece])
					self.wfile.flush()
					time.sleep(0.001)

		self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
		threading.Thread(target=self.server.serve_forever, daemon=True).start()
		self.url = f"http://127.0.0.1:{self.server.server_address[1]}/backend-api/codex"

	def close(self):
		self.server.shutdown()
		self.server.server_close()


@unittest.skipUnless(shutil.which("node"), "node is not installed")
class FillResponsesOutputTests(unittest.TestCase):
	def through_proxy(self, upstream_url: str, exit_code=0) -> tuple[int, int, bytes]:
		"""(process exit code, reply status, reply body) of one client request."""
		done = subprocess.run(
			["node", str(PROXY), upstream_url, URL_ENV, "--", sys.executable, "-c", CLIENT, str(exit_code)],
			capture_output=True, timeout=60,
		)
		status, _, body = done.stdout.partition(b"\n")
		return done.returncode, int(status or 0), body

	def serve(self, body: bytes, **kwargs) -> Upstream:
		upstream = Upstream(body, **kwargs)
		self.addCleanup(upstream.close)
		return upstream

	def test_recorded_stream_has_an_empty_final_output(self):
		# The premise of the proxy: if the recording changes, so must this file.
		self.assertEqual(events(RECORDED)[-1][0], "response.completed")
		self.assertEqual(events(RECORDED)[-1][1]["response"]["output"], [])

	def test_final_output_is_filled_with_the_streamed_items(self):
		code, status, body = self.through_proxy(self.serve(RECORDED).url)
		self.assertEqual((code, status), (0, 200))
		sent, got = events(RECORDED), events(body)
		self.assertEqual(got[:-1], sent[:-1])
		items = [data["item"] for name, data in sent if name == "response.output_item.done"]
		self.assertEqual(len(items), 1)
		self.assertEqual(items[0]["type"], "function_call")
		self.assertEqual(got[-1][0], "response.completed")
		self.assertEqual(got[-1][1]["response"]["output"], items)
		want = sent[-1][1]
		want["response"]["output"] = items
		self.assertEqual(got[-1][1], want)

	def test_events_before_the_final_one_are_byte_identical(self):
		_, _, body = self.through_proxy(self.serve(RECORDED).url)
		cut = RECORDED.rindex(b"event: response.completed")
		self.assertEqual(body[:cut], RECORDED[:cut])

	def test_stream_sent_with_no_content_type_is_filled(self):
		# What the ChatGPT Codex backend does (seen live 2026-10-05).
		_, status, body = self.through_proxy(self.serve(RECORDED, content_type=None).url)
		self.assertEqual(status, 200)
		self.assertEqual(len(events(body)[-1][1]["response"]["output"]), 1)

	def test_crlf_stream_is_filled_too(self):
		crlf = RECORDED.replace(b"\n", b"\r\n")
		_, _, body = self.through_proxy(self.serve(crlf).url)
		self.assertEqual(len(events(body)[-1][1]["response"]["output"]), 1)
		cut = crlf.rindex(b"event: response.completed")
		self.assertEqual(body[:cut], crlf[:cut])

	def test_stream_split_at_every_size_gives_the_same_result(self):
		want = self.through_proxy(self.serve(RECORDED, piece=len(RECORDED)).url)[2]
		for piece in (1, 2, 3, 5, 13, 64, 1000):
			with self.subTest(piece=piece):
				self.assertEqual(self.through_proxy(self.serve(RECORDED, piece=piece).url)[2], want)

	def test_final_output_already_present_is_left_alone(self):
		item = {"type": "message", "id": "kept"}
		filled = b"".join(
			f"event: {name}\ndata: {json.dumps(data)}\n\n".encode()
			for name, data in [
				("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": {"type": "message", "id": "streamed"}}),
				("response.completed", {"type": "response.completed", "response": {"output": [item]}}),
			]
		)
		_, _, body = self.through_proxy(self.serve(filled).url)
		self.assertEqual(body, filled)

	def test_stream_with_no_items_is_left_alone(self):
		bare = b'event: response.completed\ndata: {"type":"response.completed","response":{"output":[]}}\n\n'
		_, _, body = self.through_proxy(self.serve(bare).url)
		self.assertEqual(body, bare)

	def test_final_event_with_no_blank_line_after_is_filled(self):
		_, _, body = self.through_proxy(self.serve(RECORDED.rstrip(b"\n")).url)
		self.assertEqual(len(events(body)[-1][1]["response"]["output"]), 1)

	def test_upstream_that_hangs_up_mid_stream_breaks_the_reply(self):
		# The client must not be handed a stream that looks complete.
		cut = RECORDED.rindex(b"event: response.completed")
		code, status, body = self.through_proxy(self.serve(RECORDED, drop_after=cut).url)
		self.assertNotEqual(code, 0)
		self.assertNotIn(b"response.completed", body)
		self.assertNotIn(b"fill_responses_output", body)

	def test_other_reply_cut_short_breaks_the_reply_too(self):
		error = b'{"error":{"message":"' + b"x" * 4000 + b'"}}'
		code, _, body = self.through_proxy(self.serve(error, content_type="application/json", drop_after=100).url)
		self.assertNotEqual(code, 0)
		self.assertNotEqual(body, error)

	def test_request_body_keeps_its_length(self):
		upstream = self.serve(RECORDED)
		self.through_proxy(upstream.url)
		self.assertEqual(upstream.requests[0]["headers"].get("content-length"), str(len(b'{"stream":true}')))

	def test_malformed_and_unfinished_events_pass_through(self):
		odd = b"data: not json\n\n: comment\n\ndata: [DONE]\n\nevent: cut-off\ndata: {"
		_, _, body = self.through_proxy(self.serve(odd).url)
		self.assertEqual(body, odd)

	def test_other_reply_passes_through_with_its_status(self):
		error = b'{"error":{"message":"denied"}}'
		code, status, body = self.through_proxy(self.serve(error, content_type="application/json", status=403).url)
		self.assertEqual((code, status, body), (0, 403, error))

	def test_request_reaches_the_upstream_unchanged(self):
		upstream = self.serve(RECORDED)
		self.through_proxy(upstream.url)
		self.assertEqual(len(upstream.requests), 1)
		request = upstream.requests[0]
		self.assertEqual(request["path"], "/backend-api/codex/responses")
		self.assertEqual(request["body"], b'{"stream":true}')
		self.assertEqual(request["headers"]["authorization"], "Bearer placeholder-token")
		self.assertEqual(request["headers"]["chatgpt-account-id"], "placeholder-account")
		self.assertEqual(request["headers"]["accept-encoding"], "identity")

	def test_exit_code_is_the_commands(self):
		code, _, _ = self.through_proxy(self.serve(RECORDED).url, exit_code=7)
		self.assertEqual(code, 7)

	def test_unreachable_upstream_is_a_502(self):
		upstream = Upstream(RECORDED)
		upstream.close()
		code, status, body = self.through_proxy(upstream.url)
		self.assertEqual((code, status), (0, 502))
		self.assertIn(b"fill_responses_output:", body)

	def test_usage_error_without_a_command(self):
		done = subprocess.run(["node", str(PROXY), "http://127.0.0.1:1", URL_ENV], capture_output=True, timeout=60)
		self.assertEqual(done.returncode, 2)
		self.assertIn(b"usage:", done.stderr)


if __name__ == "__main__":
	unittest.main()
