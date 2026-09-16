package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

var webhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Manage HMAC-verified deploy webhook sources",
	Long: `/webhook/{source} accepts deploy payloads from CI with HMAC-SHA256
signature verification. Each source is a named (integration, secret) pair.
The secret is shown ONCE on creation and stored encrypted.

POST shape:
  Header:  X-Pmcluster-Signature: sha256=<hex-of-hmac>
  Body:    {"app_name": "...", "version": "...", "manifest": "..."}`,
}

var webhookAddCmd = &cobra.Command{
	Use:   "add <source>",
	Short: "Create a new webhook source and print its shared secret once",
	Args:  cobra.ExactArgs(1),
	RunE:  runWebhookAdd,
}

var webhookListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured webhook sources (without revealing secrets)",
	RunE:  runWebhookList,
}

var webhookRemoveCmd = &cobra.Command{
	Use:   "remove <source>",
	Short: "Remove a webhook source (revokes its shared secret immediately)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWebhookRemove,
}

func init() {
	webhookAddCmd.Flags().String("description", "", "human-readable note about who/what uses this source")

	webhookCmd.AddCommand(webhookAddCmd, webhookListCmd, webhookRemoveCmd)
	rootCmd.AddCommand(webhookCmd)
}

func runWebhookAdd(cmd *cobra.Command, args []string) error {
	source := strings.TrimSpace(args[0])
	if source == "" {
		return errors.New("source: required")
	}
	desc, _ := cmd.Flags().GetString("description")

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return fmt.Errorf("generate secret: %w", err)
	}
	secretHex := hex.EncodeToString(secretBytes)

	ciphertext, err := cipher.Encrypt([]byte(secretHex))
	if err != nil {
		return fmt.Errorf("encrypt secret: %w", err)
	}

	if err := st.CreateWebhookSource(cmd.Context(), source, desc, ciphertext); err != nil {
		if errors.Is(err, store.ErrWebhookSourceExists) {
			return fmt.Errorf("webhook source %q already exists", source)
		}
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), `
✅ Webhook source %q created.

🔑 Shared secret (shown once — save it now):

   %s

CI configuration:

   Endpoint :  https://pmcluster.<your-domain>/webhook/%s
   Method   :  POST
   Header   :  X-Pmcluster-Signature: sha256=<hmac-sha256(body, secret)>
   Body     :  {"app_name": "...", "version": "...", "manifest": "<dsl-yaml>"}

Example signing in shell:
   echo -n "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print "sha256=" $2}'
`, source, secretHex, source)
	return nil
}

func runWebhookList(cmd *cobra.Command, _ []string) error {
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	sources, err := st.ListWebhookSources(cmd.Context())
	if err != nil {
		return fmt.Errorf("list webhook sources: %w", err)
	}
	if len(sources) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no webhook sources configured — use `pmcluster webhook add <source>`)")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SOURCE\tCREATED\tLAST USED\tDESCRIPTION")
	for _, s := range sources {
		lastUsed := "—"
		if s.LastUsedAt.Valid {
			lastUsed = time.Unix(s.LastUsedAt.Int64, 0).Format(time.RFC3339)
		}
		desc := "—"
		if s.Description.Valid && s.Description.String != "" {
			desc = s.Description.String
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			s.Source,
			time.Unix(s.CreatedAt, 0).Format(time.RFC3339),
			lastUsed, desc,
		)
	}
	return w.Flush()
}

func runWebhookRemove(cmd *cobra.Command, args []string) error {
	source := args[0]
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.DeleteWebhookSource(cmd.Context(), source); err != nil {
		if errors.Is(err, store.ErrWebhookSourceNotFound) {
			return fmt.Errorf("webhook source %q not found", source)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Webhook source %q removed.\n", source)
	return nil
}
