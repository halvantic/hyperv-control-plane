# Ballast agent — a least-privilege AD service account

Today the documented posture is "the agent's service account needs to be a
domain admin" (see `-service-user` in `agent/service/main.go`: *"Cluster/domain
operations need a domain admin"*). Reading every AD-touching code path says
that's more than is actually required.

**This doc is rationale and status only.** The actual, runnable setup and test
tooling lives in two scripts — this doc used to embed their code inline, which
meant every edit had to happen twice and inevitably drifted (the "functions
called before they're defined" bug, the GPO logic, and the CNO change below
were all fixed in the scripts and silently not reflected here). Not doing that
again: from here on, this doc explains *why*, the scripts are the *how*.

- **`scripts/provision-ballast-agent-account.ps1`** — sets up the account,
  local admin (via a GPO linked to the OU, by default), and the AD
  delegations. Full parameter docs are in its own comment-based help
  (`Get-Help .\provision-ballast-agent-account.ps1 -Full`).
- **`scripts/test-ballast-least-privilege.ps1`** — the empirical test plan
  (A–E below) that proves this on real rig hardware. Lab/test use only —
  grants and revokes a sensitive DC right, forms and destroys real clusters,
  runs live migrations.

## Why domain admin isn't the actual requirement

Every direct AD read/write in the agent lives in one file,
`agent/hyperv/powershell_cluster.go`, in one function:

- `EnsureMigrationDelegation` (~line 1060) binds LDAP directly against a DC
  (no credential passed — it authenticates as whatever identity the agent's
  PowerShell child process runs as, i.e. the service account) and writes
  `msDS-AllowedToDelegateTo` on the node's own and its cluster peers' computer
  objects, to enable Kerberos constrained delegation for live migration.
- `FormCluster`/`DestroyCluster` (~lines 1162/1261) call
  `New-Cluster -AdministrativeAccessPoint ActiveDirectoryAndDns`, which
  creates a Cluster Name Object (CNO — a computer account) in AD, and
  `Remove-Cluster`, which deletes it.

Everything else that touches AD is read-only (locating a DC). Domain join
(`agent/hyperv/powershell_identity.go`, `JoinDomain`) uses a **separate,
per-operation credential** delivered by the centre as a secret — never the
service account's own identity — so the service account needs no domain-join
rights at all.

That leaves exactly two AD needs, both narrowly scoped to an OU:

1. Write access to `msDS-AllowedToDelegateTo`, for classic Kerberos
   constrained delegation (live migration) — *if* the object-ACL delegation
   is actually sufficient on its own. Test A/B in the plan below exist
   because that "if" is the open question: writing this attribute is
   documented in places as also requiring the `SeEnableDelegationPrivilege`
   **user right** on the DC (distinct from an object ACL), which by default
   only Domain Admins hold.
2. The ability to create/delete a Cluster Name Object — see below, this
   changed shape after a real failure.

## What this account should NOT have

- Not a member of **Domain Admins**.
- Not a member of **Account Operators**.
- Not granted `SeEnableDelegationPrivilege` as a permanent default (Test B
  grants it temporarily and reverts it).

## CNO creation: OU-scoped create/delete, not a per-name prestaged object

**This changed after a real failure, not just a design preference.**

The first version of this delegation prestaged a single, named, disabled
computer object (e.g. `BallastCluster`) and granted the service account Full
Control on just that one object — the more conservative of Microsoft's two
documented options for delegated cluster administration. It worked in the
empirical test plan, which happened to form a cluster under the same name
that was prestaged.

It failed the first time someone actually used it for real: forming a
cluster named `Primary` (a name that was never prestaged) produced

```
New-Cluster : An error occurred while performing the operation. An error
occurred while creating the cluster 'Primary'. Access is denied
```

`New-Cluster -AdministrativeAccessPoint ActiveDirectoryAndDns` looks for a
disabled computer object matching the **cluster name** first; only if none
exists does it fall back to creating one — which needs Create Computer
Objects rights on the OU, broader than "Full Control on one already-named
object." A prestage-by-name approach means the operator has to know and
declare the cluster's name *before* provisioning the account, and re-running
the provisioning step (or hand-prestaging) every time a cluster is renamed or
a new one is formed under a different name. That's a real, recurring
friction, not a one-off setup cost.

The fix: delegate **create and delete of computer objects, scoped to the OU
and to the `computer` object class only** — Microsoft's other documented
option for this exact scenario — instead of Full Control on one prestaged
object:

```
dsacls <OU DN> /I:S /G "<account>:CC;computer"
dsacls <OU DN> /I:S /G "<account>:DC;computer"
```

This is broader than "Full Control on one object" (it covers *any* computer
object in the OU, not just one chosen name), but still meaningfully narrower
than the OU's Full Control or a blanket "Create/Delete All Child Objects" —
scoped to the `computer` schema class only, so it grants nothing over users,
groups, or the OU's own properties. It means: any cluster name, formed or
re-formed, at any time, with zero advance setup per name. `provision-ballast-agent-account.ps1`
implements this; there is no `-CnoName` parameter to set because there's
nothing left to name in advance.

---

## Empirical verification plan

Run via `scripts/test-ballast-least-privilege.ps1` (`-Tests A,B,C,D,E`, or a
subset) against two or more test nodes free of other in-progress lab work.
For each step: **PASS/FAIL** and, on failure, the **exact error text**.

- **Test A** — classic delegation write, object ACL only, no
  `SeEnableDelegationPrivilege`. Meshed across every ordered pair in `-Nodes`,
  matching what `EnsureMigrationDelegation` actually does. Predicted **FAIL**
  with an AD constraint violation (commonly `DirectoryServicesCOMException`
  `0x2098` or LDAP `INSUFF_ACCESS_RIGHTS`), despite the object ACL granting
  write.
- **Test B** — grants `SeEnableDelegationPrivilege` on the DC, retests one
  representative pair, then **reverts the grant** — never left in place past
  the test that needed it. If it flips A's result to PASS, that confirms the
  DC right (not the ACL) was the blocker.
- **Test C** — the key unknown. Resource-based constrained delegation
  (`msDS-AllowedToActOnBehalfOfOtherIdentity` on the destination) instead of
  classic delegation, with the DC right still not granted, then a **real live
  migration** to prove whether Hyper-V's migration path actually honours
  RBCD — genuinely unverified before running it, not assumed either way.
- **Test D** — cluster formation under the OU-scoped create/delete
  delegation above: form, destroy, then form again under a **second,
  different name**, proving no per-name prestage is needed for either.
- **Test E** — a manual checklist (host reconcile, switch/vNIC, VM, disk
  resize, cluster ops, migration), read from the console rather than
  automated — report anything that fails specifically on a privilege error.

Results print as a table at the end of the run (`$script:Results`), built
from what actually ran — not something to fill in by hand.

## Open follow-ups

- If **A** confirms `SeEnableDelegationPrivilege` is required: document the
  grant/revert pair in Test B as a named, narrow exception — not a reason to
  fall back to Domain Admins.
- If **C** proves RBCD both writable and migration-compatible: raise a
  follow-up proposal for an `agent/hyperv` code change (a second delegation
  mode). Not done as part of this doc.
- The `Primary` failure above is the first real (not lab-predicted) data
  point this posture has produced. Treat future real-world failures the same
  way: fix the actual cause, then fold the fix back into both the script and
  this doc in the same change — not just the script.
