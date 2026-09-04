package types

/*
Where a configuration backup is written.

	The backup itself already exists — the config export and the encrypted key
	escrow — and until now it could only be downloaded through the browser. A
	backup that needs a person to remember to fetch it is one that does not exist
	on the day it is wanted, so it needs somewhere to land.

	THE CENTRE WRITES IT, not an agent. A backup is the centre's own artefact and
	its schedule is the centre's, but the deciding reason is the day you need it:
	you may be restoring precisely BECAUSE the hosts are gone. Routing the write
	through a host would make the disaster-recovery artefact depend on the thing
	the disaster removed.

	A share is outside what Ballast administers — it runs no agent on a NAS and
	should not — so where the remedy lies on the share, the console says so and
	names the one step rather than failing obscurely. Same boundary as
	ISOLibrarySpec, whose shape this follows deliberately.
*/
type BackupTargetSpec struct {
	// Path is the UNC share to write into, e.g. \nas.lab.local\backups.
	//
	// Use the FQDN rather than an IP, for the reason the ISO library gives: an
	// IP forces a fallback away from Kerberos, which often cannot complete.
	Path string `json:"path"`

	/* CredentialSecret optionally names a stored credential to reach the share
	   with. Without one the centre writes as its own service identity, which is
	   the right answer on a domain-joined share and no answer at all on a
	   standalone NAS. */
	CredentialSecret string `json:"credentialSecret,omitempty"`

	// Keep is how many backups to retain at the target. Zero keeps everything,
	// which is a decision an operator can make and not one to make for them by
	// defaulting to a number.
	Keep int `json:"keep,omitempty"`
}

/*
ShareFault is what went wrong reaching a share, as a named cause rather than

	as the message the OS returned.

	Six or seven distinct failures arrive as one or two Windows messages, and they
	have completely different remedies: "Access is denied" covers a rejected
	credential, a share that grants read and not write, and a path the account
	cannot traverse. Handing that string to an operator tells them the write
	failed and nothing about which of the three to go and fix — the defect
	CLAUDE.md names, in the feature whose whole value is being trustworthy a year
	later.
*/
type ShareFault string

const (
	ShareOK ShareFault = ""
	// ShareNameNotResolved — DNS has no answer for the host in the UNC path.
	ShareNameNotResolved ShareFault = "NameNotResolved"
	// ShareUnreachable — the name resolves and nothing answers on 445.
	ShareUnreachable ShareFault = "Unreachable"
	// ShareAuthFailed — the server answered and rejected the credential.
	ShareAuthFailed ShareFault = "AuthFailed"
	/* ShareNotFound — the server or the SHARE NAME in the UNC could not be
	   found. Distinct from SharePathMissing, which is a folder inside a share
	   that does exist. Windows says "The network path was not found" for this,
	   and Go maps it onto fs.ErrNotExist alongside a genuinely missing folder —
	   so the two arrive looking identical and mean different things. Reporting
	   this one as a missing folder tells an operator the share is reachable,
	   which is precisely what has not been established. */
	ShareNotFound ShareFault = "ShareNotFound"
	// SharePathMissing — the server and share are fine; the folder is not there.
	SharePathMissing ShareFault = "PathMissing"
	// ShareWriteDenied — reachable, authenticated, and the account may not write.
	// The one a connect test passes and every backup then fails on.
	ShareWriteDenied ShareFault = "WriteDenied"
	// ShareNoSpace — the write started and the volume is full.
	ShareNoSpace ShareFault = "NoSpace"
	/* ShareUNCUnsupported — a \\server\share path given to a centre that is not
	   Windows.

	   Not a reachability problem and not a permissions one: that path form is a
	   Windows convention, and on Linux the whole string is one filename. The
	   open fails with ENOENT, which classified as PathMissing and sent an
	   operator to check whether a share that exists exists. The target was
	   configured while the centre WAS a Windows binary and kept working right up
	   until the centre moved, which is exactly when nobody re-tests a backup
	   setting. */
	ShareUNCUnsupported ShareFault = "UNCUnsupported"
	// ShareUnknown — none of the above. Reported AS unknown, with the original
	// message, rather than guessed into one of the others.
	ShareUnknown ShareFault = "Unknown"
)

/*
Remedy is what an operator should do about it, in one sentence.

	Written for the person, and honest about which side of the boundary the fix is
	on: Ballast can tell you the account cannot write, and cannot grant it.
*/
func (f ShareFault) Remedy(path string) string {
	switch f {
	case ShareOK:
		return ""
	case ShareNameNotResolved:
		return "The name in " + path + " does not resolve from the centre. Check it is spelled right and that the centre can resolve it — a share named by IP will resolve and then often fail to authenticate, so prefer the FQDN."
	case ShareUnreachable:
		return "The name resolves but nothing answered on port 445. Check the share is running and that a firewall between the centre and it is not blocking SMB."
	case ShareAuthFailed:
		return "The share rejected the credential. If it is domain-joined, the centre's own identity needs access; if it is a standalone NAS, give Ballast a stored credential for it — the centre cannot authenticate as a computer account to a NAS that is not in the domain."
	case ShareNotFound:
		return "Windows could not find " + path + ". That is the server or the share name, not a folder inside it — check the share exists and is spelled right, and that the centre can reach it. It is a different answer from a missing folder, and Ballast cannot tell you the share is reachable because that is what has not been established."
	case SharePathMissing:
		return "The share is reachable but " + path + " does not exist on it. Create the folder, or correct the path."
	case ShareWriteDenied:
		return "The share can be reached and read, and this account may not write to it. Grant it write on the share AND on the folder's permissions — a read-only share passes every connection test and fails every backup. That is on the share itself, which Ballast does not administer."
	case ShareNoSpace:
		return "The share is out of space. Free some, or point the target somewhere with room."
	case ShareUNCUnsupported:
		return "This centre does not run on Windows, and " + path + " is a Windows \\\\server\\share path — so nothing was tried. Mount the share on the appliance and set the target to the mount point instead (for example /mnt/ballast-backup). The share itself is almost certainly fine; it is the path form this centre cannot use."
	}
	return "The share could not be written to and the reason was not one Ballast recognises. The message it returned is above; it comes from the share, not from Ballast."
}
