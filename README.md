<h1 align="center">tfstray</h1>
<h3 align="center">Find what Terraform doesn't own. Find out who made it.</h3>
 
## What it is
 
`terraform plan` only sees resources inside its state file.
Anything created in the Portal, by a one-off `az` command, or by another pipeline is invisible to it, and nothing tells you who to ask about it.
 
tfstray answers one question with one read-only command:
*what is in this subscription that no Terraform state manages, and who created it?*
 
It lists every Azure resource missing from the state files you give it, drops the ones Azure created for itself, and ranks the rest by who created them, so you start with the 19 resources a person clicked into existence instead of auditing all 312.
 
tfstray never writes to your subscription or your state.
 
## Features
 
- **Unmanaged inventory** - every top-level resource and resource group in the subscription, diffed against one or more state files. Terraform `data` blocks count as *not* owned.
- **IaC coverage** - one number, `in state / scanned`, so you can watch it improve over time.
- **Platform suppression** - built-in rules for resources Azure creates on your behalf (`MC_*`, `NetworkWatcherRG`, ...), plus your own `.tfstrayignore`. Suppressed rows are always counted, never silently hidden.
- **Attribution** - the creator of each unmanaged resource, from the Activity Log.
- **Triage** - created by a person → `REVIEW`; by a service principal → `LIKELY OTHER IaC`; no record → `UNKNOWN`.
- **CI-friendly** - JSON on stdout, logs on stderr, and exit codes that tell "found problems" apart from "could not run".
## Quick Start
 
### Requirements
 
- Azure CLI, logged in with `az login`
- **Reader** role on the subscription to scan
- Terraform state as a local file, or piped from `terraform show -json`
### Install
 
```sh
go install github.com/bronzedior/tfstray/cmd/tfstray@latest
```
 
### Run
 
```sh
# one or more local state files
tfstray scan --subscription <id> --state prod.tfstate
 
# a directory of state files, as JSON
tfstray scan --subscription <id> --state ./states/ --format json
 
# remote state, piped
terraform show -json | tfstray scan --subscription <id> --state -
```
 
```
Scanned 312 | in state: 256 | unmanaged: 56 | coverage: 82%
 
  disk-test-old      Compute/disks       rg-app
  st0accountlogs     Storage/accounts    rg-logs
  rg-legacy-billing  Resources/groups    -
  ...
```
 
## How It Works
 
```
  Azure Resource Graph          your .tfstate files
  (every resource + RG)         (managed resources only)
           │                              │
           └──────────────┬───────────────┘
                          ▼
                 difference: not in any state
                          │
                          ▼
                 suppress platform-created      (planned)
                          │
                          ▼
                 Activity Log: who created it   (planned)
                          │
                          ▼
                 REVIEW / LIKELY OTHER IaC / UNKNOWN
```
 
Resource IDs are compared case-insensitively, because Azure treats them that way and Terraform providers don't always preserve casing.
Attribution only queries resources that survive the diff, so runtime tracks the number of unmanaged resources, not the subscription's total activity.
 
## Exit codes
 
| Code | Meaning |
| --- | --- |
| 0 | Ran successfully |
| 1 | Ran successfully, `--fail-on` threshold crossed *(planned)* |
| 2 | Could not run: auth failure, bad state file, API error, bad flags |
 
## Known limitations
 
- **Child resources are invisible.** Subnets and NSG rules are not separate rows in Resource Graph, so hand-made ones are never flagged.
- **Implicitly created resources may be flagged.** A VM's OS disk exists in Azure but has no ID of its own in state.
- **Attribution looks back 90 days.** Older resources show as `UNKNOWN`, not as a guessed name.
- **Service principals show as GUIDs.** Resolving names needs Microsoft Graph permissions, beyond Reader.

 