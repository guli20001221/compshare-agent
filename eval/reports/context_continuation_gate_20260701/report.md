# Context Continuation Gate Report

Date: 2026-07-01

## Scope

This run validates `COMPSHARE_CONTEXT_CONTINUATION=1` without changing the default value.

Agent branch: `codex/context-continuation-gate`  
Agent commit: `9e399460b4ed4509533b50cc6347c1fd4fac9da2`  
Frontend source: `feature/AIAssisant-update` local ref `10569e7`

## Automated Checks

Passed:

- `go test ./internal/engine ./internal/workflow ./internal/intent ./internal/httpapi -count=1`
- `go test ./... -count=1`
- `git diff --check`

## Local Stack

Backend and WebSocket gateway started successfully:

- Backend: `http://127.0.0.1:7429`
- WebSocket gateway: `ws://localhost:8090`
- `COMPSHARE_CONTEXT_CONTINUATION=1`

Frontend dev server started after skipping dependency auto-update:

- URL: `http://localhost:3000/#/light-gpu/`
- Port 3000 is listening.
- `http://localhost:3000/` returns the frontend HTML.

Frontend limitation:

- The production frontend branch currently has the local `AGENT_HTTP_DEV` / `AGENT_WS_DEV` path commented out in `frame/src/Frame/AIAssistant/service.js`.
- Therefore the local page does not connect to this local PR backend without a frontend code change. The backend behavior below was verified through the same WebSocket protocol using `wsgateway`.

## WebSocket Behavior Checks

### Instance Context

Sequence:

1. `我有哪些实例`
2. `第1台 GPU 忙不忙`
3. `帮我重启它`

Result: passed.

- The second turn selected the first listed instance and called monitor query.
- The third turn reused the same instance and emitted `RebootInstanceWorkflow` confirmation.
- Confirmation was rejected in the probe, so no reboot was executed.

### Create Disk Missing Size

Sequence:

1. `我有哪些实例`
2. select a VM instance
3. `给这台加一块数据盘`
4. `200G`

Result: partially passed.

- The missing disk size was captured as a pending task.
- `200G` resumed `CreateDiskWorkflow`.
- Execution stopped before confirmation because price was not returned: `未获取到价格，无法安全确认`.
- This is the expected safety behavior when pricing is unavailable.

### Resize Disk Missing Target Size

Sequence:

1. `我有哪些实例`
2. select a VM instance
3. `把这台系统盘扩一下`
4. `200G`

Result: passed.

- The missing target size was captured as a pending task.
- `200G` resumed `ResizeDiskWorkflow`.
- The workflow queried instance, support zone, capacity check, and price.
- A confirmation card was emitted.
- Confirmation was rejected in the probe, so no disk resize was executed.

### Resize Instance Missing Spec

Sequence:

1. `我有哪些实例`
2. select a 4090 instance
3. `把这台改配一下`
4. `4C8G`

Result: continuation passed; target spec rejected by business validation.

- `4C8G` resumed `ResizeInstanceWorkflow`.
- The selected 4090 instance does not support `4C/8GB`; valid options were `16C/64GB` and `16C/94GB`.
- No confirmation was emitted, which is correct for an invalid target.

### Negative Samples Without Context

Fresh sessions:

- `200G`
- `4C8G`
- `那5090呢`
- `算了，4090多少钱`

Result: passed.

- None of these triggered a write operation.
- `算了，4090多少钱` returned a price answer.

### Create Follow-Up

Sequences tried:

- `在华北一C用 PyTorch 开 4090` -> `那华北二A呢`
- `在华北一C用 PyTorch 开 4090` -> `那5090呢`
- `在华北一C用最新pytorch给我开一台` -> `那华北二A呢`

Result: not fully covered by this live environment.

- The first turn now reaches a creation confirmation card instead of failing before confirmation.
- The probe rejects the confirmation, which correctly clears the pending context.
- Because no recoverable failure frame remained, these runs do not prove failure-follow-up continuation.
- Existing unit tests cover the frame continuation path; a live test needs a reproducible pre-confirm failure such as a zone/model/stock mismatch.

## Verdict

Do not enable `COMPSHARE_CONTEXT_CONTINUATION` by default yet.

The backend/WS validation is good for the first batch:

- instance selection continuation works;
- add-disk, resize-disk, and resize-instance pending parameter continuation works;
- no-context short replies do not trigger writes;
- write operations still require a confirmation card or stop before confirmation when price/spec validation fails.

Remaining blockers before default-on:

1. A true frontend-to-local-backend run needs either frontend local-dev routing restored or another supported way to point the production branch at local `127.0.0.1:7429` and `localhost:8090`.
2. Create failure follow-up needs a live case that fails before confirmation but leaves a recoverable context frame.
3. `4C8G` should be retested with an instance type that actually supports that target, or the acceptance case should use a valid target for the selected instance.

