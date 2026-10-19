package sessions

// sqlc runs through go run rather than a tool directive, so its
// dependencies stay out of airut's go.mod and never reach a bot that
// imports airut. v1.31.1 parses SQL through WebAssembly, so it runs with
// CGO_ENABLED=0 like the rest of the build.
//go:generate go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
