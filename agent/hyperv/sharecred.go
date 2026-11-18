package hyperv

// ShareCredential is a username and password for an SMB share the host's own
// identity cannot reach. Used for the pass it is given to and never stored.
type ShareCredential struct {
	Username string
	Password string
}
