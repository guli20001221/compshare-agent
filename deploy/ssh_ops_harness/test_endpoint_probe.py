"""Offline/local-network tests for the opaque structured endpoint probe."""
import http.server
import json
import socket
import threading

import endpoint_probe

FAILS = []


def check(name, condition):
    if not condition:
        FAILS.append(name)
        print("XX ", name)


class _Handler(http.server.BaseHTTPRequestHandler):
    seen_paths = []
    seen_methods = []
    seen_authorizations = []

    def do_GET(self):
        self.__class__.seen_paths.append(self.path)
        self.__class__.seen_methods.append(("GET", self.path))
        self.__class__.seen_authorizations.append(self.headers.get("Authorization", ""))
        if self.path == "/redirect":
            self.send_response(302)
            self.send_header("Location", f"http://localhost:{self.server.server_address[1]}/ok")
        elif self.path == "/same-redirect":
            self.send_response(302)
            self.send_header("Location", "/ok")
        elif self.path == "/fail":
            self.send_response(503)
        else:
            self.send_response(204)
        self.send_header("X-Probe-Secret", "must-not-return")
        self.end_headers()

    def do_HEAD(self):
        self.__class__.seen_paths.append(self.path)
        self.__class__.seen_methods.append(("HEAD", self.path))
        self.__class__.seen_authorizations.append(self.headers.get("Authorization", ""))
        if self.path == "/same-redirect":
            self.send_response(302)
            self.send_header("Location", "/ok")
        else:
            self.send_response(204)
        self.send_header("X-Probe-Secret", "must-not-return")
        self.end_headers()

    def log_message(self, *_args):
        pass


httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
thread = threading.Thread(target=httpd.serve_forever, daemon=True)
thread.start()
port = httpd.server_address[1]

tcp_listener = socket.socket()
tcp_listener.bind(("127.0.0.1", 0))
tcp_listener.listen(2)
tcp_port = tcp_listener.getsockname()[1]
tcp_payloads = []


def _accept_twice():
    for _ in range(2):
        conn, _ = tcp_listener.accept()
        try:
            conn.settimeout(1)
            tcp_payloads.append(conn.recv(1))
        finally:
            conn.close()


tcp_thread = threading.Thread(target=_accept_twice, daemon=True)
tcp_thread.start()

secret_url = f"http://127.0.0.1:{port}/ok?token=live-secret"
targets = endpoint_probe.normalize_targets([
    {"id": "platform-http-1", "kind": "http", "label": "Jupyter platform entry",
     "source": "DescribeCompShareInstance.Softwares.URL", "url": secret_url},
    {"id": "platform-tcp-2", "kind": "tcp", "label": "reported forward",
     "source": "DescribeCompShareInstance.TcpForwards", "host": "127.0.0.1", "port": tcp_port},
    {"id": "platform-http-3", "kind": "http", "label": "failing application",
     "source": "test", "url": f"http://127.0.0.1:{port}/fail"},
    {"id": "platform-http-4", "kind": "http", "label": "redirecting application",
     "source": "test", "url": f"http://127.0.0.1:{port}/redirect"},
    {"id": "platform-http-5", "kind": "http", "label": "same-origin redirect",
     "source": "test", "url": f"http://127.0.0.1:{port}/same-redirect"},
    {"id": "bad-url", "kind": "http", "label": "bad", "source": "test",
     "url": "file:///etc/passwd"},
    {"id": "bad-port", "kind": "tcp", "label": "bad", "source": "test",
     "host": "127.0.0.1", "port": 70000},
    {"id": "../../escape", "kind": "tcp", "label": "bad", "source": "test",
     "host": "127.0.0.1", "port": 1},
])

check("normalizer-keeps-only-server-target-shapes",
      set(targets) == {"platform-http-1", "platform-tcp-2", "platform-http-3", "platform-http-4",
                       "platform-http-5"})
public = json.dumps(endpoint_probe.public_targets(targets))
check("public-targets-hide-url-host-and-token",
      all(value not in public for value in ("127.0.0.1", "live-secret", "token=", "http://")))
schema = endpoint_probe.input_schema(targets)
check("schema-is-an-opaque-enum", schema["properties"]["target_id"]["enum"] == list(targets))
check("schema-accepts-no-url-host-port", all(k not in schema["properties"] for k in ("url", "host", "port")))
check("schema-allows-only-an-absolute-bounded-path",
      schema["properties"]["path"]["pattern"] == "^/"
      and schema["properties"]["path"]["maxLength"] == endpoint_probe._MAX_PATH_LENGTH)
check("schema-closes-http-methods-to-read-only-options",
      schema["properties"]["method"]["enum"] == ["GET", "HEAD"]
      and "default" not in schema["properties"]["method"])
check("schema-keeps-a-flat-complete-target-enum-for-cli-compatibility",
      schema["properties"]["target_id"]["enum"] == list(targets)
      and "allOf" not in schema and "oneOf" not in schema
      and "send only this field" in schema["properties"]["target_id"]["description"])
check("schema-without-a-current-request-capability-exposes-no-authorization-input",
      "authorization" not in schema["properties"]
      and "authorization_ref" not in schema["properties"])
authorization_ref = "current-user-authorization-1"
auth_schema = endpoint_probe.input_schema(targets, [authorization_ref, "not valid"])
check("schema-exposes-only-an-opaque-current-request-authorization-reference",
      "authorization" not in auth_schema["properties"]
      and auth_schema["properties"]["authorization_ref"]["enum"] == [authorization_ref]
      and "privately" in auth_schema["properties"]["authorization_ref"]["description"]
      and all(field not in auth_schema["properties"]
              for field in ("token", "bearer_token", "headers", "body")))

http_result = endpoint_probe.probe(targets, "platform-http-1")
check("http-probe-reaches-response", http_result["transport_reachable"] is True and http_result["http_status"] == 204)
check("http-result-hides-private-destination",
      all(value not in json.dumps(http_result) for value in ("127.0.0.1", "live-secret", "token=", "must-not-return")))
auth_token = "test-api-" + "secret-123"
auth_value = "Bearer " + auth_token
auth_result = endpoint_probe.probe(
    targets, "platform-http-1", "/v1/models", authorization=auth_value)
check("authenticated-http-probe-sends-user-value-without-returning-it",
      auth_result["transport_reachable"] is True and auth_result["authorization_sent"] is True
      and _Handler.seen_authorizations[-1] == auth_value
      and auth_value not in json.dumps(auth_result)
      and auth_token not in json.dumps(auth_result))
head_result = endpoint_probe.probe(targets, "platform-http-1", method="HEAD")
check("head-probe-reaches-response-without-returning-content",
      head_result["transport_reachable"] is True and head_result["http_status"] == 204
      and head_result["method"] == "HEAD"
      and all(field not in head_result for field in ("body", "body_preview", "headers")))
path_result = endpoint_probe.probe(targets, "platform-http-1", "/index.html?view=compact")
check("http-path-stays-on-selected-origin-and-preserves-platform-query",
      path_result["transport_reachable"] is True and path_result["http_status"] == 204
      and _Handler.seen_paths[-1] in (
          "/index.html?token=live-secret&view=compact",
          "/index.html?view=compact&token=live-secret",
      ))
check("http-path-result-still-hides-query-and-destination",
      all(value not in json.dumps(path_result)
          for value in ("127.0.0.1", "live-secret", "view=compact", "token=", "must-not-return")))
for bad_path in ("relative", "//example.com/escape", "https://example.com/escape", "/ok#hidden"):
    invalid = endpoint_probe.probe(targets, "platform-http-1", bad_path)
    check("invalid-path-is-rejected-before-network::" + bad_path,
          invalid["transport_reachable"] is False
          and invalid["stage"] == "request_validation"
          and invalid["error_class"] == "invalid_path")
too_long = endpoint_probe.probe(targets, "platform-http-1", "/" + "a" * endpoint_probe._MAX_PATH_LENGTH)
check("overlong-path-is-rejected", too_long["error_class"] == "invalid_path")
failed_http = endpoint_probe.probe(targets, "platform-http-3")
check("http-error-still-proves-transport-not-application-health",
      failed_http["transport_reachable"] is True
      and failed_http["stage"] == "http_response" and failed_http["http_status"] == 503)
redirect = endpoint_probe.probe(targets, "platform-http-4")
check("cross-origin-redirect-is-not-followed",
      redirect["transport_reachable"] is True
      and redirect["stage"] == "redirect_refused"
      and redirect["error_class"] == "cross_origin_redirect")
_Handler.seen_methods.clear()
head_redirect = endpoint_probe.probe(targets, "platform-http-5", method="HEAD")
check("head-method-is-preserved-across-a-same-origin-redirect",
      head_redirect["transport_reachable"] is True and head_redirect["http_status"] == 204
      and head_redirect["method"] == "HEAD" and head_redirect["redirects"] == 1
      and _Handler.seen_methods == [("HEAD", "/same-redirect"), ("HEAD", "/ok")])
tcp_result = endpoint_probe.probe(targets, "platform-tcp-2")
check("tcp-probe-connects-without-payload",
      tcp_result["transport_reachable"] is True and tcp_result["stage"] == "tcp_connected")
tcp_cli_defaults = endpoint_probe.probe(targets, "platform-tcp-2", "/", method="GET")
check("tcp-probe-tolerates-cli-materialized-http-defaults-without-payload",
      tcp_cli_defaults["transport_reachable"] is True
      and tcp_cli_defaults["stage"] == "tcp_connected")
tcp_thread.join(2)
check("tcp-probe-sends-zero-bytes-for-plain-and-cli-default-shapes",
      not tcp_thread.is_alive() and tcp_payloads == [b"", b""])
tcp_path = endpoint_probe.probe(targets, "platform-tcp-2", "/not-applicable")
check("tcp-target-rejects-a-path",
      tcp_path["transport_reachable"] is False
      and tcp_path["error_class"] == "http_options_not_supported")
tcp_head = endpoint_probe.probe(targets, "platform-tcp-2", method="HEAD")
check("tcp-target-rejects-http-method-options",
      tcp_head["transport_reachable"] is False
      and tcp_head["error_class"] == "http_options_not_supported")
tcp_auth = endpoint_probe.probe(targets, "platform-tcp-2", authorization=auth_value)
check("tcp-target-rejects-authorization-before-connect",
      tcp_auth["transport_reachable"] is False
      and tcp_auth["error_class"] == "http_options_not_supported")
invalid_method = endpoint_probe.probe(targets, "platform-http-1", method="POST")
check("http-method-is-a-closed-read-only-enum",
      invalid_method["transport_reachable"] is False
      and invalid_method["stage"] == "request_validation"
      and invalid_method["error_class"] == "invalid_http_method")
unknown = endpoint_probe.probe(targets, "not-listed")
check("model-cannot-invent-a-destination",
      unknown["transport_reachable"] is False and unknown["error_class"] == "unknown_target_id")
for bad_auth in (" Bearer x", "Bearer x\r\nX-Evil: y", "x" * 2049):
    invalid = endpoint_probe.probe(targets, "platform-http-1", authorization=bad_auth)
    check("invalid-authorization-is-rejected-before-network::" + repr(bad_auth[:20]),
          invalid["transport_reachable"] is False
          and invalid["stage"] == "request_validation"
          and invalid["error_class"] == "invalid_authorization")

httpd.shutdown()
httpd.server_close()
tcp_listener.close()

if FAILS:
    print(f"\n{len(FAILS)} FAILED: {', '.join(FAILS)}")
    raise SystemExit(1)
print("endpoint_probe: ALL GREEN")
