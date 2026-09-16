package hyperv

import (
	"context"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// JumboPathTester is the end-to-end half of MTU: whether a frame that size
// actually crosses the wire, which no amount of reading configuration can say.
//
// Kept off the main Interface deliberately. Every other method there is part of
// making a host match its desired state; this one asserts nothing and changes
// nothing, and the reconcile loop must never call it — a ping storm on every
// pass is not diagnosis, it is load.
type JumboPathTester interface {
	TestJumboPath(ctx context.Context, vnics []types.ManagementVNICSpec, targets []string, mtu int) ([]JumboProbe, error)
}
