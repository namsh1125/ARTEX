import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
from urllib.request import Request, urlopen
from urllib.error import HTTPError

from bridge import ALIAS, BridgeError, BridgeServer, CodexRunner, decode_answer, prepare


def request_body():
    return {"model": ALIAS, "messages": [{"role": "user", "content": "hello"}]}


def with_tool():
    body = request_body()
    body["tools"] = [{"type": "function", "function": {
        "name": "lookup", "parameters": {"type": "object", "required": ["id"],
        "properties": {"id": {"type": "integer"}}, "additionalProperties": False}}}]
    return body


class MappingTests(unittest.TestCase):
    def decode(self, body, answer):
        _, validators, choice, forced = prepare(body)
        return decode_answer(answer, validators, choice, forced, body.get("parallel_tool_calls", True))

    def test_text_and_refusal_preserved(self):
        message = self.decode(request_body(), {"content": "Cannot perform that action.", "tool_calls": []})
        self.assertEqual(message["content"], "Cannot perform that action.")

    def test_tool_round_trip_preserves_ids(self):
        body = with_tool()
        message = self.decode(body, {"content": "", "tool_calls": [{"name": "lookup", "arguments": '{"id": 7}'}]})
        call = message["tool_calls"][0]
        body["messages"] += [message, {"role": "tool", "tool_call_id": call["id"], "content": "found"}]
        prompt, *_ = prepare(body)
        history = json.loads(prompt.split("\n\n", 1)[1])["messages"]
        self.assertEqual(history[-1]["tool_call_id"], history[-2]["tool_calls"][0]["id"])
        self.assertEqual(json.loads(call["function"]["arguments"]), {"id": 7})

    def test_invalid_tool_responses_rejected(self):
        for call in ({"name": "unknown", "arguments": "{}"},
                     {"name": "lookup", "arguments": '{"id": "wrong"}'},
                     {"name": "lookup", "arguments": "[]"},
                     {"name": "lookup", "arguments": "not json"}):
            with self.subTest(call=call), self.assertRaises(BridgeError) as caught:
                self.decode(with_tool(), {"content": "", "tool_calls": [call]})
            self.assertEqual(caught.exception.status, 502)

    def test_choice_and_parallel_constraints(self):
        call = {"name": "lookup", "arguments": '{"id": 7}'}
        cases = [("none", [call]), ("required", []),
                 ({"type": "function", "function": {"name": "lookup"}}, [])]
        for choice, calls in cases:
            body = with_tool()
            body["tool_choice"] = choice
            with self.subTest(choice=choice), self.assertRaises(BridgeError):
                self.decode(body, {"content": "text", "tool_calls": calls})
        body = with_tool()
        body["parallel_tool_calls"] = False
        with self.assertRaises(BridgeError):
            self.decode(body, {"content": "", "tool_calls": [call, call]})

    def test_empty_and_invalid_answers_rejected(self):
        for answer in ({"content": "", "tool_calls": []}, {}, [],
                       {"content": None, "tool_calls": []}):
            with self.subTest(answer=answer), self.assertRaises(BridgeError):
                self.decode(request_body(), answer)

    def test_unsupported_input_rejected(self):
        cases = [[], {}, dict(request_body(), model="other"),
                 dict(request_body(), response_format={}),
                 dict(request_body(), stream="false"),
                 dict(request_body(), messages=[{"role": "user", "content": [{"type": "image_url"}]}])]
        for body in cases:
            with self.subTest(body=body), self.assertRaises(BridgeError):
                prepare(body)

    def test_schema_does_not_fetch_network_refs(self):
        body = with_tool()
        body["tools"][0]["function"]["parameters"] = {"$ref": "https://example.com/schema"}
        with self.assertRaises(BridgeError) as caught:
            prepare(body)
        self.assertEqual(caught.exception.status, 400)


class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.calls = []
        def runner(prompt):
            self.calls.append(prompt)
            return {"content": "hello", "tool_calls": []}
        self.server = BridgeServer(("127.0.0.1", 0), "test-token", runner)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = "http://127.0.0.1:" + str(self.server.server_port)

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def send(self, body=None, token="test-token", path="/v1/chat/completions", headers=None):
        data = json.dumps(body).encode() if body is not None else None
        req = Request(self.url + path, data=data, headers={"Authorization": "Bearer " + token,
                      "Content-Type": "application/json", **(headers or {})})
        try:
            with urlopen(req, timeout=3) as response:
                return response.status, response.read()
        except HTTPError as exc:
            return exc.code, exc.read()

    def test_json_and_models(self):
        status, raw = self.send(request_body())
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(raw)["choices"][0]["message"]["content"], "hello")
        self.assertNotIn("usage", json.loads(raw))
        self.assertEqual(self.send(path="/v1/models")[0], 200)

    def test_auth_origin_and_unknown_path_never_invoke_runner(self):
        self.assertEqual(self.send(request_body(), token="wrong")[0], 401)
        self.assertEqual(self.send(request_body(), headers={"Origin": "https://example.com"})[0], 403)
        self.assertEqual(self.send(request_body(), path="/v1/responses")[0], 404)
        self.assertFalse(self.calls)

    def test_busy(self):
        self.server.slot.acquire()
        try:
            self.assertEqual(self.send(request_body())[0], 429)
        finally:
            self.server.slot.release()
        self.assertEqual(self.send(request_body())[0], 200)

    def test_sse_tool_frames(self):
        self.server.runner = lambda _: {"content": "", "tool_calls": [{"name": "lookup", "arguments": '{"id": 1}'}]}
        body = with_tool()
        body["stream"] = True
        status, raw = self.send(body)
        self.assertEqual(status, 200)
        frames = [line[6:] for line in raw.decode().splitlines() if line.startswith("data: ")]
        self.assertEqual(frames[-1], "[DONE]")
        first, last = map(json.loads, frames[:-1])
        self.assertEqual(first["choices"][0]["delta"]["tool_calls"][0]["index"], 0)
        self.assertEqual(last["choices"][0]["finish_reason"], "tool_calls")

    def test_errors_release_slot_and_keep_http_status(self):
        def fail(_):
            raise BridgeError(504, "timeout")
        self.server.runner = fail
        self.assertEqual(self.send(dict(request_body(), stream=True))[0], 504)
        self.assertTrue(self.server.slot.acquire(blocking=False))
        self.server.slot.release()


@unittest.skipUnless(os.name == "posix", "Fake executable fixture uses a POSIX shebang")
class ProcessTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.binary = Path(self.tmp.name) / "fake-codex"

    def tearDown(self):
        self.tmp.cleanup()

    def script(self, code):
        import sys
        self.binary.write_text("#!" + sys.executable + "\n" + code, encoding="utf-8")
        self.binary.chmod(0o700)
        return CodexRunner(str(self.binary), timeout=1)

    def test_process_stdin_flags_output_cleanup(self):
        runner = self.script('''import sys, json, os
from pathlib import Path
assert "--sandbox" in sys.argv and "read-only" in sys.argv
assert "--ignore-user-config" in sys.argv
assert "features.shell_tool=false" in sys.argv
assert "--ephemeral" in sys.argv
assert "CODEX_API_KEY" not in os.environ
assert "ARTEX_CODEX_BRIDGE_TOKEN" not in os.environ
assert sys.stdin.read() == "literal $(echo should-not-run)"
out = Path(sys.argv[sys.argv.index("--output-last-message") + 1])
out.write_text(json.dumps({"content": str(Path.cwd()), "tool_calls": []}))
''')
        answer = runner("literal $(echo should-not-run)")
        self.assertFalse(Path(answer["content"]).exists())

    def test_timeout(self):
        runner = self.script("import time\ntime.sleep(10)\n")
        with self.assertRaises(BridgeError) as caught:
            runner("hello")
        self.assertEqual(caught.exception.status, 504)

    def test_nonzero_and_malformed_output(self):
        for code in ("raise SystemExit(1)",
                     'import sys\nfrom pathlib import Path\nPath(sys.argv[sys.argv.index("--output-last-message")+1]).write_text("invalid")'):
            runner = self.script(code)
            with self.subTest(code=code), self.assertRaises(BridgeError) as caught:
                runner("hello")
            self.assertEqual(caught.exception.status, 502)


if __name__ == "__main__":
    unittest.main()
