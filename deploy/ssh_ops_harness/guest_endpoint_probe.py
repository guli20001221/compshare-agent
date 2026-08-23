"""Structured TCP/HTTP probe of the remote guest's loopback through the SSH transport."""
import re
import socket

import guardrails
import ssh_transport

_HOST_HEADER = re.compile(r"[A-Za-z0-9.-]+(?::\d+)?")
_PATH = re.compile(r"/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*")
_MAX_RESPONSE_BYTES = 32768
_MAX_BODY_BYTES = 8192

TOOL_DESCRIPTION = (
    "Probe a TCP listener or HTTP GET/HEAD on loopback inside the selected remote guest, through "
    "the existing SSH transport and without requiring curl/wget/python on the guest. Use it to "
    "separate process/listener/application health from an external platform route. The destination "
    "host is fixed to 127.0.0.1; the tool accepts only a port, bounded path, and optional literal "
    "Host header for virtual-host diagnostics. It cannot reach public/private network hosts, send "
    "request bodies, authenticate, write files, or change the guest.")


def input_schema():
    return {
        "type": "object",
        "properties": {
            "protocol": {"type": "string", "enum": ["tcp", "http"]},
            "port": {"type": "integer", "minimum": 1, "maximum": 65535},
            "method": {"type": "string", "enum": ["GET", "HEAD"], "default": "GET"},
            "path": {"type": "string", "minLength": 1, "maxLength": 512, "default": "/"},
            "host_header": {"type": "string", "minLength": 1, "maxLength": 253,
                            "description": "Optional literal virtual host; never an address to dial."},
            "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": 10, "default": 5},
        },
        "required": ["protocol", "port"],
        "additionalProperties": False,
    }


def _validated(args):
    if not isinstance(args, dict):
        return None
    protocol, port = args.get("protocol"), args.get("port")
    method, path = args.get("method", "GET"), args.get("path", "/")
    host_header = args.get("host_header", "")
    timeout = args.get("timeout_seconds", 5)
    if protocol not in ("tcp", "http"):
        return None
    if not isinstance(port, int) or isinstance(port, bool) or not 1 <= port <= 65535:
        return None
    if method not in ("GET", "HEAD") or not isinstance(path, str) or not _PATH.fullmatch(path):
        return None
    if (host_header and
            (not isinstance(host_header, str) or not _HOST_HEADER.fullmatch(host_header))):
        return None
    if not isinstance(timeout, int) or isinstance(timeout, bool) or not 1 <= timeout <= 10:
        return None
    return protocol, port, method, path, host_header, timeout


def _read_response(channel):
    chunks, total = [], 0
    while total < _MAX_RESPONSE_BYTES:
        try:
            chunk = channel.recv(min(8192, _MAX_RESPONSE_BYTES - total))
        except socket.timeout:
            break
        if not chunk:
            break
        chunks.append(chunk)
        total += len(chunk)
    return b"".join(chunks), total >= _MAX_RESPONSE_BYTES


def probe(conn, args, secrets=(), opener=ssh_transport.open_client):
    checked = _validated(args)
    if checked is None:
        return {"ok": False, "error_class": "invalid_arguments"}
    protocol, port, method, path, host_header, timeout = checked
    client, connect_error = opener(conn)
    if connect_error:
        return {"ok": False, "protocol": protocol, "port": port,
                "error_class": connect_error.get("error", "connect_failed"),
                "detail": connect_error.get("detail", "")}
    channel = None
    try:
        transport = client.get_transport()
        if transport is None or not transport.is_active():
            return {"ok": False, "protocol": protocol, "port": port,
                    "error_class": "ssh_transport_inactive"}
        try:
            channel = transport.open_channel(
                "direct-tcpip", ("127.0.0.1", port), ("127.0.0.1", 0), timeout=timeout)
        except Exception as exc:  # noqa: BLE001 — class only; no Paramiko/private detail
            # A direct-tcpip rejection while the SSH transport stays active is the actual negative
            # guest observation (typically a closed listener). If the transport died, the probe
            # itself failed and must remain a wire failure rather than a completed read.
            try:
                transport_active = bool(transport.is_active())
            except Exception:  # noqa: BLE001
                transport_active = False
            return {"ok": False, "protocol": protocol, "port": port,
                    **({"probe_completed": True, "connected": False}
                       if transport_active else {}),
                    "error_class": type(exc).__name__}
        channel.settimeout(timeout)
        if protocol == "tcp":
            return {"ok": True, "protocol": "tcp", "port": port, "probe_completed": True,
                    "connected": True}

        virtual_host = host_header or "127.0.0.1:%d" % port
        request = ("%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n"
                   "User-Agent: compshare-ops-probe\r\n\r\n" %
                   (method, path, virtual_host)).encode("ascii")
        channel.sendall(request)
        raw, capped = _read_response(channel)
        head, separator, body = raw.partition(b"\r\n\r\n")
        status_line = head.split(b"\r\n", 1)[0].decode("ascii", "replace") if head else ""
        match = re.match(r"HTTP/\d(?:\.\d)?\s+(\d{3})(?:\s|$)", status_line)
        status_code = int(match.group(1)) if match else None
        body_cut = capped or len(body) > _MAX_BODY_BYTES
        body_text = guardrails.scrub_output(
            body[:_MAX_BODY_BYTES].decode("utf-8", "replace"), secrets)
        return {"ok": bool(match), "protocol": "http", "port": port, "probe_completed": True,
                "connected": True,
                "method": method, "path": path, "host_header": virtual_host,
                "status_code": status_code, "status_line": status_line,
                "body": body_text, "truncated": body_cut,
                **({"error_class": "invalid_http_response"} if not match else {})}
    except Exception as exc:  # noqa: BLE001 — return only the stable class, never remote text
        return {"ok": False, "protocol": protocol, "port": port, "probe_completed": True,
                "connected": channel is not None,
                "error_class": type(exc).__name__}
    finally:
        if channel is not None:
            try:
                channel.close()
            except Exception:  # noqa: BLE001
                pass
        client.close()
