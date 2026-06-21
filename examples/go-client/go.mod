// A self-contained, dependency-free example client for the agent-gpu
// OpenAI-compatible API. It is a NESTED module (its own go.mod) so it can be
// copied out and `go run` on its own without pulling the server's gRPC /
// Prometheus dependencies, and so the repo-root `go build ./...` / `go test
// ./...` automatically exclude it. Keep this module stdlib-only: it must have
// ZERO `require` directives.
module github.com/jaypetez/agent-gpu/examples/go-client

go 1.25
