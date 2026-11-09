package pullcred

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// The docker credential helper protocol, implemented to the letter:
//
//	docker-credential-pmcluster get    <stdin: server URL>  → {"ServerURL","Username","Secret"}
//	docker-credential-pmcluster store  <stdin: JSON>        → no output, exit 0
//	docker-credential-pmcluster erase  <stdin: server URL>   → no output, exit 0
//	docker-credential-pmcluster list                         → {} on stdout
//
// get is the only verb that does anything: the credential is fetched when it is
// needed and never stored. store/erase/list are no-ops on purpose — Docker
// calls them after `docker login` and for `docker logout`, and a helper that
// FAILED there would make an unrelated `docker login` fail. Returning success
// while storing nothing is the honest implementation of "this credential is not
// a file".
//
// Exit codes matter: Docker reads a non-zero status as "no credential", shows
// the helper's stderr and, depending on the verb, continues unauthenticated.
// So a failure must print NOTHING on stdout (a partial JSON object would be
// parsed as a credential) and one line on stderr.
//
// The one absolute rule: stdout carries exactly one JSON object on a successful
// `get`, and nothing at all for store/erase/list. Docker parses stdout
// strictly; a log line there breaks every pull.

// Credential is the helper protocol's success shape. The field names are
// Docker's (capitalised), not ours.
type Credential struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

// Getter is the one capability the helper's `get` needs. Remote implements it
// over the credential listener; tests substitute a fake.
type Getter interface {
	Get(ctx context.Context, server string) (Token, error)
}

// Helper runs the helper protocol. It is a value (not a process) so the same
// code is exercised by the unit tests without exec.
type Helper struct {
	Getter Getter
	// Out/Err default to os.Stdout/os.Stderr in Serve.
	Out io.Writer
	Err io.Writer
}

// Exit codes. 0 is success; Docker treats any non-zero as "helper failed".
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Serve runs one verb. args is the full argv (args[0] is the program name).
func (h Helper) Serve(args []string, stdin io.Reader) int {
	out, errOut := h.Out, h.Err
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}

	if len(args) < 2 {
		fmt.Fprintln(errOut, "usage: docker-credential-pmcluster <store|get|erase|list>")
		return ExitUsage
	}

	switch args[1] {
	case "get":
		server, err := readServer(stdin)
		if err != nil {
			fmt.Fprintf(errOut, "docker-credential-pmcluster get: %v\n", err)
			return ExitError
		}
		if h.Getter == nil {
			fmt.Fprintln(errOut, "docker-credential-pmcluster get: no credential source is configured")
			return ExitError
		}
		tok, err := h.Getter.Get(context.Background(), server)
		if err != nil {
			fmt.Fprintf(errOut, "docker-credential-pmcluster get %s: %v\n", server, err)
			return ExitError
		}
		if tok.Secret == "" {
			fmt.Fprintf(errOut, "docker-credential-pmcluster get %s: the cluster returned an empty token\n", server)
			return ExitError
		}
		user := tok.Username
		if user == "" {
			user = Username
		}
		enc := json.NewEncoder(out)
		if err := enc.Encode(Credential{ServerURL: server, Username: user, Secret: tok.Secret}); err != nil {
			fmt.Fprintf(errOut, "docker-credential-pmcluster get: write credential: %v\n", err)
			return ExitError
		}
		return ExitOK

	case "store", "erase":
		// Consume stdin so Docker's write never sees a closed pipe, then exit 0
		// without a byte on stdout.
		_, _ = io.Copy(io.Discard, io.LimitReader(stdin, MaxBodyBytes))
		return ExitOK

	case "list":
		// Nothing is stored, so the map is empty. Docker renders this as "no
		// credentials configured for this helper", which is true.
		_, _ = io.Copy(io.Discard, io.LimitReader(stdin, MaxBodyBytes))
		if _, err := io.WriteString(out, "{}\n"); err != nil {
			return ExitError
		}
		return ExitOK

	default:
		fmt.Fprintf(errOut, "docker-credential-pmcluster: unknown verb %q (want store, get, erase or list)\n", args[1])
		fmt.Fprintln(errOut, "usage: docker-credential-pmcluster <store|get|erase|list>")
		return ExitUsage
	}
}

// readServer reads the registry URL Docker writes to stdin. Docker includes a
// trailing newline; an empty stdin is a usage error, not a reason to ask the
// cluster for a token for "".
func readServer(stdin io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(stdin, MaxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("read server URL: %w", err)
	}
	server := strings.TrimSpace(string(b))
	if server == "" {
		return "", fmt.Errorf("no registry URL on stdin")
	}
	return server, nil
}
