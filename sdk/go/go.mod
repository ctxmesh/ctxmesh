// Its OWN module, deliberately. sdk/go inside the root module would make `go get` pull the
// operator's entire Kubernetes dependency tree into an agent that wants an HTTP client.
module github.com/ctxmesh/ctxmesh/sdk/go

go 1.23
