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

tcp_client, tcp_channel, tcp_transport = _open()
tcp = guest_endpoint_probe.probe(
    {}, {"protocol": "tcp", "port": 8888}, opener=lambda _c: (tcp_client, None))
check("tcp-probe-connects-without-sending", tcp["ok"] and tcp["connected"] and not tcp_channel.sent)
check("tcp-probe-still-fixed-to-loopback", tcp_transport.opened[1] == ("127.0.0.1", 8888))

opened = []
for name, args in [
    ("bad-port", {"protocol": "tcp", "port": 0}),
    ("bad-protocol", {"protocol": "udp", "port": 1}),
    ("body-method", {"protocol": "http", "port": 80, "method": "POST"}),
    ("header-injection", {"protocol": "http", "port": 80, "host_header": "x\r\nAuth: y"}),
    ("absolute-url", {"protocol": "http", "port": 80, "path": "http://example.com/"}),
]:
    bad = guest_endpoint_probe.probe(
        {}, args, opener=lambda _c: (opened.append(True), None))
    check("invalid-is-rejected-before-connect::" + name,
          bad["error_class"] == "invalid_arguments")
check("invalid-never-opens-ssh", opened == [])

secret_client, _, _ = _open(b"HTTP/1.1 200 OK\r\n\r\nknown-secret")
scrubbed = guest_endpoint_probe.probe(
    {}, {"protocol": "http", "port": 80}, secrets=("known-secret",),
    opener=lambda _c: (secret_client, None))
check("http-body-is-secret-scrubbed", "known-secret" not in scrubbed["body"])

if FAILS:
    print("\n%d FAILED: %s" % (len(FAILS), ", ".join(FAILS)))
    raise SystemExit(1)
print("guest_endpoint_probe: ALL GREEN")
