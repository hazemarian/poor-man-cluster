package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/server"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/spf13/cobra"
)

var observeCmd = &cobra.Command{Use: "observe", Short: "Monitor a standalone Docker host without managing its workloads", RunE: runObserve}

// This entrypoint intentionally never calls runServe: no Swarm initialization,
// reconciliation, volume repair, deployment, snapshot or registry side effects.
func runObserve(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	token := os.Getenv("PMCLUSTER_OBSERVE_TOKEN")
	if token == "" {
		return errors.New("PMCLUSTER_OBSERVE_TOKEN is required")
	}
	if err = os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "data.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err = ensureObserveToken(cmd.Context(), st, token); err != nil {
		return err
	}
	dc, err := docker.New()
	if err != nil {
		return err
	}
	defer dc.Close()
	inventory, err := services.NewStandalone()
	if err != nil {
		return err
	}
	defer inventory.Close()
	// The regular Docker adapter exposes Swarm NodeList, which is unavailable
	// on a standalone host. Keep every other read-only engine operation intact
	// while projecting one truthful local host row from Docker Info.
	observerDocker := standaloneObserverRuntime{Client: dc}
	handler := server.New(server.Deps{Lookup: st, Store: st, Docker: observerDocker, Services: inventory, ReadOnly: true, Investigation: &investigation.Local{Store: st}})
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	fmt.Fprintln(cmd.OutOrStdout(), "pmcluster standalone observer listening on", cfg.ListenAddr)
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// standaloneObserverRuntime preserves the real Docker adapter for cluster
// info and health probes but replaces the Swarm-only node listing with a
// single local-host observation. A successful Docker Info call is the only
// readiness evidence available in this mode; no Swarm leadership or desired
// capacity is invented.
type standaloneObserverRuntime struct{ runtime.Client }

func (s standaloneObserverRuntime) NodeList(ctx context.Context) ([]runtime.Node, error) {
	info, err := s.Client.Info(ctx)
	if err != nil {
		return nil, err
	}
	host := info.Name
	if host == "" {
		host = "standalone"
	}
	return []runtime.Node{{
		ID:            "standalone:" + host,
		Hostname:      host,
		Role:          "standalone",
		Availability:  "unknown",
		Status:        "ready",
		EngineVersion: info.ServerVersion,
	}}, nil
}

var _ runtime.Client = standaloneObserverRuntime{}

func ensureObserveToken(ctx context.Context, st *store.Store, token string) error {
	id, secret := auth.SplitToken(token)
	if len(id) != 8 || len(secret) < 40 {
		return errors.New("observer requires a generated pmc_ token")
	}
	count, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		u, err := st.UserByToken(ctx, token)
		if err != nil {
			return err
		}
		if u == nil || u.Role != auth.RoleAdmin || u.Stack != "" {
			return errors.New("observer token does not match its existing state")
		}
		return nil
	}
	hash, err := auth.HashToken(secret)
	if err != nil {
		return err
	}
	_, err = st.CreateUserWithRole(ctx, "observer-console", id, hash, auth.RoleAdmin)
	return err
}
