"""Bounded inspection of selected diagnostic environment variables for one remote process."""

import guardrails
import ssh_transport

_MAX_ENV_BYTES = 64 << 10
ALLOWED_NAMES = (
    "CUDA_VISIBLE_DEVICES",
    "NVIDIA_VISIBLE_DEVICES",
    "NVIDIA_DRIVER_CAPABILITIES",
    "CUDA_HOME",
    "CONDA_DEFAULT_ENV",
    "CONDA_PREFIX",
    "VIRTUAL_ENV",
    "PYTHONHOME",
    "PYTHONPATH",
    "LD_LIBRARY_PATH",
    "OMP_NUM_THREADS",
    "LOCAL_RANK",
    "RANK",
    "WORLD_SIZE",
)

TOOL_DESCRIPTION = (
    "Read only caller-selected diagnostic environment variables from one process in the target "
    "instance. Use it for CUDA visibility, Python/conda/venv identity, dynamic-library paths and "
    "distributed rank evidence instead of dumping /proc/PID/environ. PID and names are explicit; "
    "names are limited to the schema allowlist, arbitrary environment variables and credentials "
    "cannot be requested, output is bounded and credential-shaped values are redacted."
)


def input_schema():
    return {
        "type": "object",
        "properties": {
            "pid": {
                "type": "integer", "minimum": 1, "maximum": 2147483647,
                "description": "Numeric PID in the target instance.",
            },
            "names": {
                "type": "array", "minItems": 1, "maxItems": len(ALLOWED_NAMES),
                "uniqueItems": True,
                "items": {"type": "string", "enum": list(ALLOWED_NAMES)},
                "description": "Diagnostic variables to return when present.",
            },
        },
        "required": ["pid", "names"],
        "additionalProperties": False,
    }


def _error(error_class, **fields):
    return {"ok": False, "error_class": error_class, **fields}


def _validated_args(args):
    if not isinstance(args, dict):
        return None, _error("invalid_arguments")
    pid, names = args.get("pid"), args.get("names")
    if isinstance(pid, bool) or not isinstance(pid, int) or not 1 <= pid <= 2147483647:
        return None, _error("invalid_pid")
    if (not isinstance(names, list) or not names or len(names) > len(ALLOWED_NAMES)
            or any(not isinstance(name, str) or name not in ALLOWED_NAMES for name in names)
            or len(set(names)) != len(names)):
        return None, _error("invalid_names", pid=pid)
    return {"pid": pid, "names": names}, None


def _parse(data, requested, secrets=()):
    values = {}
    for entry in data.split(b"\0"):
        if not entry or b"=" not in entry:
            continue
        raw_name, raw_value = entry.split(b"=", 1)
        try:
            name = raw_name.decode("ascii")
            value = raw_value.decode("utf-8")
        except UnicodeDecodeError:
            continue
        if name in requested and name not in values:
            values[name] = guardrails.scrub_output(value, secrets)
    return values


def read(conn, args, secrets=(), opener=ssh_transport.open_client):
    """Read one bounded /proc/PID/environ and return only schema-allowlisted names."""
    spec, err = _validated_args(args)
    if err:
        return err
    client, connect_error = opener(conn)
    if connect_error:
        return _error(connect_error.get("error", "connect_failed"),
                      detail=connect_error.get("detail", ""))
    sftp = None
    path = "/proc/%d/environ" % spec["pid"]
    try:
        sftp = client.open_sftp()
        with sftp.file(path, "rb") as handle:
            data = handle.read(_MAX_ENV_BYTES + 1)
        if len(data) > _MAX_ENV_BYTES:
            return _error("environment_too_large", pid=spec["pid"])
        values = _parse(data, set(spec["names"]), secrets)
        return {
            "ok": True,
            "pid": spec["pid"],
            "values": {name: values[name] for name in spec["names"] if name in values},
            "missing": [name for name in spec["names"] if name not in values],
        }
    except FileNotFoundError:
        return _error("process_not_found", pid=spec["pid"])
    except PermissionError:
        return _error("permission_denied", pid=spec["pid"])
    except Exception as exc:  # noqa: BLE001 — expose class, never private remote detail
        return _error("sftp_read_failed", pid=spec["pid"], detail=type(exc).__name__)
    finally:
        if sftp is not None:
            try:
                sftp.close()
            except Exception:  # noqa: BLE001
                pass
        try:
            client.close()
        except Exception:  # noqa: BLE001
            pass
