# SecretBoundary Setup

This repo does not allow real CompShare or LLM secrets in YAML config files.
Runtime config files should keep `${ENV_VAR}` placeholders, and local secrets
must be injected through process environment variables before starting the CLI.

## One-Time Setup

Enable the versioned pre-commit hook:

```bash
git config core.hooksPath .githooks
```

The hook runs `scripts/secret_scan.ps1 -Staged` and blocks known secret-bearing
local config files such as `deploy/conf/agent.yaml`,
`eval/shadow_qa/**/agent.yaml`, `eval/shadow_qa/**/shadow_qa_agent.yaml`, and
plain `.env` files.

## Local E2E Secrets

Copy the example file and fill real values only in the local copy:

```powershell
Copy-Item eval\shadow_qa\.env.example eval\shadow_qa\.env.local
```

`eval/shadow_qa/.env.local` is gitignored. Use this format:

```env
COMPSHARE_PUBLIC_KEY=your-public-key
COMPSHARE_PRIVATE_KEY=your-private-key
LLM_API_KEY=your-llm-key

# Usually leave unset. The engine calls GetProjectList during Init when
# deploy/conf/agent.yaml.example keeps project_id empty.
# COMPSHARE_PROJECT_ID=your-project-id
```

Do not put these values into `deploy/conf/agent.yaml` or any `agent.yaml`
under `eval/shadow_qa`.

## Running The CLI On Windows

```powershell
.\scripts\load_env.ps1 -Path .\eval\shadow_qa\.env.local
.\agent.exe cli --config .\deploy\conf\agent.yaml.example
```

From `cmd.exe`:

```cmd
powershell -NoProfile -ExecutionPolicy Bypass -Command "cd F:\compshare-agent; .\scripts\load_env.ps1 -Path .\eval\shadow_qa\.env.local; .\agent.exe cli --config .\deploy\conf\agent.yaml.example"
```

## macOS / Linux Equivalent

`eval/shadow_qa/.env.local` uses simple `KEY=value` lines, so shells can load it
without the PowerShell helper:

```bash
set -a
. eval/shadow_qa/.env.local
set +a
./agent cli --config deploy/conf/agent.yaml.example
```

## ProjectId

`project_id` is optional. Keep it empty for the normal path:

```yaml
project_id: ""
```

The engine will call `GetProjectList` at startup and pick the default project.
Only set `COMPSHARE_PROJECT_ID` when you need to force a specific project or
when `GetProjectList` is unavailable. If you do force it, change the config to:

```yaml
project_id: "${COMPSHARE_PROJECT_ID}"
```

Only strict `${ENV_VAR}` placeholders are supported. Default fallback syntax
such as `${COMPSHARE_PROJECT_ID:-default}` is intentionally rejected.

## Legacy Eval Configs

Older local `eval/shadow_qa/**/agent.yaml` files may contain literal keys and
will fail under the new loader with `literal secrets are not allowed`. Migrate
them by replacing real values with placeholders:

```yaml
public_key: "${COMPSHARE_PUBLIC_KEY}"
private_key: "${COMPSHARE_PRIVATE_KEY}"
llm:
  api_key: "${LLM_API_KEY}"
```

Then load `eval/shadow_qa/.env.local` before running the case.
