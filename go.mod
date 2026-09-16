module github.com/halvantic/hyperv-control-plane

go 1.26.0

require (
	github.com/vmware/govmomi v0.56.0
	go.etcd.io/bbolt v1.5.0
	golang.org/x/sys v0.48.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12 // indirect
)

require (
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)

require github.com/halvantic/hyperv-control-plane/api v0.0.0

// TEMPORARY: api/ is not yet tagged/pushed (see repo split plan step 9).
// Remove this replace and pin the require above to a real api/vX.Y.Z tag
// the moment api/ is tagged and this repo is pushed. A replace pointing at
// a local path must never survive into the pushed repo -- it works for
// nobody who clones this without the exact same directory next to it.
replace github.com/halvantic/hyperv-control-plane/api => ./api
