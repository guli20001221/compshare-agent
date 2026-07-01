# Frontend Smoke

Frontend source:

- Root: `F:\frontend\.worktrees\context-continuation-prod-root`
- Frame: `F:\frontend\.worktrees\context-continuation-prod-root\frame`
- Commit: `10569e7`
- Source branch ref: `origin/feature/AIAssisant-update`

Startup:

- The initial `dev-cli dev -p light-gpu frame host-app` run blocked on internal npm dependency update.
- Restarted with `--withoutUpdateDevDependences --withoutOpenBrowser --skipAutoUpdate`.
- Dev server started successfully.

Verified:

- `http://localhost:3000/` returns `200`.
- `http://localhost:3000/#/light-gpu/` returns frontend HTML.
- Port 3000 is listening.

Not verified:

- The page talking to the local PR backend.

Reason:

- In this production frontend branch, local-dev HTTP and WebSocket overrides are commented out in `frame/src/Frame/AIAssistant/service.js`.
- The page therefore uses production gateway configuration instead of `http://127.0.0.1:7429` and `ws://localhost:8090`.

Conclusion:

- The production frontend branch can be started locally.
- Full frontend-to-local-backend validation requires a frontend-supported local override path or a temporary frontend test branch.

