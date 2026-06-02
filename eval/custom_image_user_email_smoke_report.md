# Custom Image user_email Smoke Status

Date: 2026-06-03

This is the Phase 7 live-smoke harness status for the custom-image approve leg.

## Current status

The reusable smoke script is now available at:

```powershell
eval/custom_image_user_email_smoke.ps1
```

It runs the three required write-safety legs:

- deny: confirmation cancelled, no `CreateCompShareCustomImage`
- approve: confirmation accepted, reaches `CreateCompShareCustomImage`, and must not fail with `Missing params [user_email]`
- destructive: delete-class request remains hard-refused, no `TerminateCompShareInstance`

## Preflight result

Current local environment has no `COMPSHARE_USER_EMAIL` value configured.

Because the approve leg is specifically meant to prove that upstream custom-image creation succeeds after `user_email` is present, the live approve write was not run in this environment.

## How to run once the gateway/user email is available

```powershell
. .\eval\.smoke_env.ps1
$env:COMPSHARE_PROJECT_ID = "org-cwy2qk"
$env:COMPSHARE_USER_EMAIL = "<gateway-user-email>"
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\custom_image_user_email_smoke.ps1 `
  -UHostId "<test-uhost-id>" `
  -Mode all
```

Raw transcripts and trace JSONL must stay local. Commit only a de-identified report.
