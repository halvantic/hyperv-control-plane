# Ballast agent — a least-privilege AD service account

Today the documented posture is "the agent's service account needs to be a
domain admin" (see `-service-user` in `agent/service/main.go`: *"Cluster/domain
operations need a domain admin"*). That is no longer true in practice: the
two AD needs below are both confirmed working on real lab hardware
(2026-09-16) with an account that is neither a Domain Admin nor an Account
Operator. `-service-user`'s comment is now stale and should be updated in the
same change that next touches `agent/service/main.go`.

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
   constrained delegation (live migration). **Confirmed on the lab
   (2026-09-16, Test A): the object-ACL delegation alone is sufficient** — a
   real live migration succeeded with the write granted only via the OU's
   object ACL, no `SeEnableDelegationPrivilege` on the DC. Some Microsoft
   documentation suggests that user right is also required; on this rig it
   was not. Test B (temporarily granting the right) was never run because A
   already passed.
2. The ability to create/delete a Cluster Name Object — see below, this
   changed shape after a real failure, and the fix is confirmed (Test D: a
   cluster was formed, destroyed, and re-formed under a second name with no
   per-name prestage).

## What this account should NOT have

- Not a member of **Domain Admins**.
- Not a member of **Account Operators**.
- Not granted `SeEnableDelegationPrivilege` — confirmed unnecessary (Test A
  passed without it; Test B, which would have granted it temporarily, was
  never needed).

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
dsacls <OU DN> /I:T /G "<account>:CC;computer"
dsacls <OU DN> /I:T /G "<account>:DC;computer"
```

`/I:T` ("this object and subobjects"), not `/I:S` ("subobjects" only) — `/I:S`
is `INHERIT_ONLY` and explicitly excludes the OU itself, so an ACE granted
that way never applies to a computer object created directly in the OU, only
to one created in some OU nested beneath it. The first version of this script
used `/I:S` and it looked correct (the ACE was visibly present on the OU) but
failed live on 2026-09-16 forming a real cluster (`WLGDC`, `Access is denied`
creating the CNO) because the CNO is created directly in the delegated OU,
not in a child OU under it.

This is broader than "Full Control on one object" (it covers *any* computer
object in the OU, not just one chosen name), but still meaningfully narrower
than the OU's Full Control or a blanket "Create/Delete All Child Objects" —
scoped to the `computer` schema class only, so it grants nothing over users,
groups, or the OU's own properties. It means: any cluster name, formed or
re-formed, at any time, with zero advance setup per name. `provision-ballast-agent-account.ps1`
implements this; there is no `-CnoName` parameter to set because there's
nothing left to name in advance.

## The mirror-image `/I:S` case: domain join

`-AllowDomainJoin` delegates two more rights, for completing (or repeating) a
domain join rather than just creating the CNO:

```
dsacls <OU DN> /I:S /G "<account>:WP;;computer"
dsacls <OU DN> /I:S /G "<account>:CA;Reset Password;computer"
```

These use `/I:S`, the opposite of the CC/DC pair above, and getting this
backwards by analogy with them is the exact mistake that shipped first: `CC;
computer` and `DC;computer` put `computer` in the **object type** slot
(`Permission;ObjectType`) — the right (create a child) applies to the OU
itself, so `/I:T` is correct. `WP;;computer` and `CA;Reset
Password;computer` put `computer` in the **inherited object type** slot
instead (`Permission;;InheritedObjectType` — note the empty middle field):
the right applies only to *existing* computer-class descendants, never to
the OU object itself, which is exactly what `/I:S` means. dsacls enforces
this itself — asking for `/I:T` on an inherited object type fails outright
with "computer is specified as Inherited Object Type. `/I:S` must be
present." / "The parameter is incorrect." Confirmed live 2026-09-17
provisioning a real fleet (HVnew01–06).

---

## Empirical verification plan

Run via `scripts/test-ballast-least-privilege.ps1` (`-Tests A,B,C,D,E`, or a
subset) against two or more test nodes free of other in-progress lab work.
For each step: **PASS/FAIL** and, on failure, the **exact error text**.

- **Test A** — classic delegation write, object ACL only, no
  `SeEnableDelegationPrivilege`. Meshed across every ordered pair in `-Nodes`,
  matching what `EnsureMigrationDelegation` actually does. **Confirmed PASS
  on the lab, 2026-09-16** — the object-ACL write was sufficient on its own; a
  real live migration between the tested pair succeeded. (The doc originally
  predicted this would fail; it didn't. Recorded here as a correction, not
  quietly dropped.)
- **Test B** — grants `SeEnableDelegationPrivilege` on the DC, retests one
  representative pair, then **reverts the grant**. **Not run** — moot once A
  passed on its own.
- **Test C** — resource-based constrained delegation instead of classic
  delegation. **Not run** — moot once A passed; nothing here needs RBCD.
- **Test D** — cluster formation under the OU-scoped create/delete
  delegation above: form, destroy, then form again under a **second,
  different name**. **Confirmed PASS on the lab, 2026-09-16** — this is what
  surfaced and then validated the fix in the CNO section above.
- **Test E** — a manual checklist (host reconcile, switch/vNIC, VM, disk
  resize, cluster ops, migration), read from the console rather than
  automated. **Not run.** The two AD-specific unknowns this doc exists to
  answer (A and D) are now closed; E is a broader smoke test across the rest
  of the surface and is still worth doing before calling the posture fully
  proven end to end, but it isn't gating the AD delegation conclusion above.

Results print as a table at the end of the run (`$script:Results`), built
from what actually ran — not something to fill in by hand.

## Open follow-ups

- **Closed**: whether the object ACL alone is sufficient for migration
  delegation (Test A, PASS) and whether OU-scoped create/delete removes the
  per-name CNO prestage requirement (Test D, PASS). Domain Admins /
  `SeEnableDelegationPrivilege` are confirmed unnecessary for either.
- **Still open**: Test E, the manual checklist across the rest of the
  reconcile surface (switch/vNIC, disk resize, general VM ops) under this
  account. Worth running before treating the least-privilege posture as
  proven beyond cluster formation and migration.
- The `Primary`/`WLGDC` failures above are the first real (not lab-predicted)
  data points this posture has produced. Treat future real-world failures the
  same way: fix the actual cause, then fold the fix back into both the script
  and this doc in the same change — not just the script.
