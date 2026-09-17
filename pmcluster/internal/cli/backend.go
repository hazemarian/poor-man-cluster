package cli

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service/impl"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service/remote"
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

func backendConfigs(cmd *cobra.Command) (service.ConfigsService, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewConfigs(rc), func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	return impl.NewConfigs(st), func() { _ = st.Close() }, nil
}

func backendSecrets(cmd *cobra.Command) (service.SecretsService, func(), error) {
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
	return impl.NewSecrets(st, cipher), func() { _ = st.Close() }, nil
}

func backendWebhooks(cmd *cobra.Command) (service.WebhooksService, func(), error) {
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
	return impl.NewWebhooks(st, cipher), func() { _ = st.Close() }, nil
}

func backendAPIKeys(cmd *cobra.Command) (service.APIKeysService, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewAPIKeys(rc), func() {}, nil
	}
	st, _, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	return impl.NewAPIKeys(st), func() { _ = st.Close() }, nil
}

func backendTLS(cmd *cobra.Command) (service.TLSService, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewTLS(rc), func() {}, nil
	}
	st, cipher, dc, err := siteCertDeps(cmd, loadConfig(cmd), "")
	if err != nil {
		return nil, nil, err
	}
	return tlsService(cmd, st, cipher, dc, loadConfig(cmd)), func() { _ = st.Close(); _ = dc.Close() }, nil
}

// loadConfig returns the effective CLI config for commands that need one.
func loadConfig(cmd *cobra.Command) *config.Config {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil
	}
	return cfg
}
