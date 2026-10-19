import json
import io
import os
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch
from urllib.request import Request, urlopen
from urllib.error import HTTPError

from bridge import (ALIAS, BridgeError, BridgeServer, CodexRunner, CodexResult,
                    decode_answer, prepare, read_usage)


class UsageTests(unittest.TestCase):
    def parse(self, *usages):
        return read_usage(io.StringIO('\n'.join(
            json.dumps({"type": "turn.completed", "usage": u}) for u in usages)))

    def test_real_counters_cached_subset_and_multiple_turns(self):
        usage = self.parse({"input_tokens": 100, "cached_input_tokens": 80,
                            "output_tokens": 20, "reasoning_output_tokens": 5},
                           {"input_tokens": 40, "cached_input_tokens": 10,
                            "output_tokens": 10, "reasoning_output_tokens": 2})
        self.assertEqual(usage, {"prompt_tokens": 140, "completion_tokens": 30,
                                "total_tokens": 170,
                                "prompt_tokens_details": {"cached_tokens": 90},
                                "completion_tokens_details": {"reasoning_tokens": 7}})

    def test_missing_is_not_zero_and_model_text_is_not_usage(self):
        self.assertIsNone(self.parse(None))
        self.assertIsNone(self.parse({"input_tokens": 10, "cached_input_tokens": 0,
                                      "output_tokens": 1}, None))
        self.assertIsNone(read_usage(io.StringIO(
            '{"type":"item.completed","item":{"usage":{"input_tokens":999}}}\n')))
        self.assertEqual(self.parse({"input_tokens": 0, "cached_input_tokens": 0,
                                    "output_tokens": 0})["total_tokens"], 0)

    def test_invalid_counters_and_json_rejected_without_leaking(self):
        base = {"input_tokens": 10, "cached_input_tokens": 2, "output_tokens": 3}
        for bad in (-1, True, "10", 1.5, None):
            with self.subTest(bad=bad), self.assertRaises(BridgeError):
                self.parse(dict(base, input_tokens=bad))
        for usage in ({}, [], dict(base, cached_input_tokens=11),
                      dict(base, reasoning_output_tokens=4)):
            with self.subTest(usage=usage), self.assertRaises(BridgeError):
                self.parse(usage)
        with self.assertRaises(BridgeError) as caught:
            read_usage(io.StringIO('private malformed text'))
        self.assertNotIn('private', str(caught.exception))


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
            return CodexResult({"content": "hello", "tool_calls": []})
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
        self.server.runner = lambda _: CodexResult({"content": "", "tool_calls": [{"name": "lookup", "arguments": '{"id": 1}'}]})
        body = with_tool()
        body["stream"] = True
        status, raw = self.send(body)
        self.assertEqual(status, 200)
        frames = [line[6:] for line in raw.decode().splitlines() if line.startswith("data: ")]
        self.assertEqual(frames[-1], "[DONE]")
        first, last = map(json.loads, frames[:-1])
        self.assertEqual(first["choices"][0]["delta"]["tool_calls"][0]["index"], 0)
        self.assertEqual(last["choices"][0]["finish_reason"], "tool_calls")

    def test_json_and_sse_usage(self):
        usage = {"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120,
                 "prompt_tokens_details": {"cached_tokens": 80}}
        self.server.runner = lambda _: CodexResult({"content": "hello", "tool_calls": []}, usage)
        self.assertEqual(json.loads(self.send(request_body())[1])["usage"], usage)
        for include in (True, False):
            body = dict(request_body(), stream=True, stream_options={"include_usage": include})
            status, raw = self.send(body)
            self.assertEqual(status, 200)
            frames = [line[6:] for line in raw.decode().splitlines() if line.startswith("data: ")]
            self.assertEqual(frames[-1], "[DONE]")
            events = list(map(json.loads, frames[:-1]))
            self.assertEqual(sum("usage" in e for e in events), int(include))
            if include:
                self.assertEqual(events[-1]["choices"], [])
                self.assertEqual(events[-1]["usage"], usage)

    def test_include_usage_validation_and_missing_usage(self):
        self.assertEqual(self.send(dict(request_body(), stream_options={"include_usage": "true"}))[0], 400)
        status, raw = self.send(dict(request_body(), stream=True, stream_options={"include_usage": True}))
        self.assertEqual(status, 200)
        self.assertNotIn(b'"usage"', raw)

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
        return CodexRunner(str(self.binary), timeout=5)

    def test_process_stdin_flags_output_cleanup(self):
        runner = self.script('''import sys, json, os
from pathlib import Path
assert "--sandbox" in sys.argv and "read-only" in sys.argv
assert "--ignore-user-config" in sys.argv
assert "features.shell_tool=false" in sys.argv
assert "--ephemeral" in sys.argv
assert "--json" in sys.argv
assert "CODEX_API_KEY" not in os.environ
assert "ARTEX_CODEX_BRIDGE_TOKEN" not in os.environ
assert sys.stdin.read() == "literal $(echo should-not-run)"
out = Path(sys.argv[sys.argv.index("--output-last-message") + 1])
out.write_text(json.dumps({"content": str(Path.cwd()), "tool_calls": []}))
print(json.dumps({"type":"turn.completed", "usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20}}))
''')
        result = runner("literal $(echo should-not-run)")
        self.assertFalse(Path(result.answer["content"]).exists())
        self.assertEqual(result.usage["total_tokens"], 120)

    def test_timeout(self):
        runner = self.script("import time\ntime.sleep(10)\n")
        runner.timeout = 1
        with self.assertRaises(BridgeError) as caught:
            runner("hello")
        self.assertEqual(caught.exception.status, 504)

    def test_missing_usage_keeps_answer(self):
        runner = self.script('''import sys, json
from pathlib import Path
Path(sys.argv[sys.argv.index("--output-last-message")+1]).write_text(
    json.dumps({"content":"hello", "tool_calls":[]}))
print('{"type":"turn.completed"}')
''')
        result = runner("hello")
        self.assertEqual(result.answer["content"], "hello")
        self.assertIsNone(result.usage)

    def test_oversized_event_output_rejected(self):
        runner = self.script('print("x" * 1025)')
        with patch("bridge.MAX_OUTPUT", 1024), self.assertRaises(BridgeError) as caught:
            runner("hello")
        self.assertEqual(caught.exception.status, 502)

    def test_nonzero_and_malformed_output(self):
        for code in ("raise SystemExit(1)",
                     'import sys\nfrom pathlib import Path\nPath(sys.argv[sys.argv.index("--output-last-message")+1]).write_text("invalid")'):
            runner = self.script(code)
            with self.subTest(code=code), self.assertRaises(BridgeError) as caught:
                runner("hello")
            self.assertEqual(caught.exception.status, 502)


if __name__ == "__main__":
    unittest.main()
