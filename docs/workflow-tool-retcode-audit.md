# Tool/Workflow RetCode Audit

Last audited: 2026-06-26

This table tracks the runtime contract between agent workflows/tools and the
CompShare upstream API. When adding or changing a tool/workflow, update this
file together with tests.

## Workflow Matrix

| Workflow | Upstream actions | Placement source | Price gate | State/preflight gate | RetCode focus |
|---|---|---|---|---|---|
| `CreateInstanceWorkflow` | `Describe*Images`, `CheckCompShareResourceCapacity`, `GetCompShareInstanceUserPrice`, `CreateCompShareInstance` | support-zone catalog; submit keeps `Zone` and derived `Region` | required before final confirm | inventory/capacity, image shape, Pod container image | 230, 226603, 226604, 8433 |
| `StopInstanceWorkflow` | `DescribeCompShareInstance`, `StopCompShareInstance` | queried instance `Zone`; `Region` may derive from that `Zone` | no | Running only; Spot adds force | 230, 8903, 8905 |
| `StartInstanceWorkflow` | `DescribeCompShareInstance`, optional resize for no-card start, `StartCompShareInstance` | queried instance `Zone`; `Region` may derive from that `Zone` | no | Stopped only; no-card path validates support before resize/start | 230, 8442, 8443, 8445, 8905 |
| `RebootInstanceWorkflow` | `DescribeCompShareInstance`, `RebootCompShareInstance` | queried instance `Zone`; `Region` may derive from that `Zone` | no | Running only | 230, 8903, 8905 |
| `RenameInstanceWorkflow` | `DescribeCompShareInstance`, `ModifyCompShareInstanceName` | queried instance `Zone`; `Region` may derive from that `Zone` | no | instance found; name present | 230, 220, 8903 |
| `ResetPasswordWorkflow` | `DescribeCompShareInstance`, `ResetCompShareInstancePassword` | queried instance `Zone`; `Region` may derive from that `Zone` | no | VM stopped or container online; password validated/masked | 230, 8314, 8903 |
| `SetStopSchedulerWorkflow` | `DescribeCompShareInstance`, `UpdateCompShareStopScheduler` | queried instance `Zone`; `Region` may derive from that `Zone` | no | Running, non-Spot, shutdown time valid | 230, 8903, 8905 |
| `CancelStopSchedulerWorkflow` | `DescribeCompShareInstance`, `DeleteCompShareStopScheduler` | queried instance `Zone`; `Region` may derive from that `Zone` | no | instance found; any state allowed | 230, 8903 |
| `ResizeInstanceWorkflow` | `DescribeCompShareInstance`, `GetCompShareInstanceUpgradePrice`, `ResizeCompShareInstance` | queried instance `Zone`; `Region` may derive from that `Zone` | required before confirm | Stopped; at least one target CPU/GPU/memory change | 230, 8333, 8357, 8090 |
| `ResizeDiskWorkflow` | `DescribeCompShareInstance`, `DescribeCompShareSupportZone`, `CheckCompShareResizeAttachedDisk`, `GetCompShareAttachedDiskUpgradePrice`, `ResizeCompShareDisk` or `ResizeCompShareInstance` | queried instance `Zone`; Pod path resolves `zone_id/az_group` through support-zone catalog first, instance fields only fallback | required before confirm | target disk resolved; target size larger; Pod uses instance-resize disk branch | 230, 8067, 8107, 8090 |
| `ReinstallInstanceWorkflow` | `DescribeCompShareInstance`, `Describe*Images`, `ReinstallCompShareInstance` | queried instance `Zone`; `Region` may derive from that `Zone` | no | Stopped; image exists; Pod needs container image; system disk large enough for image size | 230, 8010, 8017, 8027, 8315, 226603 |
| `CreateDiskWorkflow` | `DescribeCompShareInstance`, `GetCompShareInstancePrice`, `CreateAndAttachCompshareDisk` | queried instance `Zone`; `Region` may derive from that `Zone` | required before confirm | VM only; size present | 230, 8090, 8067 |
| `CreateCustomImageWorkflow` | `DescribeCompShareInstance`, `CreateCompShareCustomImage`, `GetCompShareImageCreateProgress` | queried instance `Zone`; `Region` required before confirm | no | VM Running/Stopped; container must be Running | 210, 230, 8964, 8968 |
| `EnableNetOptimizerWorkflow` | `CheckCompShareNetOptimizer`, `SyncCompShareNetOptimizer` | support-zone catalog resolves internal region group | no | Zone required; already-enabled skips write | 230, 8433 |
| `CreateCFSWorkflow` | `GetCompShareCFSPrice`, `CreateCFS`, `DescribeCFS` | support-zone catalog; Pod zone only | required before confirm | name/size/zone valid; Pod-only; internal placement resolved | 230, 8433, 8090 |
| `ResizeCFSWorkflow` | `DescribeCFS`, `GetCompShareCFSUpgradePrice`, `ResizeCFS` | `DescribeCFS` zone id | required before confirm | target size larger than current | 230, 8433, 8090 |

## Tool Matrix

| Tool | Purpose | Placement/identity rule | Price/field rule | User-facing boundary |
|---|---|---|---|---|
| `DescribeCompShareInstance` | instance list/detail | user account only; no inventory inference | do not treat missing price fields as zero | sanitize passwords/tokens/commands |
| `DescribeAvailableCompShareInstanceTypes` | saleable types | may filter by zone; status is saleable flag, not live stock | none | do not answer "has stock" from status alone |
| `DescribeCompShareGpuInventory` | raw GPU stock | inventory is keyed by support-zone id; join via support-zone catalog | raw GPU count is not full create capacity | explain as raw GPU stock only |
| `CheckCompShareResourceCapacity` | create-capacity precheck | use concrete `Zone` and derived `Region` | capacity result decides "can create" wording | do not retry mutating calls automatically |
| `GetCompShareInstancePrice` | workflow quote helper, including data-disk create quote | use concrete zone/region for zone-specific quotes | missing quote blocks confirmation | not the main user-facing price-answer path |
| `GetCompShareInstanceUserPrice` | user-facing instance price answer and create quote | same placement as create price | returns discounted/original/list groups | ordinary price, actual price, and list-price wording are rendered from the returned groups |
| `GetCompShareInstanceUpgradePrice` | instance resize quote | source instance placement must be included | missing price blocks confirm | workflow/internal placement only |
| `GetCompShareAttachedDiskUpgradePrice` | disk resize quote | source instance placement must be included | missing price blocks confirm | workflow only |
| `GetCompShareRefundPrice` | instance refund estimate | explicit instance ids only | estimate only; no release action | read-only |
| `DescribeCompShareImages` | platform images | no static zone guarantee | image size is MB; used for reinstall disk preflight | image source only, not stock |
| `DescribeCommunityImages` | community images | no static zone guarantee | exact image still may fail dynamic zone adaptation | image source only, not stock |
| `DescribeCompShareCustomImages` | custom images | account-scoped | image status matters before reuse | do not solve upstream identity injection |
| `DescribeCompShareSharingImages` | shared images | account-scoped | same image-shape checks as other images | read-only |
| `GetCompShareInstanceMonitor` | current/history monitor | target instance ids only; max window guarded by tools policy | parse VM and Pod result shapes separately | no raw result dump |
| `DescribeCompShareJupyterToken` | Jupyter entry/token | target instance only | token must not be shown in plain text | return safe entry guidance |
| `DescribeCFS` | CFS list/detail | optional zone string; internal fields not model-filled | missing CFS zone id blocks resize | read-only |
| `GetCompShareCFSPrice` | CFS create price | Pod zone only; internal placement resolved by backend | payable value is `PriceDetails[0].Disks` | no raw `PriceDetails` card |
| `GetCompShareCFSUpgradePrice` | CFS resize price | `DescribeCFS` supplies internal zone id | missing price blocks confirm | read-only quote / workflow gate |
| `GetCompShareCFSRefundPrice` | CFS refund estimate | explicit CFS id | estimate only | no delete exposure |

## Required Review Checklist

- Mutating workflow has a state gate, a confirmation gate, and no raw destructive
  operation exposed as a normal tool.
- Existing-resource workflow uses queried resource placement; no default zone is
  allowed for an already-created resource.
- Any cost-changing operation either shows a valid price before confirmation or
  stops before confirmation.
- User/model-supplied args never include internal placement fields.
- Tests use upstream-shaped fixtures: instance type entries carry string `Zone`,
  support-zone entries carry ids, image size is MB, CFS create price is in
  `PriceDetails[0].Disks`.
- RetCode hints avoid raw internal fields and give a concrete next step.
