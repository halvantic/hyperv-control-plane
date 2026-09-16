# Ballast Agent

Hosts that keep honouring their last-known-good configuration when the control plane is offline.

This repository is the **agent only** — the Windows service that runs on a Hyper-V host, pulls desired state from a Ballast centre, and drives the host towards it. The orchestration centre (the control plane, console, and everything that turns a fleet of these agents into a managed platform) is closed-source and proprietary; it is not in this repository.

Full product: **https://ballast.halvantic.com/**

## What this is

Ballast borrows the Kubernetes kubelet pattern for Hyper-V. A centre holds declarative desired state for hosts, clusters, and VMs; this agent compares actual state to desired and emits idempotent jobs to close the gap. It caches the last desired state it was given in an embedded store, so if the centre goes offline the agent keeps enforcing what it was last told — it never invents intent, and it never stops.

This is the part of the product we open the source to, deliberately: it's the piece running with elevated privileges on your hypervisor hosts, and "trust us" isn't good enough for that. Read it, build it, verify it does what we say it does.

## Layout

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

`api/` is versioned independently of the agent (`github.com/halvantic/hyperv-control-plane/api`) because the centre depends on the same schema — this is the wire contract between an open agent and a closed centre, and it has to be able to move on its own schedule.

## Building

```
go build ./agent/service
```

Requires Go 1.26+. Hyper-V/FailoverClusters operations shell out to PowerShell, so the agent itself only builds and runs meaningfully on Windows, though most of the reconcile logic is unit-tested without needing a live host.

## Testing

```
go test ./...
```

from the repository root, and separately from `api/` (it's its own module).

## Contributing

See `CONTRIBUTING.md` for the conventions this codebase holds to and the mental model behind it. Short version: idempotency on every reconcile operation, and the agent never invents intent — it only enforces the last desired state it was given.

## License

Apache 2.0 — see `LICENSE`.
