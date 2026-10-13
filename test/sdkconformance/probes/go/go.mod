// The Go SDK's conformance probe. Its own module, so the root module never depends on the SDK and
// the probe always builds against the SDK in this tree.
module github.com/ctxmesh/ctxmesh/test/sdkconformance/probes/go

go 1.23

require github.com/ctxmesh/ctxmesh/sdk/go v0.0.0

replace github.com/ctxmesh/ctxmesh/sdk/go => ../../../../sdk/go
