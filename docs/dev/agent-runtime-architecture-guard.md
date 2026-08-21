# Agent runtime architecture guard

The production Agent has one model/tool loop. The architecture guard prevents
new keyword routers, direct-dispatch decision sites and retired runtime switches
from quietly creating a second semantic control plane.

Run:

```powershell
go run ./cmd/architecture-audit
go test ./internal/architectureguard -count=1
```

`internal/architectureguard/baseline.json` is an upper bound, not a target.
Deleting a finding needs no baseline update. Additions require review showing
that the site enforces protocol syntax, authorization, security, an exact entity
format or another non-semantic invariant. Only then regenerate it with:

```powershell
go run ./cmd/architecture-audit -write
```

Do not add historical inventories or rollout checklists to production code.
Current architectural decisions belong in `CLAUDE.md`; code comments should
explain only the invariant adjacent to the implementation.
