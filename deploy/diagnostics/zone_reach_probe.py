#!/usr/bin/env python3
"""Zone SSH-reachability probe. MEASURE ONLY.

Reads a JSON list of targets on stdin, TCP-connects to each candidate address and
reads the SSH banner. Never authenticates, never sends a byte other than what the
banner read requires, never mutates anything.

Runs identically on a laptop and inside the production cluster; the vantage is
recorded in every row so two runs can be compared.

Target shape (all optional except id/zone/port):
  {"id","zone","kind":"uhost"|"pod","host","port",
   "private_ip","vpc_id","region","region_id"}

Output: one JSON object per line on stdout.
"""

import json
import os
import socket
import sys
import time
import urllib.request

CONNECT_TIMEOUT = 4.0  # matches sshops.perCandidateDialTimeout
BANNER_TIMEOUT = 4.0


def classify(exc):
    """Name the failure the way internal/sshops/dial_candidates.go does."""
    if isinstance(exc, socket.gaierror):
        return "dns-failed"
    if isinstance(exc, socket.timeout) or isinstance(exc, TimeoutError):
        return "timeout"
    if isinstance(exc, ConnectionRefusedError):
        return "refused"
    if isinstance(exc, OSError):
        # WSAETIMEDOUT / ETIMEDOUT arrive as plain OSError on some stacks.
        if getattr(exc, "errno", None) in (10060, 110):
            return "timeout"
        if getattr(exc, "errno", None) in (10061, 111):
            return "refused"
        if getattr(exc, "errno", None) in (10051, 101, 113, 10065):
            return "unreachable"
        return "error"
    return "error"


def probe(host, port, label):
    """One TCP connect plus a banner read. Returns a result dict."""
    row = {"label": label, "host": host, "port": port}
    t0 = time.monotonic()
    sock = None
    try:
        # getaddrinfo separately so a DNS failure is not reported as a connect failure.
        infos = socket.getaddrinfo(host, port, 0, socket.SOCK_STREAM)
        row["resolved"] = sorted({i[4][0] for i in infos})
        family, socktype, proto, _, sockaddr = infos[0]
        sock = socket.socket(family, socktype, proto)
        sock.settimeout(CONNECT_TIMEOUT)
        sock.connect(sockaddr)
        row["tcp"] = "open"
        row["tcp_ms"] = round((time.monotonic() - t0) * 1000, 1)
    except Exception as exc:  # noqa: BLE001 - the class IS the measurement
        row["tcp"] = "failed"
        row["tcp_ms"] = round((time.monotonic() - t0) * 1000, 1)
        row["error_class"] = classify(exc)
        row["error"] = f"{type(exc).__name__}: {exc}"
        if sock is not None:
            sock.close()
        return row

    # A shared ingress can accept TCP and then never speak. The banner is what
    # separates "something answered" from "sshd answered".
    t1 = time.monotonic()
    try:
        sock.settimeout(BANNER_TIMEOUT)
        data = sock.recv(256)
        row["banner_ms"] = round((time.monotonic() - t1) * 1000, 1)
        text = data.decode("ascii", "replace").strip()
        row["banner"] = text[:120]
        row["is_sshd"] = text.startswith("SSH-")
    except Exception as exc:  # noqa: BLE001
        row["banner_ms"] = round((time.monotonic() - t1) * 1000, 1)
        row["banner"] = None
        row["is_sshd"] = False
        row["banner_error_class"] = classify(exc)
        row["banner_error"] = f"{type(exc).__name__}: {exc}"
    finally:
        sock.close()
    return row


def transform_ipv4_to_ipv6(gateway_url, region_id, private_ip, vpc_id, timeout=10):
    """Same unsigned internal-gateway call the product makes (tools/uvpc.go)."""
    body = json.dumps({
        "Backend": "UVPCFEGO",
        "Action": "TransformIPv4ToIPv6",
        "RegionId": int(region_id),
        "ip": private_ip,
        "VPCId": vpc_id,
    }).encode()
    req = urllib.request.Request(
        gateway_url, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        payload = json.loads(resp.read().decode("utf-8", "replace"))

    # A non-zero RetCode is raised, not returned as "no mapping". The first run of
    # this probe read the address out of the wrong key and a SUCCESSFUL call came
    # back looking exactly like an unreachable zone, which is the one confusion
    # this whole measurement exists to avoid.
    ret = payload.get("RetCode")
    if ret not in (0, None):
        raise RuntimeError("RetCode=%s %s" % (ret, payload.get("Message")))

    # "IpV6" is the wire's spelling -- see internal/tools/uvpc.go, which unmarshals
    # exactly this field. The others are accepted only as a courtesy; if one of them
    # ever hits, the product would already be broken.
    for key in ("IpV6", "IPv6", "Ipv6", "ipv6", "ip", "IP", "Ip"):
        value = payload.get(key)
        if isinstance(value, str) and value.strip():
            return value.strip(), payload
    raise RuntimeError("RetCode=0 but no address field; keys=%s" % sorted(payload))


def public_v6_candidates(prefix, ipv4):
    """Reproduce sshops.publicIPv6Candidates: the simple and the RFC 6052 forms."""
    if not prefix or not ipv4:
        return []
    try:
        base = bytearray(socket.inet_pton(socket.AF_INET6, prefix))
        b = socket.inet_pton(socket.AF_INET, ipv4)
    except OSError:
        return []
    simple = bytearray(base)
    for i in range(4):
        simple[12 + i] |= b[i]
    out = [("public-v6-simple", socket.inet_ntop(socket.AF_INET6, bytes(simple)))]
    if all(x == 0 for x in base[6:]):  # only defined for a /48
        rfc = bytearray(base)
        rfc[6:9] = b[0:3]
        rfc[9] = 0
        rfc[10] = b[3]
        out.append(("public-v6-rfc6052", socket.inet_ntop(socket.AF_INET6, bytes(rfc))))
    return out


def main():
    # Config comes from the environment when the PROGRAM itself is on stdin
    # (`kubectl exec -i ... -- python -`), which is how the in-cluster run works.
    raw = os.environ.get("ZONE_REACH_CONFIG")
    cfg = json.loads(raw) if raw else json.load(sys.stdin)
    vantage = cfg.get("vantage", "unknown")
    gateway = cfg.get("gateway_url") or ""
    prefix = cfg.get("public_ipv6_prefix") or ""
    stamp = cfg.get("stamp") or ""

    for target in cfg["targets"]:
        candidates = []
        notes = []

        if target.get("kind") == "pod":
            # Pods are never rewritten: no VPCId to map, and the advertised host is
            # a DNS name, so resolveDialHost returns it untouched.
            candidates.append(("pod-advertised", target["host"], target["port"]))
        else:
            internal = None
            if gateway and target.get("private_ip") and target.get("vpc_id"):
                try:
                    internal, _ = transform_ipv4_to_ipv6(
                        gateway, target["region_id"], target["private_ip"], target["vpc_id"])
                except Exception as exc:  # noqa: BLE001
                    notes.append(f"transform failed: {type(exc).__name__}: {exc}")
            elif not gateway:
                notes.append("no gateway url on this vantage; internal-ipv6 not resolvable")
            else:
                # Say WHICH input was missing. "no mapping" with no reason is what
                # made the first run unreadable.
                missing = [k for k in ("private_ip", "vpc_id") if not target.get(k)]
                notes.append("target carries no %s; cannot ask for a mapping" % "/".join(missing))
            if internal:
                candidates.append(("internal-ipv6", internal, target["port"]))
            else:
                notes.append("internal-ipv6 produced no mapping")
            for label, host in public_v6_candidates(prefix, target.get("host")):
                candidates.append((label, host, target["port"]))
            # Probed as a CONTROL only; production never dials it.
            candidates.append(("advertised-eip-CONTROL", target["host"], target["port"]))

        for label, host, port in candidates:
            row = probe(host, port, label)
            row.update({
                "stamp": stamp,
                "vantage": vantage,
                "instance_id": target["id"],
                "zone": target["zone"],
                "kind": target.get("kind"),
                "notes": notes,
                "probed_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
            })
            print(json.dumps(row, ensure_ascii=False), flush=True)


if __name__ == "__main__":
    main()
