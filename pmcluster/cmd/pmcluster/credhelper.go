package main

import (
	"os"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/pullcred"
)

// credHelperName is the symlink docker resolves. Docker runs a credential
// helper as `docker-credential-<suffix> <verb>`, so the binary dispatches on
// its own argv[0]: the same executable is `pmcluster` and, under this name, the
// GHCR credential helper.
const credHelperName = "docker-credential-pmcluster"

// runCredentialHelper runs one helper verb and returns the process exit code.
//
// The contract is Docker's, and it is strict: stdout carries exactly one JSON
// object for `get` and nothing at all otherwise. That is why this path runs
// before cobra and before any logging setup — a usage banner or a log line on
// stdout would break every pull with a parse error that says nothing about the
// cause.
func runCredentialHelper() int {
	return pullcred.Helper{
		Getter: pullcred.Remote{},
		Out:    os.Stdout,
		Err:    os.Stderr,
	}.Serve(os.Args, os.Stdin)
}
