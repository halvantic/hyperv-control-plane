# Contributing to the Ballast agent

Read this before writing code. It's the standing brief for anyone working in
this repository, including an AI coding assistant.

## Mental model: desired state, reconcile, autonomy

Borrow the Kubernetes kubelet pattern:

- A centre (not in this repository) holds **desired state**. It never
  imperatively drives this agent — it only ever hands over what a host,
  cluster, or VM should look like.
- The agent compares **actual** to **desired** and emits idempotent jobs to
  close the gap. Applying the same spec twice is always a no-op.
- The agent caches the **last-honoured desired state** in an embedded store
  (`agent/store`). Losing the centre never changes intent; the agent simply
  keeps enforcing what it was last given, and reports itself as running
  autonomously.
- `Generation` (set by the centre) and `ObservedGeneration` (reported by this
  agent) tell you whether an object is settled.

This is the load-bearing idea of the whole product. Protect it. Nothing in
this repository should ever make the agent's behaviour depend on the centre
being reachable *right now* — a host that can't phone home is meant to keep
working, not degrade.

## Non-negotiables

- **Idempotency** on every reconcile operation. No operation may assume it
  runs exactly once.
- **The agent never invents intent.** It only enforces the last desired state
  it was given. This is what prevents config split-brain when the centre is
  offline.
- **Never reimplement cluster quorum.** Use `Test-Cluster` / `New-Cluster` /
  `Set-ClusterQuorum` and coordinate with Windows Failover Clustering; do not
  hand-roll consensus. Declaring a witness and reconciling to it is
  coordination, not reimplementation — the cluster still owns every quorum
  decision. Counting votes ourselves would not be.
- **Reboots are governed by `RebootPolicy`.** The agent surfaces "reboot
  required" and only reboots autonomously when policy permits.
- **Least privilege.** The agent's account should need the minimum AD/host
  rights that let it do its job — see `docs/agent-least-privilege-ad.md` for
  what it actually needs and why, and don't casually add a new privileged
  operation without asking whether it belongs here at all.

## Conventions

- Go: run `gofmt`/`goimports`; standard Go error handling (wrap with
  context, no panics in reconcile paths); table-driven tests; `go vet` clean.
- The schema (`api/types`) is the contract between this agent and whatever
  centre it's paired with. It's a separately versioned module — a field
  added to a shared type must be added to the proto and both converters in
  the same change, with a round-trip test covering it, or it silently
  round-trips as its zero value: the field exists at both ends and is
  carried by neither.
- Commit messages and code comments: plain, direct, no filler.
- Commonwealth English in comments and user-facing strings ("ise" not
  "ize", "behaviour", "honoured", "minimise").
- No boastful or inflated language anywhere in the code or its docs.

## Hyper-V operations

Hyper-V operations shell out to PowerShell modules (`Hyper-V`,
`NetAdapter`, `FailoverClusters`, `Storage`) — see `agent/hyperv`. Every
operation should be documented and easy to target for testing without a
live host. Idempotency applies here too: a script that assumes it's the
only thing that has ever touched a switch or a volume will eventually be
wrong.
