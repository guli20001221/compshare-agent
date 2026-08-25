"""Structured TCP/HTTP probe of the remote guest's loopback through the SSH transport."""
import re
import socket

import guardrails
import ssh_transport

_HOST_HEADER = re.compile(r"[A-Za-z0-9.-]+(?::\d+)?")
_PATH = re.compile(r"/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*")
_MAX_RESPONSE_BYTES = 32768
_MAX_BODY_BYTES = 8192
_MAX_AUTHORIZATION_LENGTH = 2048
_INPUT_FIELDS = {
    "protocol", "port", "method", "path", "host_header", "authorization", "timeout_seconds",
}
_HTTP_CALL_EXAMPLE = {"protocol": "http", "port": 8000, "path": "/health"}

TOOL_DESCRIPTION = (
    "Probe a TCP listener or HTTP GET/HEAD on loopback inside the selected remote guest, through "
    "the existing SSH transport and without requiring curl/wget/python on the guest. Use it to "
    "separate process/listener/application health from an external platform route. The destination "
    "host is fixed to 127.0.0.1; the tool accepts only a port, bounded path, and optional literal "
    "Host header for virtual-host diagnostics. For a caller-requested API check it can send one "
    "exact user-provided Authorization value; that value is never returned or shown in activity. "
    "It cannot reach public/private network hosts, send request bodies, write files, or change the guest. "
    "For HTTP pass separate fields like protocol=http, port=8000, path=/health; never pass a URL.")


def input_schema():
    return {
        "type": "object",
        "properties": {
            "protocol": {
                "type": "string", "enum": ["tcp", "http"],
                "description": "Lowercase http for GET/HEAD, or tcp for connect-only.",
            },
            "port": {
                "type": "integer", "minimum": 1, "maximum": 65535,
                "description": "Guest loopback TCP port as an integer, not a URL or string.",
            },
            "method": {"type": "string", "enum": ["GET", "HEAD"], "default": "GET"},
            "path": {"type": "string", "minLength": 1, "maxLength": 512, "default": "/"},
            "host_header": {"type": "string", "minLength": 1, "maxLength": 253,
                            "description": "Optional literal virtual host; never an address to dial."},
            "authorization": {
                "type": "string", "minLength": 1, "maxLength": _MAX_AUTHORIZATION_LENGTH,
                "description": (
                    "Optional exact Authorization header value supplied by the user, for example "
                    "Bearer <key>. It is sent only to guest loopback and never returned."
                ),
            },
            "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": 10, "default": 5},
        },
        "required": ["protocol", "port"],
        "additionalProperties": False,
    }


def _invalid_arguments(args, invalid_fields):
    """Return actionable schema feedback without reflecting any caller-provided value."""
    result = {
        "ok": False,
        "error_class": "invalid_arguments",
        "invalid_fields": sorted(set(invalid_fields)),
        "expected_http_call": dict(_HTTP_CALL_EXAMPLE),
    }
    if isinstance(args, dict):
        unknown_count = len(set(args) - _INPUT_FIELDS)
        if unknown_count:
            result["unknown_field_count"] = unknown_count
    return result


def _validated(args):
    if not isinstance(args, dict):
        return None, _invalid_arguments(args, ["arguments"])
    protocol, port = args.get("protocol"), args.get("port")
    method, path = args.get("method", "GET"), args.get("path", "/")
    host_header = args.get("host_header", "")
    authorization = args.get("authorization", "")
    timeout = args.get("timeout_seconds", 5)
    invalid = []
    if protocol not in ("tcp", "http"):
        invalid.append("protocol")
    if not isinstance(port, int) or isinstance(port, bool) or not 1 <= port <= 65535:
        invalid.append("port")
    if method not in ("GET", "HEAD") or not isinstance(path, str) or not _PATH.fullmatch(path):
        invalid.extend(name for name, bad in (
            ("method", method not in ("GET", "HEAD")),
            ("path", not isinstance(path, str) or not _PATH.fullmatch(path)),
        ) if bad)
    if (host_header and
            (not isinstance(host_header, str) or not _HOST_HEADER.fullmatch(host_header))):
        invalid.append("host_header")
    if (authorization and
            (not isinstance(authorization, str) or len(authorization) > _MAX_AUTHORIZATION_LENGTH
             or authorization != authorization.strip()
             or any(ord(ch) < 0x20 or ord(ch) >= 0x7f for ch in authorization))):
        invalid.append("authorization")
    if protocol == "tcp" and authorization:
        invalid.append("authorization")
    if not isinstance(timeout, int) or isinstance(timeout, bool) or not 1 <= timeout <= 10:
        invalid.append("timeout_seconds")
    if set(args) - _INPUT_FIELDS:
        invalid.append("unknown_fields")
    if invalid:
        return None, _invalid_arguments(args, invalid)
    return (protocol, port, method, path, host_header, authorization, timeout), None


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
    checked, validation_error = _validated(args)
    if validation_error is not None:
        return validation_error
    protocol, port, method, path, host_header, authorization, timeout = checked
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
        authorization_line = "Authorization: %s\r\n" % authorization if authorization else ""
        request = ("%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n"
                   "User-Agent: compshare-ops-probe\r\n%s\r\n" %
                   (method, path, virtual_host, authorization_line)).encode("ascii")
        channel.sendall(request)
        raw, capped = _read_response(channel)
        head, separator, body = raw.partition(b"\r\n\r\n")
        status_line = head.split(b"\r\n", 1)[0].decode("ascii", "replace") if head else ""
        match = re.match(r"HTTP/\d(?:\.\d)?\s+(\d{3})(?:\s|$)", status_line)
        status_code = int(match.group(1)) if match else None
        body_cut = capped or len(body) > _MAX_BODY_BYTES
        auth_parts = authorization.split(None, 1) if authorization else []
        response_secrets = tuple(secrets) + tuple(
            item for item in (authorization, auth_parts[-1] if auth_parts else "") if item)
        body_text = guardrails.scrub_output(
            body[:_MAX_BODY_BYTES].decode("utf-8", "replace"), response_secrets)
        return {"ok": bool(match), "protocol": "http", "port": port, "probe_completed": True,
                "connected": True,
                "method": method, "path": path, "host_header": virtual_host,
                "authorization_sent": bool(authorization),
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
