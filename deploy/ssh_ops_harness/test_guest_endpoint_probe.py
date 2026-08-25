"""Offline contract tests for the guest-loopback TCP/HTTP probe."""
import guest_endpoint_probe

FAILS = []


def check(name, condition):
    if not condition:
        FAILS.append(name)
        print("XX ", name)


class _Channel:
    def __init__(self, response=b""):
        self.response, self.offset, self.sent, self.timeout, self.closed = response, 0, b"", None, False

    def settimeout(self, value):
        self.timeout = value

    def sendall(self, data):
        self.sent += data

    def recv(self, limit):
        data = self.response[self.offset:self.offset + limit]
        self.offset += len(data)
        return data

    def close(self):
        self.closed = True


class _Transport:
    def __init__(self, channel):
        self.channel, self.opened = channel, None

    def is_active(self):
        return True

    def open_channel(self, kind, dest, source, timeout=None):
        self.opened = kind, dest, source, timeout
        return self.channel


class _Client:
    def __init__(self, transport):
        self.transport, self.closed = transport, False

    def get_transport(self):
        return self.transport

    def close(self):
        self.closed = True


def _open(response=b""):
    channel = _Channel(response)
    transport = _Transport(channel)
    client = _Client(transport)
    return client, channel, transport


http_response = (b"HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain\r\n\r\n"
                 b"Blocked request for fixture.invalid")
client, channel, transport = _open(http_response)
result = guest_endpoint_probe.probe(
    {}, {"protocol": "http", "port": 5173, "path": "/",
         "host_header": "5173-cpod-test.invalid", "timeout_seconds": 4},
    opener=lambda _c: (client, None))
check("http-probe-dials-only-guest-loopback",
      transport.opened == ("direct-tcpip", ("127.0.0.1", 5173), ("127.0.0.1", 0), 4))
check("http-probe-sends-bounded-get-and-literal-host",
      channel.sent.startswith(b"GET / HTTP/1.1\r\n") and
      b"Host: 5173-cpod-test.invalid\r\n" in channel.sent)
check("http-probe-reports-status-and-body",
      result["ok"] and result["status_code"] == 403 and "Blocked request" in result["body"])
check("http-probe-closes-channel-and-client", channel.closed and client.closed)

auth_token = "guest-api-" + "secret-456"
auth_value = "Bearer " + auth_token
auth_client, auth_channel, _ = _open(
    b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\n" + auth_token.encode())
authenticated = guest_endpoint_probe.probe(
    {}, {"protocol": "http", "port": 8000, "path": "/v1/models",
         "authorization": auth_value}, opener=lambda _c: (auth_client, None))
check("authenticated-guest-probe-sends-header-without-returning-secret",
      ("Authorization: " + auth_value + "\r\n").encode() in auth_channel.sent
      and authenticated["authorization_sent"] is True
      and auth_token not in authenticated["body"])

status_client, _, _ = _open(
    ("HTTP/1.1 401 Bearer " + auth_token + "\r\nContent-Length: 0\r\n\r\n").encode())
status_echo = guest_endpoint_probe.probe(
    {}, {"protocol": "http", "port": 8000, "path": "/v1/models",
         "authorization": auth_value}, opener=lambda _c: (status_client, None))
check("http-status-line-is-secret-scrubbed",
      status_echo["status_code"] == 401
      and auth_token not in status_echo["status_line"]
      and "REDACTED" in status_echo["status_line"])

boundary_prefix = auth_token[:9]
boundary_body = (b" " * (guest_endpoint_probe._MAX_BODY_BYTES - len(boundary_prefix))
                 + auth_token.encode() + b"tail")
boundary_client, _, _ = _open(b"HTTP/1.1 200 OK\r\n\r\n" + boundary_body)
boundary_echo = guest_endpoint_probe.probe(
    {}, {"protocol": "http", "port": 8000, "authorization": auth_value},
    opener=lambda _c: (boundary_client, None))
check("http-body-is-scrubbed-before-the-return-boundary",
      boundary_echo["truncated"] is True
      and boundary_prefix not in boundary_echo["body"]
      and auth_token not in boundary_echo["body"])
old_order_body = guest_endpoint_probe.guardrails.scrub_output(
    boundary_body[:guest_endpoint_probe._MAX_BODY_BYTES].decode(), (auth_token,))
check("boundary-fixture-would-catch-the-old-slice-before-scrub-order",
      boundary_prefix in old_order_body)

transport_prefix = auth_token[:9]
http_prefix = b"HTTP/1.1 200 OK\r\nX-Pad: " + b"a" * 25000 + b"\r\n\r\n"
transport_padding = b" " * (
    guest_endpoint_probe._MAX_RESPONSE_BYTES - len(http_prefix) - len(transport_prefix))
transport_body = transport_padding + auth_token.encode() + b"tail"
transport_client, _, _ = _open(http_prefix + transport_body)
transport_echo = guest_endpoint_probe.probe(
    {}, {"protocol": "http", "port": 8000, "authorization": auth_value},
    opener=lambda _c: (transport_client, None))
check("http-body-drops-a-possible-partial-secret-at-the-transport-cap",
      transport_echo["truncated"] is True
      and transport_prefix not in transport_echo["body"]
      and auth_token not in transport_echo["body"])
old_transport_body = guest_endpoint_probe.guardrails.scrub_output(
    transport_body[:guest_endpoint_probe._MAX_RESPONSE_BYTES - len(http_prefix)].decode(),
    (auth_token,))
check("transport-cap-fixture-would-catch-the-old-partial-secret-return",
      transport_prefix in old_transport_body)

blank_auth_client, blank_auth_channel, _ = _open(
    b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n")
blank_auth = guest_endpoint_probe.probe(
    {}, {"protocol": "http", "port": 8000, "path": "/v1/models", "authorization": ""},
    opener=lambda _c: (blank_auth_client, None))
check("explicit-empty-authorization-means-no-header",
      blank_auth["ok"] is True and blank_auth["status_code"] == 401
      and blank_auth["authorization_sent"] is False
      and b"Authorization:" not in blank_auth_channel.sent)

tcp_client, tcp_channel, tcp_transport = _open()
tcp = guest_endpoint_probe.probe(
    {}, {"protocol": "tcp", "port": 8888}, opener=lambda _c: (tcp_client, None))
check("tcp-probe-connects-without-sending",
      tcp["ok"] and tcp["probe_completed"] and tcp["connected"] and not tcp_channel.sent)
check("tcp-probe-still-fixed-to-loopback", tcp_transport.opened[1] == ("127.0.0.1", 8888))


class _RefusingTransport:
    def is_active(self):
        return True

    def open_channel(self, _kind, _dest, _source, timeout=None):
        raise ConnectionRefusedError("fixture listener is closed")


refused_client = _Client(_RefusingTransport())
refused = guest_endpoint_probe.probe(
    {}, {"protocol": "tcp", "port": 8188}, opener=lambda _c: (refused_client, None))
check("closed-listener-is-a-completed-negative-probe",
      refused["ok"] is False and refused["probe_completed"] is True
      and refused["connected"] is False and refused["error_class"] == "ConnectionRefusedError")
check("closed-listener-still-closes-ssh-client", refused_client.closed)


class _BrokenTransport:
    def __init__(self):
        self.active = True

    def is_active(self):
        return self.active

    def open_channel(self, _kind, _dest, _source, timeout=None):
        self.active = False
        raise RuntimeError("fixture SSH transport died")


broken_client = _Client(_BrokenTransport())
broken = guest_endpoint_probe.probe(
    {}, {"protocol": "tcp", "port": 8188}, opener=lambda _c: (broken_client, None))
check("ssh-channel-failure-is-not-a-completed-guest-observation",
      broken["ok"] is False and "probe_completed" not in broken
      and broken["error_class"] == "RuntimeError")
check("ssh-channel-failure-still-closes-client", broken_client.closed)

opened = []
for name, args in [
    ("bad-port", {"protocol": "tcp", "port": 0}),
    ("bad-protocol", {"protocol": "udp", "port": 1}),
    ("body-method", {"protocol": "http", "port": 80, "method": "POST"}),
    ("header-injection", {"protocol": "http", "port": 80, "host_header": "x\r\nAuth: y"}),
    ("authorization-injection", {"protocol": "http", "port": 80,
                                  "authorization": "Bearer x\r\nX-Evil: y"}),
    ("tcp-authorization", {"protocol": "tcp", "port": 80,
                            "authorization": "Bearer x"}),
    ("absolute-url", {"protocol": "http", "port": 80, "path": "http://example.com/"}),
]:
    bad = guest_endpoint_probe.probe(
        {}, args, opener=lambda _c: (opened.append(True), None))
    check("invalid-is-rejected-before-connect::" + name,
          bad["error_class"] == "invalid_arguments")
check("invalid-never-opens-ssh", opened == [])

invalid_shape = guest_endpoint_probe.probe(
    {}, {"url": "http://127.0.0.1:8000/health", "authorization": "Bearer never-return-me"},
    opener=lambda _c: (opened.append(True), None))
check("invalid-shape-explains-fields-without-reflecting-values",
      invalid_shape["invalid_fields"] == ["port", "protocol", "unknown_fields"]
      and invalid_shape["unknown_field_count"] == 1
      and invalid_shape["expected_http_call"] == {
          "protocol": "http", "port": 8000, "path": "/health",
      }
      and "never-return-me" not in repr(invalid_shape)
      and "127.0.0.1" not in repr(invalid_shape))
schema = guest_endpoint_probe.input_schema()
check("schema-makes-protocol-and-integer-port-explicit",
      "Lowercase" in schema["properties"]["protocol"]["description"]
      and "integer" in schema["properties"]["port"]["description"]
      and "authorization" not in schema["properties"]
      and "authorization_ref" not in schema["properties"]
      and "never pass a URL" in guest_endpoint_probe.TOOL_DESCRIPTION)
authorization_ref = "current-user-authorization-1"
auth_schema = guest_endpoint_probe.input_schema([authorization_ref, "x" * 65])
check("schema-exposes-only-an-opaque-current-request-authorization-reference",
      "authorization" not in auth_schema["properties"]
      and auth_schema["properties"]["authorization_ref"]["enum"] == [authorization_ref]
      and "privately" in auth_schema["properties"]["authorization_ref"]["description"]
      and all(field not in auth_schema["properties"]
              for field in ("token", "bearer_token", "headers", "body")))

secret_client, _, _ = _open(b"HTTP/1.1 200 OK\r\n\r\nknown-secret")
scrubbed = guest_endpoint_probe.probe(
    {}, {"protocol": "http", "port": 80}, secrets=("known-secret",),
    opener=lambda _c: (secret_client, None))
check("http-body-is-secret-scrubbed", "known-secret" not in scrubbed["body"])

if FAILS:
    print("\n%d FAILED: %s" % (len(FAILS), ", ".join(FAILS)))
    raise SystemExit(1)
print("guest_endpoint_probe: ALL GREEN")
