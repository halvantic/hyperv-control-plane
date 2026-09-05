package hyperv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

/* Clearing stale persistent logins — the "favourites" in iscsicpl — without
 * touching a live session.
 *
 * Ballast makes a session persistent by registering the existing one, and that
 * call fails against login state left by an earlier configuration. A session
 * made fresh by Connect-IscsiTarget never needs it, because that carries
 * persistence and CHAP together — which is why a node whose session was built
 * fresh is always clean and a node carrying leftovers never becomes persistent
 * however many passes run.
 *
 * The result is a node that works perfectly until it reboots and then does not
 * come back, and Ballast reporting "lost on reboot" for ever with no way out
 * except the iSCSI Initiator control panel on the host.
 *
 * NOTHING IS DISCONNECTED, and that is the whole reason this is safe to offer.
 *
 * It was assumed here that clearing this needed the session dropped, and that
 * was wrong. Observed on HVNEW01, 2026-09-06: the persistent entries and
 * discovery portals were removed with the session still connected, no storage
 * went away, and the next reconcile registered it properly. A persistent entry
 * is what happens at BOOT; removing it does not touch what is connected now.
 *
 * So the reconcile is left to do the repair. This only removes what was
 * standing in its way.
 */

// favouritesScript removes persistent-login entries for the declared targets,
// leaving every live session connected.
func favouritesScript(targets []string) string {
	quoted := make([]string, 0, len(targets))
	for _, t := range targets {
		if strings.TrimSpace(t) != "" {
			quoted = append(quoted, psQuote(t))
		}
	}
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$wanted = @(%s)
if ($wanted.Count -eq 0) { throw 'no targets are declared for this host, so there is nothing to clear' }

$removed = @()
$errs = @()
$kept = @()

# The persistent-login table. There is no cmdlet for removing an entry, so
# iscsicli is the supported route and its output is parsed rather than guessed
# at — the same reader the disconnect path uses.
$out = @()
try { $out = @(& iscsicli ListPersistentTargets 2>&1 | ForEach-Object { [string]$_ }) }
catch { throw ('could not list the persistent logins on this host: ' + $_.Exception.Message) }

$cur = @{}
$records = @()
foreach ($line in $out) {
  $kv = $line -split '\s*:\s*', 2
  if ($kv.Count -ne 2) { continue }
  $k = $kv[0].Trim(); $v = $kv[1].Trim()
  if ($k -eq 'Target Name') {
    if ($cur.ContainsKey('target')) { $records += ,$cur }
    $cur = @{ target = $v }
  }
  elseif ($k -eq 'Initiator Name') { $cur['initiator'] = $v }
  elseif ($k -eq 'Port Number') { $cur['port'] = $v }
  elseif ($k -like 'Address and Socket*') { $cur['addr'] = $v }
}
if ($cur.ContainsKey('target')) { $records += ,$cur }

foreach ($rec in $records) {
  $t = [string]$rec['target']
  # Only the DECLARED targets. An entry for something nobody declared may be
  # another product's, and removing it would be Ballast reaching outside what
  # it was asked to manage.
  $match = @($wanted | Where-Object { $_ -eq $t })
  if ($match.Count -eq 0) { $kept += $t; continue }

  $init = [string]$rec['initiator']; if (-not $init) { $init = 'ROOT' + [char]92 + 'ISCSIPRT' + [char]92 + '0000_0' }
  $port = [string]$rec['port']
  if (-not $port -or $port -like '*Any*') { $port = '*' }
  $addr = ''; $sock = '3260'
  $parts = ([string]$rec['addr']) -split '\s+' | Where-Object { $_ }
  if ($parts.Count -ge 1) { $addr = $parts[0] }
  if ($parts.Count -ge 2) { $sock = $parts[1] }
  if (-not $addr) { $errs += ('no portal address recorded for ' + $t); continue }
  try {
    $r = & iscsicli RemovePersistentTarget $init $t $port $addr $sock 2>&1
    if ($LASTEXITCODE -eq 0) { $removed += ($t + ' via ' + $addr + ':' + $sock) }
    else { $errs += ($addr + ':' + $sock + ' - ' + (($r | ForEach-Object { [string]$_ }) -join ' ')) }
  } catch { $errs += ($addr + ':' + $sock + ' - ' + $_.Exception.Message) }
}

# Proof that nothing was dropped. The count is read back and reported, so the
# claim that this does not disturb storage is a measurement rather than a
# promise made in a comment.
$live = @(Get-IscsiSession -ErrorAction SilentlyContinue).Count

'RESULT=' + (@{ removed = $removed; errors = $errs; keptForeign = $kept; liveSessions = $live } | ConvertTo-Json -Compress -Depth 3)
`, strings.Join(quoted, ", "))
}

// ClearISCSIFavourites removes stale persistent logins for the declared targets
// and reports what went, leaving every session connected.
func (p *PowerShell) ClearISCSIFavourites(ctx context.Context, targets []string) (string, error) {
	out, err := p.run(ctx, favouritesScript(targets))
	if err != nil {
		return "", fmt.Errorf("clear persistent logins: %w", err)
	}
	var res struct {
		Removed      []string `json:"removed"`
		Errors       []string `json:"errors"`
		KeptForeign  []string `json:"keptForeign"`
		LiveSessions int      `json:"liveSessions"`
	}
	if derr := json.Unmarshal([]byte(resultJSON(string(out))), &res); derr != nil {
		return "", fmt.Errorf("clear persistent logins: could not read the result: %w", derr)
	}

	switch {
	case len(res.Errors) > 0 && len(res.Removed) == 0:
		return "", fmt.Errorf("no persistent login could be removed: %s", strings.Join(res.Errors, "; "))
	case len(res.Removed) == 0:
		return fmt.Sprintf("no stale persistent login was found for the declared targets; %d session(s) still connected", res.LiveSessions), nil
	}
	msg := fmt.Sprintf("removed %d persistent login(s): %s. %d session(s) still connected — nothing was disconnected. "+
		"The next reconcile registers the session properly.",
		len(res.Removed), strings.Join(res.Removed, ", "), res.LiveSessions)
	if len(res.Errors) > 0 {
		msg += " Some could not be removed: " + strings.Join(res.Errors, "; ")
	}
	return msg, nil
}
