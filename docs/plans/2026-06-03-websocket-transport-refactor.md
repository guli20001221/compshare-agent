# SSE → WebSocket Transport Refactor

Date: 2026-06-03
Branch: codex/diagnosis-routing-optimization (or a fresh `feat/ws-transport`)
Decisions locked by user (2026-06-03):
- **Full replace**: delete SSE entirely, no parallel SSE endpoint.
- **Frame body = existing Action protocol**: reuse `{Action, SessionId, Message, ...}` inbound and the
  existing event payload structs (meta/step/token/confirmation/done) outbound, wrapped with a `type` field.

Reason: the gateway does not support SSE. Chat must stream over a gateway-initiated WebSocket.

---

## 0. Context that must survive compaction

### 0a. Corrected upstream finding (custom-image publish / user_email)
Verified against real upstream source at `F:\uhost-compshare-api-master` (NOT in the agent repo):
- `pkg/api/base.go:60` — `UserEmail string json:"user_email"`; `NewBaseRequest` (`:76-86`) does a **plain
  `json.Unmarshal` from the request body**. The backend publish task `internal/api/image/ucloud/create_compshare_custom_image.go:95`
  only checks `task.request.UserEmail == ""`.
- **So the publish interface itself accepts a body-supplied user_email — it does NOT require an auth-derived one.**
  My earlier claim ("interface only trusts auth-derived email") was WRONG.
- The backend `dispatch` (`cmd/server.go:96`) has only `Recovery` + `GWMetadata` middleware — **no signature
  verification**. `autoFillReq` (`:302`) comment: "用于内部接口，模拟外部网关的信息填充". → the backend runs
  behind a **public gateway** that verifies signatures and rewrites identity fields.
- Empirical: agent put user_email in JSON body (`usesJSONBody` experiment) and STILL got `210 Missing params
  [user_email]`. If the body reached `NewBaseRequest`, it would have bound. → the gateway strips/overwrites
  user_email based on the **signed caller identity** before forwarding. Agent signs as STS service role
  `ServiceRoleForCompshare`, which has **no associated user email** → gateway forwards empty user_email → 210.
- **Fix is in the gateway/identity layer, not the backend publish API and not agent transport.** Three options
  (all need upstream/platform): S1 map a user_email to the agent's service role / allow it to pass through;
  S2 agent calls upstream with a **user** identity (forward user `u_jwt_token` / per-user STS); S3 upstream opens
  a body-trusting path for this service-role call.
- **Changing SSE→WS does NOT affect this** — publish 210 is hop-2 (agent→upstream OpenAPI), WS is hop-1
  (gateway→agent). They are independent.

### 0b. WS gateway contract (from colleague, 2026-06-03)
Handshake is a WS upgrade; identity is in the **upgrade HTTP headers**, NOT in frames. Action is in the query.
```
r.Header.Get("X-Company-Id")      → top_organization_id
r.Header.Get("X-Organization-Id") → organization_id
r.Header.Get("X-User-Id")         → user id
r.Header.Get("X-Iam-Identity")    → IAM URN
r.Header.Get("X-Request-Id")      → request_uuid
r.Header.Get("Api-Metadata")      → "user-id=..., organization-id=..., company-id=..., request-id=..." (redundant fallback)
r.URL.Query().Get("Action")       → CreateCSAgentWS
```
⚠️ **No user_email in the WS header list.** The current SSE/body path DOES carry user_email. After WS cutover the
agent may lose body-supplied user_email entirely (only the IAM URN survives). Irrelevant for publish (gateway
ignores client user_email anyway), but **confirm with the gateway team whether WS handshake injects user_email**
if any other feature depends on it.

---

## 1. Current state (what exists today)

- Entry: `cmd/server.go` — gin, `router.POST("/", handlers.Dispatch)`, `GET /healthz`, OPTIONS + CORS.
- `internal/httpapi/dispatch.go::Dispatch` — `ParseBaseRequest(c)` (reads POST body, form or JSON), then
  switch on `base.Action`:
  - `GetCSAgentMeta` / `CreateCSAgentSession` / `GetCSAgentSession` / `SendCSAgentFeedback` → handler → `writeResult` (single JSON).
  - `SendCSAgentChat` → `handleChat` (SSE stream).
  - `ConfirmCSAgentAction` → `handleConfirm` → `confirmBroker.Resolve`.
- `internal/httpapi/baserequest.go::ParseBaseRequest` — reads identity from **body** (`top_organization_id`,
  `organization_id`, `ProjectId`, `user_email`, `request_uuid`, `Action`).
- `internal/httpapi/sse/writer.go` — `Writer.WriteEvent(event, data)` / `WriteKeepalive`.
- `internal/httpapi/handlers_chat.go::handleChat` — opens `sse.New(c.Writer)`, streams events:
  `meta` → `step`(per ReAct step) → `token`(per delta) → `confirmation`(blocks) → final `token`/done.
  - `ConfirmFunc` (`:347`): `confirmBroker.Register` → `WriteEvent("confirmation", …)` → **blocks** in
    `WaitForConfirmation(ctx, ch, 60s)`. Resolved out-of-band by a **separate** `ConfirmCSAgentAction` HTTP request.
- `internal/httpapi/confirm_broker.go` — **transport-agnostic** Register/Resolve/Cancel/WaitForConfirmation.
  Keyed by UUID + (sessionID, owner) ownership check. **Reused as-is under WS.**

## 2. Target state

One gateway-initiated WS connection per client session. All Actions become frames on that socket. Streaming
(tokens/steps) and the confirm round-trip happen on the same socket.

### 2.1 Dependency
Add `github.com/coder/websocket` (maintained successor to `nhooyr.io/websocket`; same API). `go mod tidy`.
⚠️ **nhooyr/coder CloseNow shares the Close mutex** (memory: nhooyr-closenow-blocks-on-close-mutex). Use
**goroutine-per-conn + a single total-connection-deadline context**; never "drain then CloseNow" in a way that
serializes N×timeout. One writer goroutine OR a mutex-guarded writer; readers in their own goroutine.

### 2.2 New files
- `internal/httpapi/ws/writer.go` — `Writer` wrapping `*websocket.Conn`. `WriteFrame(ctx, typ string, data any)`
  marshals `{"type": typ, ...data}` (flatten data fields to top level, like `flattenEnvelope`) and writes one
  text message. Mutex-guarded for concurrent writes. Replaces `sse.Writer`. Mirror `sse/writer_test.go` →
  `ws/writer_test.go`.
- `internal/httpapi/ws.go` — `(*Handlers).HandleWS(c *gin.Context)`:
  1. `base, err := ParseBaseRequestFromHeaders(c.Request)` — connection-scoped identity.
  2. Accept upgrade (`websocket.Accept`, with `InsecureSkipVerify`/OriginPatterns per gateway origin).
  3. `defer conn.CloseNow()`. Build `ctx, cancel := context.WithTimeout(c.Request.Context(), maxConnLifetime)`.
  4. **Read loop** (this goroutine): `conn.Read(ctx)` → parse frame JSON → `simplejson` → dispatch by `Action`:
     - `SendCSAgentChat` → spawn a **goroutine** running the WS variant of `handleChat` (so a blocked
       `ConfirmFunc` does not stall frame reading — the confirm frame must be readable while Chat blocks).
       Guard: at most one in-flight Chat per connection (the agentpool `Lease` already serializes per session;
       a second Chat frame while one is in-flight → error frame).
     - `ConfirmCSAgentAction` → `confirmBroker.Resolve(confirmationID, sessionID, base.Owner, confirmed)` →
       unblocks the in-flight Chat's `ConfirmFunc`. Reuses `handleConfirm` logic (extract to a shared helper that
       takes parsed fields, so both HTTP-legacy-removal and WS call the same core — or just call broker directly).
     - `GetCSAgentMeta` / `CreateCSAgentSession` / `GetCSAgentSession` / `SendCSAgentFeedback` → call existing
       handler, write one result frame via `WriteFrame("result", flattenEnvelope(...))`.
     - unknown Action → error frame.
  5. On read error / ctx done → cancel, return (deferred CloseNow).

### 2.3 Changed files
- `cmd/server.go`:
  - Replace `router.POST("/", handlers.Dispatch)` with `router.GET("/", handlers.HandleWS)` (gateway upgrades via
    GET with `?Action=CreateCSAgentWS`; if Action query missing/!=CreateCSAgentWS, 400).
  - Keep `GET /healthz`. Remove OPTIONS + `corsMiddleware` (WS uses Origin checks at Accept; confirm gateway origin).
  - `http.Server` ReadTimeout/WriteTimeout must NOT kill long-lived WS — set them to 0 or rely on per-conn ctx
    deadline. (Today `WriteTimeout` would cut the stream; verify and adjust.)
- `internal/httpapi/baserequest.go`: add `ParseBaseRequestFromHeaders(r *http.Request) (BaseRequest, error)`.
  - top_org from `X-Company-Id`; org from `X-Organization-Id`; request_uuid from `X-Request-Id` (gen if empty);
    Action from `r.URL.Query().Get("Action")`; UserEmail from header if present else "" (see 0b warning);
    parse `Api-Metadata` as fallback for any missing field. Keep the existing body-based `ParseBaseRequest` only if
    something still needs it; otherwise delete with SSE.
  - `buildUserContext(base)` (`handlers.go:75`) is unchanged — it already maps BaseRequest→UserContext incl. UserEmail.
- `internal/httpapi/handlers_chat.go::handleChat`: parameterize the writer. Extract the streaming core to accept a
  small `streamSink` interface { Meta/Step/Token/Confirmation/Done } implemented by the WS writer. Replace all
  `sw.WriteEvent("token"/"step"/"meta"/"confirmation", …)` with sink calls. The `ConfirmFunc` body is unchanged
  except it writes via the sink. `c.Request.Context()` → the connection ctx.
- `internal/httpapi/dispatch.go`: `Dispatch` (POST router) is **removed** along with SSE. Keep `flattenEnvelope`
  (reused by WS result frames) and `writeError`/`writeResult` only if still referenced; otherwise fold into WS.

### 2.4 Deleted
- `internal/httpapi/sse/writer.go` + `writer_test.go`.
- POST `/` route, OPTIONS, CORS middleware (unless gateway needs CORS preflight for the GET upgrade — verify).
- Body-based `ParseBaseRequest` IF nothing else uses it (grep first).

## 3. Frame protocol (concrete)

Inbound (gateway → agent), one JSON text message per frame:
```json
{ "Action": "SendCSAgentChat", "SessionId": "…", "Message": "…" }
{ "Action": "ConfirmCSAgentAction", "SessionId": "…", "ConfirmationId": "…", "Confirmed": true }
{ "Action": "CreateCSAgentSession", … }   // and GetCSAgentSession / GetCSAgentMeta / SendCSAgentFeedback
```
Outbound (agent → gateway), `type` discriminates:
```json
{ "type": "meta",         "SessionId": "…", … }
{ "type": "step",         "StepType": "tool_call", … }
{ "type": "token",        "Text": "…" }
{ "type": "confirmation", "ConfirmationId": "…", "Summary": {…}, "TimeoutSeconds": 60 }
{ "type": "result",       "Action": "GetCSAgentMeta", "RetCode": 0, … }   // non-chat single results
{ "type": "done" }                                                         // or final token + done
{ "type": "error",        "RetCode": …, "Message": "…" }
```
Payload structs already exist (`metaEvent`, `stepEvent`, `tokenEvent`, `confirmationEvent`) — keep them, just add
the `type` tag at write time.

## 4. Concurrency / lifecycle (the crux)

- One reader goroutine per connection (the `HandleWS` body). It must keep reading so a `ConfirmCSAgentAction`
  frame arrives while a Chat is mid-confirm.
- Chat runs in its own goroutine; its `ConfirmFunc` blocks on the broker channel (unchanged
  `WaitForConfirmation`). The reader receives the confirm frame and calls `broker.Resolve` → channel unblocks.
- Writes from both the Chat goroutine (tokens) and the reader (result/error frames) go through the **mutex-guarded
  WS writer** — no interleaving.
- Connection deadline: `context.WithTimeout(…, maxConnLifetime)`; also a read deadline per frame if the gateway
  keeps the socket idle. On ctx done → `conn.Close(StatusNormalClosure)` then return; `defer conn.CloseNow()` as
  backstop. **Do not** loop CloseNow under a shared mutex across many conns.
- Cancel semantics: if the socket drops mid-Chat, ctx cancels → engine stops, `ConfirmFunc`’s
  `WaitForConfirmation` returns false via `ctx.Done()`, broker entry cleaned via `defer broker.Cancel`.

## 5. Tests (Rule 9 — encode WHY)

- `ws/writer_test.go` — frame marshaling has `type` tag + flattened payload; concurrent writes don't interleave.
- `ws_test.go` integration (httptest server + coder/websocket dial):
  - handshake with `X-Company-Id`/`X-Organization-Id`/`X-Request-Id` headers + `?Action=CreateCSAgentWS` →
    accepted; missing top_org → rejected (proves identity comes from headers, not frames).
  - Chat frame → receive `meta`, ≥1 `token`, `done` (proves streaming works over WS).
  - Chat that triggers a mutating workflow → receive `confirmation` frame; send `ConfirmCSAgentAction` frame on the
    **same** socket → workflow proceeds (proves single-socket confirm round-trip; this is the behavior that
    justifies the refactor — confirm no longer needs a second HTTP request).
  - Socket close mid-confirm → no goroutine leak, broker entry cleaned (proves cancel path).
- Existing handler unit tests (`handlers_chat_*_test.go`, session/meta/feedback) stay green — they test handler
  logic, not transport. Update only the ones that assert SSE framing.
- `go test ./... -count=1` green; `internal/entity -race` unaffected.

## 6. Build order (each step compiles + tests green)

1. Add `coder/websocket` dep; `go mod tidy`; commit (dep only).
2. `ParseBaseRequestFromHeaders` + unit test (header→BaseRequest). No wiring yet.
3. `ws/writer.go` + test. No wiring yet.
4. `handleChat` → `streamSink` extraction; SSE writer implements the sink; **all existing SSE tests still green**
   (pure refactor, no behavior change). This de-risks the streaming split before touching transport.
5. `ws.go::HandleWS` read loop + dispatch; wire `GET /` in `cmd/server.go`; keep POST `/` temporarily for test
   parity ONLY within this step.
6. Integration tests (§5) green.
7. Delete SSE (`sse/`), POST `/`, OPTIONS/CORS, body `ParseBaseRequest` if unused. Full suite green.
8. CLI regression unaffected (CLI path doesn't use httpapi). HTTP smoke: dial WS, run one Chat end-to-end.

## 7. Open questions to confirm with gateway team (before step 5)
- WS handshake header names exact casing (`X-Company-Id` etc.) and whether `Api-Metadata` is the canonical source.
- Does the WS handshake inject `user_email`? (affects any email-dependent feature; NOT publish.)
- Origin value(s) for `websocket.Accept` OriginPatterns.
- Idle/keepalive expectations: does the gateway send pings, or must the agent? (coder/websocket has built-in ping.)
- Is there a max message size the gateway enforces?

## 8. Explicitly out of scope
- Custom-image publish 210 (hop-2 upstream/gateway identity issue; see §0a). Tracked separately; WS does not fix it.
- Any change to engine/agentpool/persistence — transport-only refactor.

---

## 9. AS-BUILT (2026-06-03) — what actually shipped, and deviations from the plan above

Implemented and green (`go build ./...`, `go vet ./...`, full `go test ./...` all pass with
`COMPSHARE_PROJECT_ID` set). Key corrections to the assumptions in §1–§8:

**D1 — The frontend was ALREADY converted.** `F:\frontend\frame` (the `console/frame` repo, MR236) is on
branch `codex/mr236-agent-integration`; commit `2ffb573 feat(AIAssistant): stream chat over WebSocket gateway
instead of SSE` sits ON TOP of the MR236 SSE base. `frame/src/Frame/AIAssistant/service.js` already opens
`wss://…/?Action=CreateCSAgentWS`, sends `SendCSAgentChat` on open, handles meta/step/token/confirmation/done/
error frames, and sends `ConfirmCSAgentAction` back over the SAME socket. So **no backend-driven frontend change
was needed** — the backend had to match the already-shipped frontend contract.

**D2 — Frame discriminator is `event`, NOT `type`.** service.js switches on `f.event` (line 88). ws.Writer
emits `{"event": <name>, ...flattenedPayload}`. (The plan's `type` was wrong; caught by reading service.js.)

**D3 — POST `/` is KEPT, not replaced.** The frontend still sends GetCSAgentMeta / CreateCSAgentSession /
GetCSAgentSession / SendCSAgentFeedback over HTTP POST (`queryService.query`). Only chat + confirm moved to WS.
So `cmd/server.go` now has BOTH `router.GET("/", handlers.HandleWS)` and `router.POST("/", handlers.Dispatch)`.
`SendCSAgentChat` was removed from Dispatch's switch (it is WS-only now).

**D4 — SSE fully deleted.** `internal/httpapi/sse/` removed; `handleChat` (gin entry) removed; chat-over-POST
tests migrated to a transport-agnostic `recordingSink` (`chat_testsink_test.go`) driving `prepareChat` +
`chatStream` directly via `runChatJSON`.

**D5 — handleChat split into `prepareChat` + `chatStream`.** `prepareChat(ctx, base, sessionID, message,
imageDataURL) (*chatPrep, *APIError)` does validation/OCR/lease/hydration/persistence; `chatStream(streamCtx,
streamWriter, base, *chatPrep)` runs the turn and streams. Both the WS handler and tests call these.

**D6 — confirm round-trip reuses the existing ConfirmBroker unchanged.** It was already transport-agnostic. The
WS read loop routes a `ConfirmCSAgentAction` frame to `confirmBroker.Resolve(id, sessionID, owner, confirmed)`,
unblocking the in-flight turn's `ConfirmFunc` (which still blocks on `WaitForConfirmation`). The owner check
(prevents cross-tenant hijack) survives the WS path — covered by `TestWS_Confirm_WrongOwnerRejected`.

### Files (as-built)
- NEW `internal/httpapi/ws/writer.go` (+`writer_test.go`) — `ws.Writer` with `WriteEvent`/`WriteKeepalive`;
  `event` discriminator; mutex-guarded (Ping shares the write side → also takes the lock).
- NEW `internal/httpapi/ws.go` — `HandleWS`: header-identity upgrade, read loop, Chat-in-goroutine so a blocked
  confirm doesn't stall reads, `chatActive` guard, deferred cancel+drain before CloseNow. `maxWSConnLifetime=10m`.
- NEW `internal/httpapi/ws_integration_test.go` — handshake identity reject, chat stream, confirm round-trip on
  same socket, wrong-owner reject. Uses real `coder/websocket` dial against httptest server.
- NEW `internal/httpapi/chat_testsink_test.go` — `recordingSink` + `runChatJSON` test harness.
- CHG `internal/httpapi/baserequest.go` — `ParseBaseRequestFromHeaders` (+`parseAPIMetadata`/`headerUint32`/
  `firstNonEmpty`). Body-based `ParseBaseRequest` KEPT (POST Actions still use it).
- CHG `internal/httpapi/handlers_chat.go` — split; SSE entry removed; event structs/comments de-SSE'd.
- CHG `internal/httpapi/dispatch.go` — `SendCSAgentChat` case removed.
- CHG `cmd/server.go` — added `GET /`; kept `POST /`; documented WriteTimeout=0 rationale (unchanged value).
- CHG `go.mod`/`go.sum` — `github.com/coder/websocket v1.8.14`.
- DEL `internal/httpapi/sse/` (writer.go + writer_test.go).
- CHG 5 chat test files — migrated off SSE body-scraping to `recordingSink` (24 test funcs).

### Still open / not done here
- `-race` on httpapi could not run in this Windows session (no CGO/gcc); the read-loop/goroutine concurrency is
  unverified by the race detector locally. Run `go test ./internal/httpapi/ -race` on the Linux CI box.
- §7 gateway questions still stand (Origin policy, ping/keepalive expectations, max message size, whether the
  handshake injects user_email). InsecureSkipVerify is currently set on Accept because the gateway is a trusted
  server-side origin; revisit if that assumption is wrong.
- Not committed (user self-manages merges).
