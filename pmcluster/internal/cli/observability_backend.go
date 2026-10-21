package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability/parseable"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/remote"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ObservabilitySourcePort is the host port the observability stack publishes
// Parseable's HTTP API on. It is published in HOST mode, so it is bound only on
// the node running the Parseable task (the platform node) — one listener, not
// one per node. Every daemon reads that single address over the cluster's
// private network; loopback is what the platform node itself uses. Parseable itself stays on the monitoring overlay
// and is never addressed here by its Swarm DNS name: the daemon is a host
// process and cannot resolve one.
const ObservabilitySourcePort = cluster.ObservabilityPort

// observabilityServiceName is the Swarm service that owns the published port.
// The daemon resolves its running task's node rather than trusting a placement
// setting, so this name is the only link to WHERE the backend actually is.
const observabilityServiceName = "observability_parseable"

// parseableLocator is the slice of the Docker client the read path needs: the
// node list to turn a hostname into an address, and the service/task calls to
// find which node is actually running the backend.
type parseableLocator interface {
	NodeList(context.Context) ([]runtime.Node, error)
	ServiceList(context.Context) ([]runtime.Service, error)
	ServiceTasks(ctx context.Context, serviceID string) ([]runtime.ServiceTask, error)
}

// observabilityAdminCredential is the managed credential Parseable is created
// with. The daemon already holds the encryption key, so no other component
// needs the password — the console reaches telemetry through this daemon API.
const observabilityAdminCredential = cluster.ObservabilityAdminCredential

// backendObservability returns the remote reader when the CLI targets a daemon,
// and a local reader that queries the backend directly otherwise.
func backendObservability(cmd *cobra.Command) (observability.Reader, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewObservability(rc), func() {}, nil
	}
	st, cfg, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	var dc runtime.Client
	if client, dockerErr := docker.New(); dockerErr == nil {
		dc = client
	}
	reader, _ := newLocalObservabilityReader(cmd.Context(), st, cipher, dc)
	return reader, func() {
		if dc != nil {
			_ = dc.Close()
		}
		_ = st.Close()
	}, nil
}

// newLocalObservabilityReader builds the daemon's read service. The returned
// service always carries Swarm inventory, so `status` reports node health even
// when the telemetry backend is unreachable.
//
// A missing credential or an unreachable backend is NOT an error here: the
// caller keeps a reader with inventory only, and every telemetry read answers
// "unavailable" rather than making the daemon fail to start.
func newLocalObservabilityReader(ctx context.Context, st *store.Store, cipher *credentials.Cipher, dc parseableLocator) (*observability.ReadService, error) {
	reader := &observability.ReadService{Inventory: dc}
	if st == nil || cipher == nil {
		return reader, errors.New("observability source requires the cluster store and encryption key")
	}
	cred, err := st.Credential(ctx, observabilityAdminCredential)
	if err != nil {
		return reader, fmt.Errorf("load %s credential: %w", observabilityAdminCredential, err)
	}
	plain, err := cipher.Decrypt(cred.PasswordCiphertext)
	if err != nil {
		return reader, errors.New("decrypt the observability credential")
	}
	// Where to read from: the node running the Parseable task, resolved from the
	// swarm so a placement change cannot point the reader at nothing. A lookup
	// failure is not fatal — it degrades to loopback.
	source, err := localObservabilitySource(observabilityEndpoint(ctx, dc), cred.Username, string(plain))
	if err != nil {
		return reader, err
	}
	reader.LogsSrc = source
	reader.Metrics = source
	reader.TracesSrc = source
	reader.Stats = source
	reader.Names = source
	return reader, nil
}

// observabilityEndpoint is where the host daemon reads Parseable.
//
// The port is published in HOST mode, so exactly one node binds it: the node
// running the Parseable TASK. That is a fact about the swarm, so it is read from
// the task, not from a placement setting — `platform_node` is a hint that can be
// unset, renamed, or ignored by placement, and a reader built on it silently
// dials an address where nothing listens. The setting is only a fallback for a
// cluster where no task is running yet, and loopback is the last resort.
func observabilityEndpoint(ctx context.Context, dc parseableLocator) string {
	if dc != nil {
		if nodes, err := dc.NodeList(ctx); err == nil {
			if name, ok := parseableTaskNode(ctx, dc); ok {
				if addr := addressOf(name, nodes); addr != "" {
					return "http://" + net.JoinHostPort(nodeHost(addr), strconv.Itoa(ObservabilitySourcePort))
				}
			}
		}
	}
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(ObservabilitySourcePort))
}

// parseableTaskNode returns the hostname of the node running the Parseable task.
func parseableTaskNode(ctx context.Context, dc parseableLocator) (string, bool) {
	services, err := dc.ServiceList(ctx)
	if err != nil {
		return "", false
	}
	for _, s := range services {
		if s.Name != observabilityServiceName {
			continue
		}
		tasks, err := dc.ServiceTasks(ctx, s.ID)
		if err != nil {
			return "", false
		}
		for _, t := range tasks {
			if t.State == "running" && t.Node != "" {
				return t.Node, true
			}
		}
	}
	return "", false
}

// addressOf resolves a node hostname to the swarm's address for it.
func addressOf(hostname string, nodes []runtime.Node) string {
	if hostname == "" {
		return ""
	}
	for _, n := range nodes {
		if n.Hostname == hostname && n.Address != "" {
			return n.Address
		}
	}
	return ""
}

// nodeHost strips the port from a swarm node address.
//
// runtime.Node.Address carries ManagerStatus.Addr, which is "10.7.224.12:2377" —
// host AND the manager's raft port — and is EMPTY on a worker. Using it verbatim
// produces "[10.7.224.12:2377]:8000", which url.Parse rejects, so the client
// fails with "endpoint must be an http(s) URL without credentials" and every
// telemetry read answers "unavailable" while the backend is perfectly healthy.
func nodeHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if h, _, err := net.SplitHostPort(addr); err == nil && h != "" {
		return h
	}
	return addr
}

// localObservabilitySource builds the Parseable adapter for the given endpoint.
// The password is used only to construct the adapter's Authorization header and
// the value redactor; it is never returned or logged.
func localObservabilitySource(endpoint, username, password string) (*parseable.Client, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("observability credential has no username")
	}
	if strings.TrimSpace(password) == "" {
		return nil, errors.New("observability credential is empty")
	}
	source, err := parseable.New(parseable.Config{
		BaseURL:  endpoint,
		Username: username,
		Password: password,
		Timeout:  observability.MaxQueryDuration,
		Redactor: observability.NewRedactor(password),
	})
	if err != nil {
		return nil, fmt.Errorf("build observability source: %w", err)
	}
	return source, nil
}
