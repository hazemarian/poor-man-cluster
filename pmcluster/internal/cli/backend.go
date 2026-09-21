package cli

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/certs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/configs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/remote"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/secrets"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/webhooks"
)

// remoteClient returns an HTTP client for the daemon API when remote mode is
// requested (--api-url flag or PMCLUSTER_API_URL env), or nil to use the
// local store. The token comes from --api-token or PMCLUSTER_API_TOKEN.
func remoteClient(cmd *cobra.Command) *remote.Client {
	if apiURL == "" {
		return nil
	}
	tok := apiToken
	if tok == "" {
		tok = os.Getenv("PMCLUSTER_API_TOKEN")
	}
	return remote.New(apiURL, tok, 30*time.Second)
}

// backendXxx helpers return the service for the active backend plus a cleanup
// func (a no-op in remote mode, where no local store is opened).

func backendConfigs(cmd *cobra.Command) (configs.Service, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewConfigs(rc), func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	return configs.NewLocal(st), func() { _ = st.Close() }, nil
}

func backendSecrets(cmd *cobra.Command) (secrets.Service, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewSecrets(rc), func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	cfg := loadConfig(cmd)
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return secrets.NewLocal(st, cipher), func() { _ = st.Close() }, nil
}

func backendWebhooks(cmd *cobra.Command) (webhooks.Service, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewWebhooks(rc), func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	cfg := loadConfig(cmd)
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return webhooks.NewLocal(st, cipher), func() { _ = st.Close() }, nil
}

func backendAPIKeys(cmd *cobra.Command) (apikeys.Service, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewAPIKeys(rc), func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	return apikeys.NewLocal(st), func() { _ = st.Close() }, nil
}

func backendBackups(cmd *cobra.Command) (backups.Service, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewBackups(rc), func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	return &backups.Local{Store: st, Run: backups.LocalTrigger{Store: st}.Trigger, ArchiveDir: backups.DefaultArchiveDir}, func() { _ = st.Close() }, nil
}

func backendTLS(cmd *cobra.Command) (certs.Service, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewTLS(rc), func() {}, nil
	}
	st, cipher, dc, err := siteCertDeps(cmd, loadConfig(cmd), "")
	if err != nil {
		return nil, nil, err
	}
	return tlsService(cmd, st, cipher, dc, loadConfig(cmd)), func() { _ = st.Close(); _ = dc.Close() }, nil
}

func backendDeploy(cmd *cobra.Command) (stacks.Deployer, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewDeploy(rc), func() {}, nil
	}
	svc, _, closeFn, err := openDeploySvc(cmd)
	if err != nil {
		return nil, nil, err
	}
	return svc, closeFn, nil
}

func backendStacks(cmd *cobra.Command) (stacks.Reader, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewStacks(rc), func() {}, nil
	}
	svc, _, closeFn, err := openDeploySvc(cmd)
	if err != nil {
		return nil, nil, err
	}
	return svc, closeFn, nil
}

// apiBaseURL returns the base URL shown in helper text: the remote API URL in
// remote mode, or the local daemon's listen address otherwise.
func apiBaseURL(cmd *cobra.Command) string {
	if remoteClient(cmd) != nil {
		return apiURL
	}
	if cfg := loadConfig(cmd); cfg != nil {
		return "http://" + cfg.ListenAddr
	}
	return ""
}

// loadConfig returns the effective CLI config for commands that need one.
func loadConfig(cmd *cobra.Command) *config.Config {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil
	}
	return cfg
}
