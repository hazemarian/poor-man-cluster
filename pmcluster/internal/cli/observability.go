package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

var observabilityCmd = &cobra.Command{
	Use:   "observability",
	Short: "Read bounded cluster telemetry",
}

var observabilityStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Compare Swarm nodes with fresh telemetry",
	Args:  cobra.NoArgs,
	RunE:  runObservabilityStatus,
}

var observabilityLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Read newest telemetry logs",
	Args:  cobra.NoArgs,
	RunE:  runObservabilityLogs,
}

var observabilityMetricsCmd = &cobra.Command{Use: "metrics", Short: "Read bounded metric series"}
var observabilityMetricsNodesCmd = &cobra.Command{
	Use:   "nodes",
	Short: "Read per-node metrics",
	Args:  cobra.NoArgs,
	RunE:  runObservabilityNodeMetrics,
}
var observabilityMetricsServicesCmd = &cobra.Command{
	Use:   "services",
	Short: "Read per-service metrics",
	Args:  cobra.NoArgs,
	RunE:  runObservabilityServiceMetrics,
}

var observabilityTracesCmd = &cobra.Command{
	Use:   "traces",
	Short: "Read trace summaries",
	Args:  cobra.NoArgs,
	RunE:  runObservabilityTraces,
}
var observabilityTraceCmd = &cobra.Command{
	Use:   "trace <trace_id>",
	Short: "Read one trace's spans",
	Args:  cobra.ExactArgs(1),
	RunE:  runObservabilityTrace,
}

func init() {
	observabilityCmd.PersistentFlags().Bool("json", false, "emit one lower_snake_case JSON envelope")
	observabilityStatusCmd.Flags().String("range", "1h", "evidence range (15m, 1h, 6h, 24h)")
	observabilityLogsCmd.Flags().String("range", "1h", "evidence range (15m, 1h, 6h, 24h)")
	observabilityLogsCmd.Flags().String("service", "", "service filter (stack/service or service name)")
	observabilityLogsCmd.Flags().String("severity", "", "severity filter: error, warn, info or debug")
	observabilityLogsCmd.Flags().String("search", "", "bounded text search")
	observabilityLogsCmd.Flags().Int("limit", 50, "number of rows (max 200)")
	observabilityLogsCmd.Flags().Int("offset", 0, "newest-first offset (max 2000)")
	for _, cmd := range []*cobra.Command{observabilityMetricsNodesCmd, observabilityMetricsServicesCmd} {
		cmd.Flags().String("range", "1h", "evidence range (15m, 1h, 6h, 24h)")
		cmd.Flags().String("bucket", "", "bucket duration (default is range-dependent)")
	}
	observabilityTracesCmd.Flags().String("range", "1h", "evidence range (15m, 1h, 6h, 24h)")
	observabilityTracesCmd.Flags().String("service", "", "service filter (stack/service or service name)")
	observabilityTracesCmd.Flags().Bool("errors-only", false, "only traces containing error spans")
	observabilityTracesCmd.Flags().Int("limit", 25, "number of traces (max 50)")
	observabilityTracesCmd.Flags().Int("offset", 0, "newest-first offset (max 1000)")
	observabilityTraceCmd.Flags().String("range", "1h", "evidence range (15m, 1h, 6h, 24h)")

	observabilityMetricsCmd.AddCommand(observabilityMetricsNodesCmd, observabilityMetricsServicesCmd)
	observabilityCmd.AddCommand(observabilityStatusCmd, observabilityLogsCmd, observabilityMetricsCmd, observabilityTracesCmd, observabilityTraceCmd)
	rootCmd.AddCommand(observabilityCmd)
}

func observabilityWindow(cmd *cobra.Command) (observability.Window, error) {
	rangeValue, err := cmd.Flags().GetString("range")
	if err != nil {
		return observability.Window{}, err
	}
	return observability.ParseWindow(rangeValue, time.Now())
}

func observabilityJSON(cmd *cobra.Command) bool {
	value, _ := cmd.Flags().GetBool("json")
	return value
}

func runObservabilityStatus(cmd *cobra.Command, _ []string) error {
	w, err := observabilityWindow(cmd)
	if err != nil {
		return err
	}
	reader, closeFn, err := backendObservability(cmd)
	if err != nil {
		return err
	}
	defer closeFn()
	result, err := reader.Status(cmd.Context(), w)
	if err != nil {
		return err
	}
	if observabilityJSON(cmd) {
		return encodeJSON(cmd, statusJSON(result))
	}
	return printStatus(cmd, result)
}

func runObservabilityLogs(cmd *cobra.Command, _ []string) error {
	w, err := observabilityWindow(cmd)
	if err != nil {
		return err
	}
	serviceValue, err := cmd.Flags().GetString("service")
	if err != nil {
		return err
	}
	service, err := cliServiceFilter(serviceValue)
	if err != nil {
		return err
	}
	severityValue, err := cmd.Flags().GetString("severity")
	if err != nil {
		return err
	}
	severity := strings.ToLower(strings.TrimSpace(severityValue))
	if !observability.ValidSeverity(severity) {
		return errors.New("severity must be error, warn, info, debug or empty")
	}
	search, err := cmd.Flags().GetString("search")
	if err != nil {
		return err
	}
	limit, err := cmd.Flags().GetInt("limit")
	if err != nil {
		return err
	}
	offset, err := cmd.Flags().GetInt("offset")
	if err != nil {
		return err
	}
	q := observability.LogQuery{Service: service, Window: w, Severity: severity, Search: observability.CleanSearch(search), Limit: limit, Offset: offset}
	if err := q.Validate(); err != nil {
		return err
	}
	reader, closeFn, err := backendObservability(cmd)
	if err != nil {
		return err
	}
	defer closeFn()
	result, err := reader.Logs(cmd.Context(), q)
	if err != nil && !errors.Is(err, observability.ErrUnavailable) {
		return err
	}
	result = normalizeUnavailable(result, w, err)
	if observabilityJSON(cmd) {
		return encodeJSON(cmd, evidenceJSON(result))
	}
	return printLogs(cmd, result)
}

func runObservabilityNodeMetrics(cmd *cobra.Command, _ []string) error {
	return runObservabilityMetrics(cmd, false)
}

func runObservabilityServiceMetrics(cmd *cobra.Command, _ []string) error {
	return runObservabilityMetrics(cmd, true)
}

func runObservabilityMetrics(cmd *cobra.Command, services bool) error {
	w, err := observabilityWindow(cmd)
	if err != nil {
		return err
	}
	bucket := observability.BucketFor(w)
	bucketValue, err := cmd.Flags().GetString("bucket")
	if err != nil {
		return err
	}
	if strings.TrimSpace(bucketValue) != "" {
		bucket, err = time.ParseDuration(strings.TrimSpace(bucketValue))
		if err != nil {
			return errors.New("bucket must be a duration")
		}
	}
	q := observability.MetricQuery{Window: w, Bucket: bucket}
	if err := q.Validate(); err != nil {
		return err
	}
	reader, closeFn, err := backendObservability(cmd)
	if err != nil {
		return err
	}
	defer closeFn()
	var result observability.Result
	if services {
		result, err = reader.ServiceMetrics(cmd.Context(), q)
	} else {
		result, err = reader.NodeMetrics(cmd.Context(), q)
	}
	if err != nil && !errors.Is(err, observability.ErrUnavailable) {
		return err
	}
	result = normalizeUnavailable(result, w, err)
	if observabilityJSON(cmd) {
		return encodeJSON(cmd, evidenceJSON(result))
	}
	return printMetrics(cmd, result, services)
}

func runObservabilityTraces(cmd *cobra.Command, _ []string) error {
	w, err := observabilityWindow(cmd)
	if err != nil {
		return err
	}
	serviceValue, err := cmd.Flags().GetString("service")
	if err != nil {
		return err
	}
	service, err := cliServiceFilter(serviceValue)
	if err != nil {
		return err
	}
	errorsOnly, err := cmd.Flags().GetBool("errors-only")
	if err != nil {
		return err
	}
	limit, err := cmd.Flags().GetInt("limit")
	if err != nil {
		return err
	}
	offset, err := cmd.Flags().GetInt("offset")
	if err != nil {
		return err
	}
	q := observability.TraceQuery{Service: service, Window: w, ErrorsOnly: errorsOnly, Limit: limit, Offset: offset}
	if err := q.Validate(); err != nil {
		return err
	}
	reader, closeFn, err := backendObservability(cmd)
	if err != nil {
		return err
	}
	defer closeFn()
	result, err := reader.Traces(cmd.Context(), q)
	if err != nil && !errors.Is(err, observability.ErrUnavailable) {
		return err
	}
	result = normalizeUnavailable(result, w, err)
	if observabilityJSON(cmd) {
		return encodeJSON(cmd, evidenceJSON(result))
	}
	return printTraces(cmd, result)
}

func runObservabilityTrace(cmd *cobra.Command, args []string) error {
	w, err := observabilityWindow(cmd)
	if err != nil {
		return err
	}
	traceID := strings.TrimSpace(args[0])
	if !observability.ValidTraceID(traceID) {
		return errors.New("trace id must be 32 lowercase hex characters")
	}
	reader, closeFn, err := backendObservability(cmd)
	if err != nil {
		return err
	}
	defer closeFn()
	result, err := reader.Trace(cmd.Context(), w, traceID)
	if err != nil && !errors.Is(err, observability.ErrUnavailable) {
		return err
	}
	result = normalizeUnavailable(result, w, err)
	if observabilityJSON(cmd) {
		return encodeJSON(cmd, evidenceJSON(result))
	}
	return printTrace(cmd, result)
}

func cliServiceFilter(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := observability.ParseServicePath(value)
	if err != nil {
		return "", err
	}
	if parsed.Stack != "" {
		return parsed.Stack + "_" + parsed.Service, nil
	}
	return parsed.Service, nil
}

func normalizeUnavailable(result observability.Result, w observability.Window, err error) observability.Result {
	if result.Window.Start.IsZero() {
		result.Window = w
	}
	if err != nil || result.Status == observability.StatusUnavailable {
		result.Status = observability.StatusUnavailable
	}
	return result
}

type cliWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type cliEvidence struct {
	Status            string           `json:"status"`
	ObservedAt        string           `json:"observed_at,omitempty"`
	Window            cliWindow        `json:"window"`
	Rows              []map[string]any `json:"rows"`
	Truncated         bool             `json:"truncated"`
	TruncationReasons []string         `json:"truncation_reasons,omitempty"`
	Coverage          string           `json:"coverage,omitempty"`
}

func evidenceJSON(result observability.Result) cliEvidence {
	status := string(result.Status)
	if status == string(observability.StatusAvailable) && len(result.Rows) == 0 {
		status = string(observability.StatusEmpty)
	}
	rows := make([]map[string]any, 0, len(result.Rows))
	for _, row := range result.Rows {
		rows = append(rows, cleanCLIMap(map[string]any(row)))
	}
	out := cliEvidence{Status: status, Window: cliWindowFor(result.Window), Rows: rows, Truncated: result.Truncated, TruncationReasons: append([]string(nil), result.TruncationReasons...), Coverage: observability.FlattenTerminalText(result.Coverage)}
	if !result.ObservedAt.IsZero() {
		out.ObservedAt = result.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

type cliNode struct {
	ID              string `json:"id"`
	Hostname        string `json:"hostname"`
	Role            string `json:"role"`
	Availability    string `json:"availability"`
	SwarmStatus     string `json:"swarm_status"`
	IsLeader        bool   `json:"is_leader"`
	TelemetryStatus string `json:"telemetry_status"`
	LastSeen        string `json:"last_seen,omitempty"`
}

type cliStatus struct {
	Status             string    `json:"status"`
	InventoryStatus    string    `json:"inventory_status"`
	TelemetryStatus    string    `json:"telemetry_status"`
	ObservedAt         string    `json:"observed_at"`
	Window             cliWindow `json:"window"`
	ExpectedNodes      int       `json:"expected_nodes"`
	ReportingNodes     int       `json:"reporting_nodes"`
	UnknownNodeSamples int       `json:"unknown_node_samples"`
	Truncated          bool      `json:"truncated"`
	TruncationReasons  []string  `json:"truncation_reasons,omitempty"`
	Nodes              []cliNode `json:"nodes"`
}

func statusJSON(result observability.StatusResult) cliStatus {
	rows := make([]cliNode, 0, len(result.Nodes))
	for _, node := range result.Nodes {
		row := cliNode{ID: observability.FlattenTerminalText(node.ID), Hostname: observability.FlattenTerminalText(node.Hostname), Role: observability.FlattenTerminalText(node.Role), Availability: observability.FlattenTerminalText(node.Availability), SwarmStatus: observability.FlattenTerminalText(node.SwarmStatus), IsLeader: node.IsLeader, TelemetryStatus: string(node.TelemetryStatus)}
		if !node.LastSeen.IsZero() {
			row.LastSeen = node.LastSeen.UTC().Format(time.RFC3339Nano)
		}
		rows = append(rows, row)
	}
	out := cliStatus{Status: string(result.Status), InventoryStatus: string(result.InventoryStatus), TelemetryStatus: string(result.TelemetryStatus), Window: cliWindowFor(result.Window), ExpectedNodes: result.ExpectedNodes, ReportingNodes: result.ReportingNodes, UnknownNodeSamples: result.UnknownNodeSamples, Truncated: result.Truncated, TruncationReasons: append([]string(nil), result.TruncationReasons...), Nodes: rows}
	if !result.ObservedAt.IsZero() {
		out.ObservedAt = result.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func cliWindowFor(w observability.Window) cliWindow {
	return cliWindow{Start: w.Start.UTC().Format(time.RFC3339Nano), End: w.End.UTC().Format(time.RFC3339Nano)}
}

func encodeJSON(cmd *cobra.Command, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}

func printStatus(cmd *cobra.Command, result observability.StatusResult) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "STATUS\t%s\nINVENTORY\t%s\nTELEMETRY\t%s\nNODES\t%d/%d reporting\n", result.Status, result.InventoryStatus, result.TelemetryStatus, result.ReportingNodes, result.ExpectedNodes)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if result.Truncated {
		// Telemetry was cut at a read bound: absent or old nodes are unknown,
		// not confirmed missing or stale.
		fmt.Fprintln(w, "NOTE\tpartial telemetry; the read was truncated, so node telemetry is unavailable where not fresh")
		if len(result.TruncationReasons) > 0 {
			fmt.Fprintf(w, "TRUNCATION REASONS\t%s\n", observability.FlattenTerminalText(strings.Join(result.TruncationReasons, ", ")))
		}
	}
	fmt.Fprintln(w, "HOSTNAME\tROLE\tSWARM\tTELEMETRY\tLAST SEEN")
	for _, node := range result.Nodes {
		last := "—"
		if !node.LastSeen.IsZero() {
			last = node.LastSeen.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", observability.FlattenTerminalText(node.Hostname), observability.FlattenTerminalText(node.Role), observability.FlattenTerminalText(node.SwarmStatus), node.TelemetryStatus, last)
	}
	return w.Flush()
}

func printLogs(cmd *cobra.Command, result observability.Result) error {
	if result.Status == observability.StatusUnavailable {
		fmt.Fprintln(cmd.OutOrStdout(), "telemetry unavailable")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	printEvidenceMeta(w, result)
	fmt.Fprintln(w, "TIMESTAMP\tSEVERITY\tSERVICE\tMESSAGE")
	for _, row := range result.Rows {
		log := observability.ProjectLog(row)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", observability.FlattenTerminalText(log.Timestamp), observability.FlattenTerminalText(log.Severity), observability.FlattenTerminalText(log.Service), observability.FlattenTerminalText(log.Body))
	}
	if len(result.Rows) == 0 {
		fmt.Fprintln(w, "(no log records)")
	}
	return w.Flush()
}

func printMetrics(cmd *cobra.Command, result observability.Result, services bool) error {
	if result.Status == observability.StatusUnavailable {
		fmt.Fprintln(cmd.OutOrStdout(), "telemetry unavailable")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	printEvidenceMeta(w, result)
	if services {
		fmt.Fprintln(w, "STACK\tSERVICE\tCONTAINER\tMETRIC\tAVG\tMAX")
		for _, row := range result.Rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", cliRowString(row, "stack"), cliRowString(row, "service"), cliRowString(row, "container"), cliRowString(row, "metric"), cliRowString(row, "avg"), cliRowString(row, "max"))
		}
	} else {
		fmt.Fprintln(w, "BUCKET\tHOST\tMETRIC\tSTATE\tVALUE")
		for _, row := range result.Rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", cliRowString(row, "bucket"), cliRowString(row, "host"), cliRowString(row, "metric"), cliRowString(row, "state"), cliRowString(row, "value"))
		}
	}
	if len(result.Rows) == 0 {
		fmt.Fprintln(w, "(no metric records)")
	}
	return w.Flush()
}

func printTraces(cmd *cobra.Command, result observability.Result) error {
	if result.Status == observability.StatusUnavailable {
		fmt.Fprintln(cmd.OutOrStdout(), "telemetry unavailable")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	printEvidenceMeta(w, result)
	fmt.Fprintln(w, "TRACE ID\tSTARTED\tDURATION NS\tSPANS\tERRORS\tROOT\tSERVICE")
	for _, row := range result.Rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", cliRowString(row, "trace_id"), cliRowString(row, "started"), cliRowString(row, "duration_ns"), cliRowString(row, "spans"), cliRowString(row, "errors"), cliRowString(row, "root"), cliRowString(row, "service"))
	}
	if len(result.Rows) == 0 {
		fmt.Fprintln(w, "(no traces)")
	}
	return w.Flush()
}

func printTrace(cmd *cobra.Command, result observability.Result) error {
	if result.Status == observability.StatusUnavailable {
		fmt.Fprintln(cmd.OutOrStdout(), "telemetry unavailable")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	printEvidenceMeta(w, result)
	fmt.Fprintln(w, "SPAN ID\tPARENT ID\tNAME\tSERVICE\tKIND\tSTATUS\tSTART NS\tDURATION NS\tMESSAGE")
	for _, row := range result.Rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", cliRowString(row, "span_id"), cliRowString(row, "parent_id"), cliRowString(row, "name"), cliRowString(row, "service"), cliRowString(row, "kind"), cliRowString(row, "status"), cliRowString(row, "start_ns"), cliRowString(row, "duration_ns"), cliRowString(row, "message"))
	}
	if len(result.Rows) == 0 {
		fmt.Fprintln(w, "(no spans)")
	}
	return w.Flush()
}

func printEvidenceMeta(w *tabwriter.Writer, result observability.Result) {
	fmt.Fprintf(w, "STATUS\t%s\n", result.Status)
	if result.Coverage != "" {
		fmt.Fprintf(w, "COVERAGE\t%s\n", observability.FlattenTerminalText(result.Coverage))
	}
	if result.Truncated {
		fmt.Fprintln(w, "TRUNCATED\ttrue")
		if len(result.TruncationReasons) > 0 {
			fmt.Fprintf(w, "TRUNCATION REASONS\t%s\n", observability.FlattenTerminalText(strings.Join(result.TruncationReasons, ", ")))
		}
	}
}

func cliRowString(row observability.Row, key string) string {
	value := row[key]
	if value == nil {
		return ""
	}
	var text string
	if raw, ok := value.(string); ok {
		text = raw
	} else if data, err := json.Marshal(value); err == nil {
		text = string(data)
	} else {
		text = fmt.Sprint(value)
	}
	return observability.FlattenTerminalText(text)
}

func cleanCLIMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[observability.FlattenTerminalText(key)] = cleanCLIValue(value)
	}
	return out
}

func cleanCLIValue(value any) any {
	switch v := value.(type) {
	case string:
		return observability.CleanTerminalText(v)
	case map[string]any:
		return cleanCLIMap(v)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = cleanCLIValue(v[i])
		}
		return out
	default:
		return value
	}
}
