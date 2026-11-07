"""HTTP adapter for a synchronous, single-argument Python function."""
import importlib
import hmac
import inspect
import json
import os
import sys
import traceback
from http.server import BaseHTTPRequestHandler, HTTPServer

MAX_BODY = 1024 * 1024


def load_handler(entry):
    handler = getattr(importlib.import_module("function"), entry)
    if not callable(handler) or inspect.iscoroutinefunction(handler):
        raise TypeError("entry must be a synchronous callable")
    inspect.signature(handler).bind({})
    return handler


class FunctionServer(HTTPServer):
    def __init__(self, address, handler, auth_mode="public", token=""):
        if auth_mode not in ("public", "bearer"):
            raise ValueError("invalid LAMBDA_AUTH_MODE")
        if auth_mode == "bearer" and not token:
            raise ValueError("bearer authentication requires an invocation token")
        super().__init__(address, RequestHandler)
        self.handler = handler
        self.auth_mode = auth_mode
        self.token = token


class RequestHandler(BaseHTTPRequestHandler):
    def parse_request(self):
        return super().parse_request() and self.authorized()

    def log_request(self, code="-", size="-"):
        # Request headers and query strings can contain credentials.
        self.log_message('"%s %s" %s', self.command, self.path.partition("?")[0], code)

    def setup(self):
        super().setup()
        self.connection.settimeout(10)

    def send_json(self, status, value):
        payload = json.dumps(value, allow_nan=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        if status == 401:
            self.send_header("WWW-Authenticate", "Bearer")
        self.end_headers()
        self.wfile.write(payload)

    def authorized(self):
        if self.server.auth_mode == "public":
            return True
        headers = self.headers.get_all("Authorization", [])
        parts = headers[0].split(" ", 1) if len(headers) == 1 else []
        if (len(parts) == 2 and parts[0].lower() == "bearer"
                and hmac.compare_digest(parts[1].encode("utf-8"), self.server.token.encode("utf-8"))):
            return True
        self.send_json(401, {"error": "unauthorized"})
        return False

    def do_GET(self):
        if self.path == "/healthz":
            self.send_json(200, {"status": "ok"})
        else:
            self.send_json(404, {"error": "not found"})

    def do_POST(self):
        if self.path not in ("/", "/invoke"):
            self.send_json(404, {"error": "not found"})
            return
        if self.headers.get("Transfer-Encoding"):
            self.send_json(400, {"error": "chunked requests are unsupported"})
            return
        if self.headers.get_content_type() != "application/json":
            self.send_json(415, {"error": "expected application/json"})
            return
        if self.headers.get("Content-Length") is None:
            self.send_json(411, {"error": "Content-Length is required"})
            return
        try:
            length = int(self.headers["Content-Length"])
            if length < 0:
                raise ValueError()
        except ValueError:
            self.send_json(400, {"error": "invalid Content-Length"})
            return
        if length > MAX_BODY:
            self.send_json(413, {"error": "request exceeds 1 MiB"})
            return
        try:
            payload = self.rfile.read(length)
            if len(payload) != length:
                raise ValueError("incomplete body")
            event = json.loads(payload, parse_constant=self.reject_constant)
        except (ValueError, UnicodeError):
            self.send_json(400, {"error": "invalid JSON"})
            return
        except (TimeoutError, OSError):
            self.send_json(408, {"error": "request body timed out"})
            return
        try:
            result = self.server.handler(event)
            # Serialize before writing headers, so invalid return values yield 500.
            self.send_json(200, result)
        except Exception:
            traceback.print_exc()
            self.send_json(500, {"error": "function execution failed"})

    @staticmethod
    def reject_constant(value):
        raise ValueError("invalid JSON constant: " + value)


if __name__ == "__main__":
    entry = sys.argv[1] if len(sys.argv) > 1 else "handler"
    auth_mode = os.environ.get("LAMBDA_AUTH_MODE", "public")
    token = os.environ.get("LAMBDA_INVOCATION_TOKEN", "")
    server = FunctionServer(("0.0.0.0", 8080), load_handler(entry), auth_mode, token)
    print("serving function on port 8080", flush=True)
    server.serve_forever()
