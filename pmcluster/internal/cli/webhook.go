package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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

	svc, closeFn, err := backendWebhooks(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	secretHex, err := svc.Create(cmd.Context(), source, desc)
	if err != nil {
		if errors.Is(err, store.ErrWebhookSourceExists) {
			return fmt.Errorf("webhook source %q already exists", source)
		}
		return fmt.Errorf("create webhook source: %w", err)
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
	svc, closeFn, err := backendWebhooks(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	sources, err := svc.List(cmd.Context())
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
		if s.LastUsedAt != 0 {
			lastUsed = time.Unix(s.LastUsedAt, 0).Format(time.RFC3339)
		}
		desc := "—"
		if s.Description != "" {
			desc = s.Description
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
	svc, closeFn, err := backendWebhooks(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := svc.Delete(cmd.Context(), source); err != nil {
		if errors.Is(err, store.ErrWebhookSourceNotFound) {
			return fmt.Errorf("webhook source %q not found", source)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✅ Webhook source %q removed.\n", source)
	return nil
}
