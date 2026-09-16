# Security policy

## Reporting a vulnerability

Email **security@halvantic.com** with a description of the issue, the
affected version or commit, and reproduction steps. Please do not open a
public issue for a suspected vulnerability.

We'll acknowledge receipt as soon as we can and follow up with next steps
once we've assessed it.

## Scope

This repository is the Ballast **agent** only: the Windows service that
runs on a Hyper-V host and reconciles it against desired state pulled from
a centre. It never phones home to anything other than the centre it is
configured to talk to.

The centre (control plane) is closed-source and out of scope for reports
against this repository — email the same address and we'll route it.

## Supported versions

The agent has no formal LTS line yet; please report against the latest
tagged release or `main`.
