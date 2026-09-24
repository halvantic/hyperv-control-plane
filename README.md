<div align="center">

# Ballast Agent 🚢

### The open-source agent that keeps a Hyper-V host honouring its last-known-good configuration — even when the control plane is offline.

[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=for-the-badge&logo=go)](https://go.dev/)
[![License](https://img.shields.io/github/license/halvantic/hyperv-control-plane?style=for-the-badge)](LICENSE)
[![Release](https://img.shields.io/github/v/release/halvantic/hyperv-control-plane?style=for-the-badge)](https://github.com/halvantic/hyperv-control-plane/releases)
[![GitHub Stars](https://img.shields.io/github/stars/halvantic/hyperv-control-plane?style=social)](https://github.com/halvantic/hyperv-control-plane)

<p align="center">
  <a href="#-overview">Overview</a> •
  <a href="#-key-features">Key Features</a> •
  <a href="#-how-it-works">How It Works</a> •
  <a href="#-project-structure">Project Structure</a> •
  <a href="#-building--testing">Building &amp; Testing</a> •
  <a href="#-downloads">Downloads</a> •
  <a href="#-security">Security</a> •
  <a href="#-contributing">Contributing</a>
</p>

![Ballast console host detail showing an mTLS-secured agent settled at generation 4 of 4, with every reconcile step already matching desired state](https://ballast.halvantic.com/img/screenshot-host-reconcile.png)

</div>

---

## ⚡ Overview

**Ballast** is a control plane and distributed agent for Hyper-V hosts and failover clusters — an SMB alternative to vCenter, built on Hyper-V rather than replacing it. This repository is the **agent only**: the Windows service that runs on each Hyper-V host, pulls its desired state from a Ballast centre, and drives the host towards it.

The orchestration centre — the control plane, console, and everything that turns a fleet of these agents into a managed platform — is closed-source and proprietary, and lives outside this repository.

Full product: **https://ballast.halvantic.com/**

This is the part of the product we open the source to, deliberately: it's the piece running with elevated privileges on your hypervisor hosts, and "trust us" isn't good enough for that. Read it, build it, verify it does what we say it does.

---

## ✨ Key Features

- **🔄 Idempotent Reconcile Loop:** Continuously diffs actual host state against desired state and closes the gap — SET-teamed vSwitches, management OS vNICs, Storage Spaces Direct + CSV, cluster membership, and VM lifecycle. Applying the same spec twice is always a no-op.
- **🛟 Offline Autonomy:** Caches the last-honoured desired state in an embedded local store. If the centre goes offline, the agent keeps enforcing exactly what it was last told — it never invents intent, and it never stops.
- **🔐 mTLS to the Centre:** The centre is its own certificate authority and issues each agent a client certificate. A missing or unreadable certificate makes the agent **refuse to start** rather than fall back to an unencrypted connection.
- **📋 Status Journal:** Reports reconcile progress, drift, and `ObservedGeneration` back to the centre, and queues status locally while disconnected.
- **🖥️ VMware Migration Support:** Built-in VMDK streaming and snapshot handling for moving workloads off VMware onto Hyper-V.
- **⚙️ PowerShell-Module Adapter:** Drives Hyper-V, FailoverClusters, Storage, and NetAdapter through their own PowerShell modules — every operation is one you could run and inspect yourself.
- **🚫 No Reimplemented Quorum:** Coordinates with Windows Failover Clustering (`Test-Cluster` / `New-Cluster` / `Set-ClusterQuorum`) rather than hand-rolling cluster consensus.

---

## 🛠️ How It Works

Ballast borrows the Kubernetes kubelet pattern for Hyper-V:

1. **Desired state lives centrally**, on a centre this repository does not contain. It never imperatively drives a host — it only ever hands over what a host, cluster, or VM *should* look like.
2. **The agent reconciles**, comparing actual state to desired state and emitting idempotent jobs to close the gap, on a continuous loop.
3. **The agent caches**, in an embedded local store (`agent/store`), the last desired state it was actually given.
4. **Losing the centre never changes intent.** The agent keeps enforcing its last-honoured state and reports itself as running autonomously (`Status.Autonomous = true`) rather than degraded.
5. **Reconnecting is automatic.** `Generation` (set by the centre) and `ObservedGeneration` (reported by this agent) tell the centre whether a host is settled or has drifted, so reconciliation on reconnect needs no manual comparison.

This is the load-bearing idea of the whole product — see the [desired-state and autonomy model](https://ballast.halvantic.com/docs/core-concepts/desired-state-contracts) for the full write-up, or [`CONTRIBUTING.md`](CONTRIBUTING.md) for the non-negotiables that protect it in code.

---

## 📁 Project Structure

```
agent/
  service/      Windows-service entrypoint and lifecycle
  store/        embedded last-honoured state + status journal
  reconcile/    reconcilers (networking, storage, cluster membership, VMs)
  hyperv/       PowerShell-module adapter for Hyper-V/FailoverClusters/Storage
  vmware/       VMware-source migration support (VMDK streaming, snapshots)
api/
  types/        the desired-state schema (its own Go module — see api/go.mod)
  proto/        gRPC service and message definitions, generated via buf
version/        the agent's own release version
cmd/nfcprobe/   a standalone diagnostic for NFC export-lease behaviour
```

`api/` is versioned independently of the agent (`github.com/halvantic/hyperv-control-plane/api`) because the closed-source centre depends on the same schema — this is the wire contract between an open agent and a closed centre, and it has to be able to move on its own schedule.

---

## 🚀 Building & Testing

| Requirement | Value |
| :--- | :--- |
| **Go** | 1.26+ |
| **OS to build on** | Any — the code builds cross-platform |
| **OS to run on** | Windows Server, Hyper-V role enabled (Hyper-V/FailoverClusters operations shell out to PowerShell) |

```bash
# Clone the repository
git clone https://github.com/halvantic/hyperv-control-plane.git
cd hyperv-control-plane

# Build the agent
go build ./agent/service

# Run the tests (from the repository root, and separately from api/ —
# it's its own module)
go test ./...
cd api && go test ./...
```

Most of the reconcile logic is unit-tested without needing a live Hyper-V host, but the agent itself only builds and runs meaningfully on Windows.

---

## 📦 Downloads

Prefer a built binary over building from source? The current release, `v1.0.1`, is published two ways, both with `SHA256SUMS` for verification:

- [GitHub Releases](https://github.com/halvantic/hyperv-control-plane/releases) — the agent, attached to this repository's own tags.
- [ballast.halvantic.com/download](https://ballast.halvantic.com/download) — the agent alongside the closed-source centre and Ballast Manager binaries.

What changed in each release is in the [release notes](https://ballast.halvantic.com/docs/reference/release-notes).

---

## 🔒 Security

| Concern | How it's handled |
| :--- | :--- |
| **Agent ↔ centre transport** | Mutual TLS; the centre is its own CA. Missing/unreadable cert material → the agent refuses to start. |
| **Agent account privilege** | Documented least-privilege AD delegation, confirmed on real AD — see `docs/agent-least-privilege-ad.md`. |
| **Binary verification** | `SHA256SUMS` published alongside every release (see [Downloads](#-downloads)). No code-signing yet — a checksum match proves the file wasn't corrupted or swapped in transit, not who built it. |

Full write-up, including certificate lifecycle and what's still a known gap: **https://ballast.halvantic.com/docs/security**

---

## 🤝 Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the conventions this codebase holds to and the mental model behind it. Short version: idempotency on every reconcile operation, and the agent never invents intent — it only enforces the last desired state it was given.

---

## 📄 License

Apache 2.0 — see [`LICENSE`](LICENSE).
