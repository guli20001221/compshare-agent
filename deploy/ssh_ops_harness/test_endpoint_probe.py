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
    def do_GET(self):
        if self.path == "/redirect":
            self.send_response(302)
            self.send_header("Location", f"http://localhost:{self.server.server_address[1]}/ok")
        elif self.path == "/fail":
            self.send_response(503)
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
tcp_listener.listen(1)
tcp_port = tcp_listener.getsockname()[1]


def _accept_once():
    conn, _ = tcp_listener.accept()
    conn.close()


threading.Thread(target=_accept_once, daemon=True).start()

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
    {"id": "bad-url", "kind": "http", "label": "bad", "source": "test",
     "url": "file:///etc/passwd"},
    {"id": "bad-port", "kind": "tcp", "label": "bad", "source": "test",
     "host": "127.0.0.1", "port": 70000},
    {"id": "../../escape", "kind": "tcp", "label": "bad", "source": "test",
     "host": "127.0.0.1", "port": 1},
])

check("normalizer-keeps-only-server-target-shapes",
      set(targets) == {"platform-http-1", "platform-tcp-2", "platform-http-3", "platform-http-4"})
public = json.dumps(endpoint_probe.public_targets(targets))
check("public-targets-hide-url-host-and-token",
      all(value not in public for value in ("127.0.0.1", "live-secret", "token=", "http://")))
schema = endpoint_probe.input_schema(targets)
check("schema-is-an-opaque-enum", schema["properties"]["target_id"]["enum"] == list(targets))
check("schema-accepts-no-url-host-port", all(k not in schema["properties"] for k in ("url", "host", "port")))

http_result = endpoint_probe.probe(targets, "platform-http-1")
check("http-probe-reaches-response", http_result["transport_reachable"] is True and http_result["http_status"] == 204)
check("http-result-hides-private-destination",
      all(value not in json.dumps(http_result) for value in ("127.0.0.1", "live-secret", "token=", "must-not-return")))
failed_http = endpoint_probe.probe(targets, "platform-http-3")
check("http-error-still-proves-transport-not-application-health",
      failed_http["transport_reachable"] is True
      and failed_http["stage"] == "http_response" and failed_http["http_status"] == 503)
redirect = endpoint_probe.probe(targets, "platform-http-4")
check("cross-origin-redirect-is-not-followed",
      redirect["transport_reachable"] is True
      and redirect["stage"] == "redirect_refused"
      and redirect["error_class"] == "cross_origin_redirect")
tcp_result = endpoint_probe.probe(targets, "platform-tcp-2")
check("tcp-probe-connects-without-payload",
      tcp_result["transport_reachable"] is True and tcp_result["stage"] == "tcp_connected")
unknown = endpoint_probe.probe(targets, "not-listed")
check("model-cannot-invent-a-destination",
      unknown["transport_reachable"] is False and unknown["error_class"] == "unknown_target_id")

httpd.shutdown()
httpd.server_close()
tcp_listener.close()

if FAILS:
    print(f"\n{len(FAILS)} FAILED: {', '.join(FAILS)}")
    raise SystemExit(1)
print("endpoint_probe: ALL GREEN")
