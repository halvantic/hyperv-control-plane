package hyperv

import (
	"fmt"
	"net"
	"strings"
)

/* WHY a file-share witness could not be applied, in the operator's terms.

   The message this replaces named one remedy for two different faults. It ran
   Test-Path, and on $false said:

     the share \\nfs.lab.example\witness answers on SMB but this node cannot
     read it. Grant the cluster computer account Secondary$ read/write access to
     the share on the file server itself, then retry. '\\nfs.lab.example\witness'
     is not a valid file share path.

   Two faults are behind that one $false — a share that is not there, and a
   share this node may not read — and only the second has anything to do with
   permissions. On a server with no such share the sentence sends the operator
   to edit permissions on something that does not exist, and Set-ClusterQuorum's
   own words, bolted on the end, flatly contradict the sentence they follow:
   Ballast says the node cannot read it, the cmdlet says the path is not valid.
   An operator reading both learns nothing and trusts neither.

   So the script now establishes the Win32 code behind the failure and reports
   it; the sentence is built here, where it is testable, and the cmdlet's own
   report is attributed rather than appended as if it agreed. */

// witnessProblem is the agent's classification of a failed witness apply, as
// reported by witnessScript. Kind is the decided cause; Code is the Win32 error
// behind the share probe (0 when the share read, -1 when nothing classifiable
// came back).
type witnessProblem struct {
	Kind   string `json:"kind"`
	Code   int    `json:"code"`
	Server string `json:"server"`
	Share  string `json:"share"`
	Path   string `json:"path"`
	CNO    string `json:"cno"`
	Node   string `json:"node"`
	Raw    string `json:"raw"`
	// WitnessState is the existing witness resource's state, when a stuck delete
	// made it worth reading. AfterClear says the retry that followed a successful
	// clear is what failed — a different situation from never having got that far.
	WitnessState string `json:"witnessState"`
	AfterClear   bool   `json:"afterClear"`
}

// rawClip bounds the cmdlet's own report so it stays a footnote. A witness
// failure record is short; a runaway one must not bury the remedy above it.
const rawClip = 300

// witnessProblemMessage turns a classification into the sentence an operator
// acts on: what is wrong, and the one step that fixes it.
//
// The file server is outside Ballast's ownership boundary — it runs no agent
// there and cannot change a share's permissions remotely — so every message
// that needs work done on the server says so plainly and names the step,
// rather than failing obscurely and leaving the operator to work it out.
func witnessProblemMessage(p witnessProblem) string {
	server := strings.TrimSpace(p.Server)
	if server == "" {
		server = "the file server"
	}
	path := strings.TrimSpace(p.Path)
	if path == "" {
		path = "the witness share"
	}
	account := "the cluster's computer account"
	if cno := strings.TrimSpace(p.CNO); cno != "" {
		account = "the cluster computer account " + cno + "$"
	}
	node := strings.TrimSpace(p.Node)
	if node == "" {
		node = "this node"
	}

	var msg string
	switch p.Kind {
	case "unreachable":
		msg = "the file server " + server + " is not reachable on SMB (tcp/445) from this node, so the witness cannot be configured. " +
			"Check that the name resolves to the right address and that the server is up and serving SMB."

	case "noshare":
		// The distinction that was missing. Naming permissions here would send
		// someone to change the access rules on a share that is not there.
		named := "a share by that name"
		if s := strings.TrimSpace(p.Share); s != "" {
			named = "a share called " + s
		}
		msg = server + " answers on SMB, but it does not have " + named + " — so there is nothing to make a witness of, " +
			"and no permission change would help. Check the share name, and create the share on " + server + " itself if it is not there " +
			"(Ballast has no agent there and cannot create it). An NFS export is not an SMB share: a file-share witness is SMB only, " +
			"so a directory exported over NFS alone cannot serve as one."

	case "denied":
		msg = "the share " + path + " is there, but this node may not read it: the agent asked as " + node + "'s own computer account and was refused. " +
			"Grant " + account + " read/write access to the share on " + server + " itself (Ballast has no agent there and cannot do it), then retry."

	case "credentials":
		// SMB authenticates the session BEFORE it resolves the share name, so a
		// rejected logon says nothing about whether the share is there. Saying
		// otherwise would repeat the mistake this file exists to fix, one step
		// along: measured against nfs.lab.example, which returns 1326 for a path
		// whose existence is still unestablished.
		msg = server + " answered on SMB but would not accept this node's credentials for " + path + " (Windows error 1326, logon failure). " +
			"SMB authenticates before it resolves the share name, so this says nothing about whether the share is there — the server refused the identity that asked. " +
			"A file-share witness authenticates as " + account + ", so " + server + " has to accept that account: a file server that only admits named local users cannot serve a witness this way. " +
			"Grant that account read/write access on " + server + " itself (Ballast has no agent there and cannot do it), then retry."

	case "deleteblocked":
		/* The OLD witness would not delete, so the new one can never be applied.

		   Reported only when Ballast's own clearing did not run or did not work —
		   a stuck resource it can clear, it clears. So this message is about the
		   case that is left, and it must say which case that is rather than
		   repeating the cmdlet. */
		was := strings.TrimSpace(p.WitnessState)
		if was == "" {
			was = "not online"
		}
		if p.AfterClear {
			msg = "the previous witness resource was " + was + " and would not delete; Ballast cleared it, and applying " + path +
				" still failed for a different reason. The cluster now has NO witness configured — the stuck resource is gone, so this is retryable, " +
				"but until " + path + " applies the cluster is running without one."
			break
		}
		msg = "the previous witness resource is " + was + " and the cluster will not delete it, which blocks every witness change including this one — " +
			"the declaration cannot be applied while it is there. Ballast clears a stuck witness itself when the resource is not online; " +
			"it could not here, so the resource is refusing removal as well as deletion. " +
			"On a node of this cluster, 'Get-ClusterResource | Where-Object ResourceType -eq \"File Share Witness\" | Remove-ClusterResource -Force' removes it, " +
			"after which this reconciles on its own. That it needs a session on a host is a gap in Ballast, not a step you should have to know."

	case "clusterdenied":
		// The cluster reached the share and was refused on it. Distinct from
		// "denied", which is this node's own read failing: the witness is used by
		// the cluster's identity, not the node's, so the cluster's verdict is the
		// one that decides — and it holds whatever the node can or cannot see.
		msg = "the cluster reached " + path + " and was refused access to it: it asked as " + account +
			" and the share would not admit it. Grant that account read/write on the share AND on the directory behind it, on " + server +
			" itself (Ballast has no agent there and cannot do it), then retry. " +
			"On a Samba or NAS file server the cluster account usually has to exist as a user on that server before a share can name it, " +
			"and a server that quietly maps unknown users to guest produces exactly this. " +
			"If another cluster already has a working witness on this server, copy that share's permissions."
		if p.Code > 0 {
			// Said plainly, so nobody goes and investigates the node. The two
			// identities are different and only one of them is in the way.
			msg += fmt.Sprintf(" (This node's own read of the share failed separately with Windows error %d. That is a different identity and is not what is blocking the witness.)", p.Code)
		}

	case "grant":
		// Readable from this node, so the path is right and the share is there.
		// What failed is the cluster adding its own account to the share's
		// permissions, which a NAS appliance will not accept remotely.
		//
		// Windows reports that as error 67, the same number the probe returns for
		// a share that is not there (measured: EnumerateFileSystemEntries against
		// a missing share on a reachable server gives ERROR_BAD_NET_NAME, 67). The
		// two are told apart by WHERE the 67 comes from — the probe reading the
		// share, or the cmdlet's own text while the probe read it fine — which is
		// why the classification cannot be made from the cmdlet's message alone.
		msg = "the share " + path + " exists and is readable from this node, so the path is right — but " + server +
			" refused the cluster's attempt to grant itself access to it. Grant " + account + " read/write permission on that share on " + server +
			" itself (Ballast has no agent there and cannot do it), then retry. " +
			"If another cluster already has a working witness on this server, copy that share's permissions."
		if net.ParseIP(server) != nil {
			msg += " Addressing the file server by name rather than by IP also matters, because the grant authenticates with Kerberos."
		}

	default:
		// Nothing established. Say so rather than choosing a remedy at random:
		// a guess here is what produced the message this file replaces.
		msg = "the witness " + path + " could not be applied, and the cause could not be established from this node."
		if p.Code > 0 {
			msg += fmt.Sprintf(" Reading the share failed with Windows error %d.", p.Code)
		}
		if raw := clipRaw(p.Raw); raw != "" {
			msg += " Set-ClusterQuorum reported: " + raw
		}
		return msg
	}

	// Attributed, not appended. The cmdlet's wording is evidence for whoever
	// reads the condition later; run together with Ballast's own sentence it
	// reads as a second, contradicting diagnosis.
	if raw := clipRaw(p.Raw); raw != "" {
		msg += " (Set-ClusterQuorum's own report was: " + raw + ")"
	}
	return msg
}

// clipRaw reduces a PowerShell error record to one bounded line.
func clipRaw(raw string) string {
	s := strings.TrimSpace(strings.Join(strings.Fields(raw), " "))
	if s == "" {
		return ""
	}
	if len(s) > rawClip {
		s = strings.TrimSpace(s[:rawClip]) + "…"
	}
	return "\"" + s + "\""
}
