package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/remote"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
)

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Whitelisted per-service operations (list, tasks, logs, restart, exec)",
	Long: `Inspect and operate on swarm services without Portainer. Every
operation maps to a fixed Docker SDK call behind the daemon's Bearer
auth — there is no raw docker passthrough.

  pmcluster service list                  — every swarm service (all stacks)
  pmcluster service ps <stack>            — replica health for a stack
  pmcluster service tasks <stack> <svc>   — task (crash/restart) history
  pmcluster service logs <stack> <svc>    — tail stdout/stderr (--tail N)
  pmcluster service restart <stack> <svc> — force a rolling restart
  pmcluster service exec <stack> <svc> -- <argv...>  — non-interactive exec

Exec only reaches tasks running on the same node as the daemon (its
docker socket). For worker-node tasks use ssh + docker exec.`,
}

var serviceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every swarm service (all stacks)",
	RunE:  runServiceList,
}

var servicePsCmd = &cobra.Command{
	Use:   "ps <stack>",
	Short: "Show replica health for one stack",
	Args:  cobra.ExactArgs(1),
	RunE:  runServicePs,
}

var serviceTasksCmd = &cobra.Command{
	Use:   "tasks <stack> <service>",
	Short: "Show a service's task (crash/restart) history",
	Args:  cobra.ExactArgs(2),
	RunE:  runServiceTasks,
}

var serviceLogsCmd = &cobra.Command{
	Use:   "logs <stack> <service>",
	Short: "Tail a service's stdout/stderr",
	Args:  cobra.ExactArgs(2),
	RunE:  runServiceLogs,
}

var serviceRestartCmd = &cobra.Command{
	Use:   "restart <stack> <service>",
	Short: "Force a rolling restart of a service",
	Args:  cobra.ExactArgs(2),
	RunE:  runServiceRestart,
}

var serviceExecCmd = &cobra.Command{
	Use:   "exec <stack> <service> -- <argv...>",
	Short: "Run a non-interactive command in a service's running task",
	Long: `Runs argv (max 16 args, each <= 200 bytes) non-interactively in the
first running task reachable from the daemon node. Use -- to separate
the service args from the command:

  pmcluster service exec demo web -- whoami
  pmcluster service exec demo web -- sh -c "cat /etc/hosts"`,
	Args: cobra.MinimumNArgs(1),
	RunE: runServiceExec,
}

func init() {
	serviceLogsCmd.Flags().Int("tail", 200, "number of lines to tail (1-2000)")

	serviceCmd.AddCommand(
		serviceListCmd, servicePsCmd, serviceTasksCmd,
		serviceLogsCmd, serviceRestartCmd, serviceExecCmd,
	)

	rootCmd.AddCommand(serviceCmd)
}

// backendServices returns the service-ops port for the active backend.
func backendServices(cmd *cobra.Command) (services.Service, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewServices(rc), func() {}, nil
	}
	dc, err := docker.New()
	if err != nil {
		return nil, nil, err
	}
	return services.Local{Docker: dc}, func() { _ = dc.Close() }, nil
}

func runServiceList(cmd *cobra.Command, _ []string) error {
	svc, closeFn, err := backendServices(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	list, err := svc.List(cmd.Context(), "")
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	if len(list) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no swarm services)")
		return nil
	}
	printServices(cmd, list)
	return nil
}

func runServicePs(cmd *cobra.Command, args []string) error {
	stack := args[0]
	svc, closeFn, err := backendServices(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	list, err := svc.List(cmd.Context(), stack)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "(no services in stack %q)\n", stack)
		return nil
	}
	printServices(cmd, list)
	return nil
}

func printServices(cmd *cobra.Command, list []services.ServiceSummary) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTACK\tREPLICAS\tIMAGE\tMODE\tUPDATED")
	for _, s := range list {
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\t%s\n",
			s.Name, s.Stack, s.Replicas, s.Desired,
			s.Image, s.Mode,
			time.Unix(s.Updated, 0).Format(time.RFC3339),
		)
	}
	_ = w.Flush()
}

func runServiceTasks(cmd *cobra.Command, args []string) error {
	stack, service := args[0], args[1]
	svc, closeFn, err := backendServices(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	tasks, err := svc.Tasks(cmd.Context(), stack, service)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tNODE\tSLOT\tSTATE\tERROR\tSTARTED")
	for _, t := range tasks {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
			t.TaskID, t.Node, t.Slot, t.State, t.Error,
			time.Unix(t.StartedAt, 0).Format(time.RFC3339),
		)
	}
	return w.Flush()
}

func runServiceLogs(cmd *cobra.Command, args []string) error {
	stack, service := args[0], args[1]
	tail, _ := cmd.Flags().GetInt("tail")
	svc, closeFn, err := backendServices(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	lines, err := svc.Logs(cmd.Context(), stack, service, tail)
	if err != nil {
		return err
	}
	for _, ln := range lines {
		fmt.Fprintln(cmd.OutOrStdout(), ln.Line)
	}
	return nil
}

func runServiceRestart(cmd *cobra.Command, args []string) error {
	stack, service := args[0], args[1]
	svc, closeFn, err := backendServices(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := svc.Restart(cmd.Context(), stack, service); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "restarted %s_%s\n", stack, service)
	return nil
}

func runServiceExec(cmd *cobra.Command, args []string) error {
	stack, service := args[0], args[1]
	argv := args[2:]
	svc, closeFn, err := backendServices(cmd)
	if err != nil {
		return err
	}
	defer closeFn()

	res, err := svc.Exec(cmd.Context(), stack, service, argv)
	if err != nil {
		return err
	}
	if res.Stdout != "" {
		fmt.Fprint(cmd.OutOrStdout(), res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprint(cmd.ErrOrStderr(), res.Stderr)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\n(exit %d)\n", res.ExitCode)
	return nil
}
