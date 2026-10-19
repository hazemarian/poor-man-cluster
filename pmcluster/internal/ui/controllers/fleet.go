package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability/parseable"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

const (
	fleetConfigLimit   = 64 << 10
	fleetMaxHosts      = 8
	fleetRequestWindow = 12 * time.Second
	fleetLogLimit      = 10
	fleetTotalLogs     = 30
)

// FleetHostConfig is one server in the operator supplied fleet file. The
// endpoint and credential fields are intentionally consumed only while
// constructing the server-side clients; the view models contain no copies.
type FleetHostConfig struct {
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	DaemonURL     string `json:"daemon_url"`
	DaemonToken   string `json:"daemon_token"`
	ParseableURL  string `json:"parseable_url"`
	ParseableAuth string `json:"parseable_auth"`
}

type fleetHost struct {
	config    FleetHostConfig
	api       *pmapi.Client
	telemetry observability.Source
	redactor  observability.Redactor
}

// Fleet renders the authenticated, read-only view that combines independent
// daemon and Parseable endpoints. Hosts are immutable after startup so a
// request cannot replace an endpoint or credential through query parameters.
type Fleet struct {
	*Controller
	hosts []fleetHost
}

// LoadFleetConfig reads and validates the owner-readable fleet file. An empty
// path is an explicit feature-off state. Errors identify only the invalid
// field or structural problem and never include a file value.
func LoadFleetConfig(path string) ([]FleetHostConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("fleet config could not be read")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("fleet config must be a regular file with mode 0600")
	}
	if info.Size() > fleetConfigLimit {
		return nil, fmt.Errorf("fleet config exceeds %d bytes", fleetConfigLimit)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("fleet config could not be read")
	}
	var entries []FleetHostConfig
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, errors.New("fleet config is not a JSON host array")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("fleet config contains trailing data")
	}
	if err := validateFleetConfig(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func validateFleetConfig(entries []FleetHostConfig) error {
	if len(entries) == 0 {
		return errors.New("fleet config must contain at least one host")
	}
	if len(entries) > fleetMaxHosts {
		return fmt.Errorf("fleet config supports at most %d hosts", fleetMaxHosts)
	}
	names := make(map[string]struct{}, len(entries))
	hostnames := make(map[string]struct{}, len(entries))
	for i := range entries {
		e := &entries[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Hostname = strings.TrimSpace(e.Hostname)
		e.DaemonURL = strings.TrimSpace(e.DaemonURL)
		e.DaemonToken = strings.TrimSpace(e.DaemonToken)
		e.ParseableURL = strings.TrimSpace(e.ParseableURL)
		e.ParseableAuth = strings.TrimSpace(e.ParseableAuth)
		if e.Name == "" || hasControl(e.Name) {
			return fmt.Errorf("fleet host %d has an invalid name", i+1)
		}
		if e.Hostname == "" || hasControl(e.Hostname) {
			return fmt.Errorf("fleet host %d has an invalid hostname", i+1)
		}
		if _, ok := names[e.Name]; ok {
			return errors.New("fleet config contains duplicate host names")
		}
		if _, ok := hostnames[e.Hostname]; ok {
			return errors.New("fleet config contains duplicate hostnames")
		}
		names[e.Name] = struct{}{}
		hostnames[e.Hostname] = struct{}{}
		if err := validateFleetEndpoint(e.DaemonURL); err != nil {
			return fmt.Errorf("fleet host %d daemon endpoint is invalid: %w", i+1, err)
		}
		if err := validateFleetEndpoint(e.ParseableURL); err != nil {
			return fmt.Errorf("fleet host %d Parseable endpoint is invalid: %w", i+1, err)
		}
		if e.DaemonToken == "" {
			return fmt.Errorf("fleet host %d daemon token is required", i+1)
		}
		if e.ParseableAuth == "" || !strings.HasPrefix(strings.ToLower(e.ParseableAuth), "basic ") {
			return fmt.Errorf("fleet host %d Parseable auth must use Basic authentication", i+1)
		}
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateFleetEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("endpoint must be an http(s) URL without credentials, query or fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		host := strings.TrimSpace(u.Hostname())
		if host == "localhost" {
			return nil
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("http endpoints must use a loopback address")
		}
		return nil
	default:
		return errors.New("endpoint scheme must be http or https")
	}
}

// NewFleet creates one independent daemon and Parseable client per configured
// host. Redirects are disabled so a bad upstream cannot turn a bearer token or
// Basic credential into a cross-origin follow-up request.
func NewFleet(c *Controller, entries []FleetHostConfig, timeout time.Duration) (*Fleet, error) {
	if c == nil {
		return nil, errors.New("fleet controller is nil")
	}
	normalized := append([]FleetHostConfig(nil), entries...)
	if err := validateFleetConfig(normalized); err != nil {
		return nil, err
	}
	entries = normalized
	if timeout <= 0 || timeout > fleetRequestWindow {
		timeout = fleetRequestWindow
	}
	fleetRedactor := c.Redactor
	for _, entry := range entries {
		// Every host's credential is included in every source redactor. A log
		// record can contain a peer token, and host identity must not become a
		// side channel for credentials from another configured endpoint.
		fleetRedactor = fleetRedactor.WithExact(entry.DaemonToken, entry.ParseableAuth)
	}
	f := &Fleet{Controller: c, hosts: make([]fleetHost, 0, len(entries))}
	for _, entry := range entries {
		daemonHTTP := fleetHTTPClient(timeout)
		api := pmapi.NewWithHTTPClient(entry.DaemonURL, entry.DaemonToken, timeout, daemonHTTP)
		parseHTTP := fleetHTTPClient(timeout)
		source, err := parseable.New(parseable.Config{
			BaseURL: entry.ParseableURL, AuthHeader: entry.ParseableAuth,
			HTTPClient: parseHTTP, Timeout: timeout, MaxConcurrent: 3,
			Redactor: fleetRedactor,
		})
		if err != nil {
			return nil, fmt.Errorf("fleet host %q Parseable client: %w", entry.Name, err)
		}
		f.hosts = append(f.hosts, fleetHost{
			config: entry, api: api, telemetry: source,
			redactor: fleetRedactor,
		})
	}
	return f, nil
}

func fleetHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// FleetData is the complete render model for the combined page. It contains
// only configured host names, typed health values and redacted log fields.
type FleetData struct {
	Range string
	Q     string
	Host  string

	HostOptions []FleetOption
	Hosts       []FleetHostView
	Logs        []FleetLogView

	Online int64
	Total  int64

	Services        int64
	ServicesKnown   bool
	ServicesPartial bool

	LogRecords        int64
	LogRecordsKnown   bool
	LogRecordsPartial bool
	RecentLogsKnown   bool
	RecentLogsPartial bool
	ErrorRecords      int64
	ErrorsKnown       bool
	ErrorRateKnown    bool
	ErrorPctText      string

	TraceCount   int64
	TraceKnown   bool
	TracePartial bool
	Partial      bool
	Busy         bool
	FilterMiss   bool
}

// FleetOption is a safe host filter option. It intentionally has no endpoint
// or credential fields.
type FleetOption struct {
	Name     string
	Hostname string
}

// FleetHostView is one configured host's independent health projection.
type FleetHostView struct {
	Name                 string
	Hostname             string
	Status               string
	Online               bool
	Stale                bool
	TelemetryUnavailable bool
	StatusReason         string

	NodeCards []obsNodeCard
	Services  []obsServiceMetric
	Logs      []FleetLogView

	ServiceCount      int64
	ServiceCountKnown bool
	LogRecords        int64
	LogRecordsKnown   bool
	ErrorRecords      int64
	ErrorRecordsKnown bool
	ErrorRateKnown    bool
	ErrorPctText      string
	TraceCount        int64
	TraceKnown        bool
	RecentLogsKnown   bool
}

// FleetLogView is a server-redacted, bounded recent record with explicit host
// identity so similarly named services on two hosts cannot be confused.
type FleetLogView struct {
	Host      string
	Service   string
	Timestamp string
	TimeShort string
	Severity  string
	SevClass  string
	Body      string
}

// Page renders the combined dashboard after the viewer middleware has
// authenticated the request. Query values select an in-memory host/range and
// never alter an upstream URL or credential.
func (f Fleet) Page(g *gin.Context) {
	d := f.load(g)
	f.Views.Fragment(g, "fleet", d)
}

func (f Fleet) load(g *gin.Context) FleetData {
	d := FleetData{Range: observabilityDefaultRange, Q: observability.CleanSearch(g.Query("q")), Host: strings.TrimSpace(g.Query("host"))}
	if raw := strings.TrimSpace(g.Query("range")); raw != "" {
		d.Range = raw
	}
	window, err := observability.ParseWindow(d.Range, time.Now())
	if err != nil || !fleetRangeAllowed(d.Range) {
		d.Range = observabilityDefaultRange
		window, _ = observability.ParseWindow(d.Range, time.Now())
	}
	for _, host := range f.hosts {
		d.HostOptions = append(d.HostOptions, FleetOption{Name: host.config.Name, Hostname: host.config.Hostname})
	}
	sort.Slice(d.HostOptions, func(i, j int) bool { return d.HostOptions[i].Name < d.HostOptions[j].Name })
	if d.Host != "" && !fleetHasHost(f.hosts, d.Host) {
		d.FilterMiss = true
		return d
	}
	if f.ObservabilityRate != nil && !f.ObservabilityRate.Allow() {
		d.Busy = true
		return d
	}
	ctx, cancel := context.WithTimeout(g.Request.Context(), fleetRequestWindow)
	defer cancel()
	results := make(chan FleetHostView, len(f.hosts))
	selected := 0
	for _, host := range f.hosts {
		if d.Host != "" && host.config.Name != d.Host {
			continue
		}
		selected++
		go func(h fleetHost) {
			results <- f.collectHost(ctx, h, window, d.Q)
		}(host)
	}
	for i := 0; i < selected; i++ {
		d.Hosts = append(d.Hosts, <-results)
	}
	sort.Slice(d.Hosts, func(i, j int) bool { return d.Hosts[i].Name < d.Hosts[j].Name })
	f.aggregate(&d)
	return d
}

func fleetRangeAllowed(raw string) bool {
	switch raw {
	case "15m", "1h", "6h", "24h":
		return true
	default:
		return false
	}
}

func fleetHasHost(hosts []fleetHost, name string) bool {
	for _, host := range hosts {
		if host.config.Name == name {
			return true
		}
	}
	return false
}

func (f Fleet) collectHost(ctx context.Context, host fleetHost, window observability.Window, q string) FleetHostView {
	view := FleetHostView{Name: host.config.Name, Hostname: host.config.Hostname, Status: "offline"}
	if _, err := host.api.Me(ctx); err != nil {
		view.StatusReason = "authorization"
		return view
	}
	nodes, err := host.api.Nodes(ctx)
	if err != nil {
		view.StatusReason = "inventory"
		return view
	}
	var matched *pmapi.Node
	for i := range nodes {
		if nodes[i].Hostname == host.config.Hostname {
			matched = &nodes[i]
			break
		}
	}
	if matched == nil {
		view.StatusReason = "hostname"
		return view
	}
	services, err := host.api.ListServices(ctx, "")
	if err != nil {
		view.StatusReason = "inventory"
		return view
	}
	view.Online = true
	view.Status = "online"

	hostController := *f.Controller
	hostController.API = host.api
	hostController.Telemetry = host.telemetry
	hostController.Redactor = host.redactor
	obs := Observability{Controller: &hostController}
	d := observabilityData{Range: windowLabel(window), Health: observabilityHealth{Known: true, NodesKnown: true, ServicesKnown: true}}
	d.Health.Nodes = []observabilityNodeRow{{Hostname: matched.Hostname, Role: matched.Role, Status: matched.Status, Availability: matched.Availability, Leader: matched.IsLeader}}
	d.Health.NodesTotal = 1
	if strings.EqualFold(matched.Status, "ready") {
		d.Health.NodesReady = 1
	}
	projectServices(services, &d.Health)
	obs.loadMetrics(ctx, &d, window, true)
	obs.loadDashboard(ctx, &d, window, host.redactor)

	if logs, ok := host.telemetry.(observability.LogSource); ok {
		res, logErr := logs.Logs(ctx, observability.LogQuery{Window: window, Search: q, Limit: fleetLogLimit})
		if logErr == nil && res.Status != observability.StatusUnavailable {
			view.RecentLogsKnown = true
			for _, row := range host.redactor.Rows(res.Rows) {
				log := observability.ProjectLog(row)
				if q != "" && !fleetLogMatches(log, q) {
					continue
				}
				view.Logs = append(view.Logs, FleetLogView{
					Host: host.config.Name, Service: log.Service, Timestamp: log.Timestamp,
					TimeShort: shortTime(log.Timestamp), Severity: log.Severity,
					SevClass: severityClass(log.Severity), Body: host.redactor.Text(log.Body),
				})
			}
		}
	}
	view.NodeCards = d.NodeCards
	view.Services = d.Metrics.Services
	redactFleetMetricLabels(host.redactor, view.NodeCards, view.Services)
	if q != "" {
		view.Services = fleetFilterMetrics(view.Services, q)
	}
	view.ServiceCountKnown = d.Health.ServicesKnown
	view.ServiceCount = int64(len(d.Health.Services))
	view.LogRecords = d.Dash.Volume.Total
	view.ErrorRecords = d.Dash.Volume.Errors
	view.LogRecordsKnown = d.Dash.Volume.Known
	view.ErrorRateKnown = d.Dash.Volume.Known
	view.ErrorRecordsKnown = d.Dash.Volume.Known
	if d.Dash.Volume.Known {
		view.ErrorPctText = d.Dash.Volume.ErrPctText
	}
	view.TraceKnown = d.Dash.TraceKPI.Known
	view.TraceCount = d.Dash.TraceKPI.Traces
	view.TelemetryUnavailable = !d.Metrics.Queried || (d.Metrics.Unavailable && !d.Dash.Volume.Known && !d.Dash.TraceKPI.Known && !view.RecentLogsKnown)
	view.Stale = fleetTelemetryStale(d.NodeCards, window)
	return view
}

func windowLabel(w observability.Window) string {
	span := w.End.Sub(w.Start)
	switch span {
	case 15 * time.Minute:
		return "15m"
	case time.Hour:
		return "1h"
	case 6 * time.Hour:
		return "6h"
	default:
		return "24h"
	}
}

func fleetLogMatches(log observability.Log, q string) bool {
	q = strings.ToLower(q)
	return strings.Contains(strings.ToLower(log.Service), q) || strings.Contains(strings.ToLower(log.Body), q)
}

func fleetFilterMetrics(metrics []obsServiceMetric, q string) []obsServiceMetric {
	q = strings.ToLower(q)
	out := make([]obsServiceMetric, 0, len(metrics))
	for _, metric := range metrics {
		if strings.Contains(strings.ToLower(metric.Label), q) {
			out = append(out, metric)
		}
	}
	return out
}

// MetricSource deliberately leaves infrastructure labels untouched because
// those rows contain typed numbers and timestamps. Fleet is the trust boundary
// before those labels reach a shared page, so redact only display strings here
// and preserve every numeric metric and timestamp as-is.
func redactFleetMetricLabels(redactor observability.Redactor, cards []obsNodeCard, services []obsServiceMetric) {
	for i := range cards {
		cards[i].Hostname = redactor.Text(cards[i].Hostname)
		cards[i].Role = redactor.Text(cards[i].Role)
		cards[i].Status = redactor.Text(cards[i].Status)
	}
	for i := range services {
		services[i].Label = redactor.Text(services[i].Label)
	}
}

func fleetTelemetryStale(cards []obsNodeCard, window observability.Window) bool {
	var latest time.Time
	for _, card := range cards {
		for _, chart := range []obsChart{card.CPU, card.Mem, card.Load} {
			if chart.LastAt.After(latest) {
				latest = chart.LastAt
			}
		}
	}
	if latest.IsZero() {
		return false
	}
	staleAfter := 3 * observability.BucketFor(window)
	if staleAfter < 2*time.Minute {
		staleAfter = 2 * time.Minute
	}
	return time.Since(latest) > staleAfter
}

func (f Fleet) aggregate(d *FleetData) {
	d.Total = int64(len(d.Hosts))
	var totalLogs, totalErrors int64
	servicesKnown, servicesUnknown := false, false
	logsKnown, logsUnknown := false, false
	recentLogsKnown, recentLogsUnknown := false, false
	tracesKnown := false
	for _, host := range d.Hosts {
		if host.Online {
			d.Online++
		}
		if host.ServiceCountKnown {
			servicesKnown = true
			d.Services += host.ServiceCount
		} else {
			servicesUnknown = true
		}
		if host.LogRecordsKnown {
			logsKnown = true
			totalLogs += host.LogRecords
			totalErrors += host.ErrorRecords
		} else {
			logsUnknown = true
		}
		if host.RecentLogsKnown {
			recentLogsKnown = true
		} else {
			recentLogsUnknown = true
		}
		if host.TraceKnown {
			tracesKnown = true
			d.TraceCount += host.TraceCount
		} else {
			d.TracePartial = true
		}
		// Keep every host's bounded sample until the global sort. Capping while
		// walking hosts would let the first three hosts monopolize the combined
		// panel when all eight hosts have recent records.
		d.Logs = append(d.Logs, host.Logs...)
	}
	d.ServicesKnown = servicesKnown
	d.ServicesPartial = servicesUnknown
	d.LogRecordsKnown = logsKnown
	d.LogRecordsPartial = logsUnknown
	d.RecentLogsKnown = recentLogsKnown
	d.RecentLogsPartial = recentLogsUnknown
	d.LogRecords, d.ErrorRecords = totalLogs, totalErrors
	d.ErrorsKnown = logsKnown
	d.ErrorRateKnown = logsKnown && totalLogs > 0
	if d.ErrorRateKnown {
		d.ErrorPctText = fmt.Sprintf("%.1f%%", float64(totalErrors)/float64(totalLogs)*100)
	}
	d.TraceKnown = tracesKnown
	if d.Online != d.Total {
		d.TracePartial = true
	}
	d.Partial = d.Online != d.Total || d.ServicesPartial || d.LogRecordsPartial || d.RecentLogsPartial || d.TracePartial
	sort.SliceStable(d.Logs, func(i, j int) bool {
		return d.Logs[i].Timestamp > d.Logs[j].Timestamp
	})
	if len(d.Logs) > fleetTotalLogs {
		d.Logs = d.Logs[:fleetTotalLogs]
	}
}
