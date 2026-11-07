import contextlib
import http.client
import importlib.util
import io
import json
from pathlib import Path
import threading
import unittest

spec = importlib.util.spec_from_file_location("lambda_server", Path(__file__).with_name("server.py"))
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)


class WrapperTests(unittest.TestCase):
    def setUp(self):
        self.events = []

        def handler(event):
            self.events.append(event)
            if event == "raise":
                raise RuntimeError("private exception detail")
            if event == "unserializable":
                return {1, 2}
            return {"received": event}

        self.server = server.FunctionServer(("127.0.0.1", 0), handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def request(self, method, path="/", body=None, headers=None):
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        try:
            connection.request(method, path, body=body, headers=headers or {})
            response = connection.getresponse()
            self.assertEqual(response.getheader("Content-Type"), "application/json")
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    def invoke(self, event):
        return self.request("POST", body=json.dumps(event), headers={"Content-Type": "application/json"})

    def test_health_does_not_invoke_function(self):
        self.assertEqual(self.request("GET", "/healthz"), (200, {"status": "ok"}))
        self.assertEqual(self.events, [])

    def test_json_values_pass_unchanged(self):
        for event in ({"name": "Owen"}, [1, 2], None, 42, "hello"):
            self.assertEqual(self.invoke(event), (200, {"received": event}))

    def test_invalid_json_and_content_type_do_not_invoke(self):
        for body in ("{bad", "NaN", b"\xff"):
            self.assertEqual(self.request("POST", body=body, headers={"Content-Type": "application/json"})[0], 400)
        self.assertEqual(self.request("POST", body="{}", headers={"Content-Type": "text/plain"})[0], 415)
        self.assertEqual(self.events, [])

    def test_errors_are_generic_and_server_remains_available(self):
        with contextlib.redirect_stderr(io.StringIO()) as logs:
            for event in ("raise", "unserializable"):
                self.assertEqual(self.invoke(event), (500, {"error": "function execution failed"}))
            self.assertIn("private exception detail", logs.getvalue())
        self.assertEqual(self.invoke({})[0], 200)

    def test_request_size_is_bounded(self):
        status, _ = self.request("POST", body="", headers={
            "Content-Type": "application/json", "Content-Length": str(server.MAX_BODY + 1),
        })
        self.assertEqual(status, 413)
        self.assertEqual(self.events, [])

    def test_unknown_routes_do_not_invoke(self):
        self.assertEqual(self.request("GET", "/")[0], 404)
        self.assertEqual(self.request("POST", "/missing", body="{}")[0], 404)
        self.assertEqual(self.events, [])

    def test_private_invocation_and_health_require_bearer_token(self):
        token = "runtime-token-0123456789abcdef0123456789"
        self.server.auth_mode = "bearer"
        self.server.token = token
        self.assertEqual(self.invoke({})[0], 401)
        self.assertEqual(self.request("GET", "/healthz")[0], 401)
        self.assertEqual(self.request("POST", "/?token=" + token, body="{}")[0], 401)
        self.assertEqual(self.request("POST", body="{}", headers={
            "Content-Type": "application/json", "Authorization": "Bearer wrong",
        })[0], 401)
        self.assertEqual(self.events, [])
        status, value = self.request("POST", body='{"name":"Owen"}', headers={
            "Content-Type": "application/json", "Authorization": "Bearer " + token,
        })
        self.assertEqual((status, value), (200, {"received": {"name": "Owen"}}))
        self.assertEqual(self.request("GET", "/healthz", headers={
            "Authorization": "Bearer " + token,
        })[0], 200)

    def test_bearer_mode_without_token_fails_closed(self):
        with self.assertRaises(ValueError):
            server.FunctionServer(("127.0.0.1", 0), lambda event: event, "bearer", "")
        with self.assertRaises(ValueError):
            server.FunctionServer(("127.0.0.1", 0), lambda event: event, "unknown", "")


if __name__ == "__main__":
    unittest.main()
