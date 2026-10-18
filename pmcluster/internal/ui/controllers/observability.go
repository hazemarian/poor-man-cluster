package controllers

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

const (
	observabilityDefaultRange = "1h"
	observabilityRowLimit     = 500
)

// Observability renders the native, read-only health and telemetry page. It
// intentionally uses the shared daemon client and viewer route group; no
// browser credential or arbitrary Parseable query crosses this controller.
type Observability struct{ *Controller }

type observabilityData struct {
	api            *pmapi.Client
	redactor       observability.Redactor
	CanInvestigate bool
	CanConfigure   bool
	AIEnabled      bool
	Model          string

	Range   string
	Service string
	Options []observabilityServiceOption

	Tab       string
	Q         string
	Severity  string
	Search    string
	Offset    int
	Stacks    []obsStack
	StackPage obsPager
	NodeCards []obsNodeCard
	Metrics   obsMetrics

	Live         bool
	Traces       obsTraces
	TraceService string
	TraceOffset  int
	ErrorsOnly   bool

	ScopeKnown bool
	ScopeError string
	Health     observabilityHealth
	Evidence   observabilityEvidence

	ErrKey string
	ErrRaw string
}

type observabilityServiceOption struct {
	Value string
	Label string
}

type observabilityHealth struct {
	Known bool

	NodesKnown bool
	Nodes      []observabilityNodeRow
	NodesReady int64
	NodesTotal int64
	NodesError bool

	ServicesKnown        bool
	ServicesDesiredKnown bool
	Services             []observabilityServiceRow
	ServicesReady        int64
	ServicesTotal        int64
	ReplicaDeficit       int64
	DeficitKnown         bool

	// Pending is deliberately unavailable. The daemon's list endpoint reports
	// desired/running replicas but not task states for every service. Calling a
	// replica deficit "pending" would turn an observed shortfall into an
	// invented scheduler state.
	PendingKnown bool
	Pending      int64

	BackupsKnown        bool
	BackupHasVerified   bool
	BackupAgeDays       int64
	BackupError         bool
	TLSKnown            bool
	TLSPartial          bool
	TLSHasCertificate   bool
	TLSExpired          bool
	TLSDays             int64
	TLSCertificateCount int64

	HostKnown  bool
	HostName   string
	HostCPU    int
	HostMemory int64
	HostError  bool

	ObservedAt int64
}

type observabilityNodeRow struct {
	Hostname     string
	Role         string
	Status       string
	Availability string
	Leader       bool
}

type observabilityServiceRow struct {
	Stack        string
	Service      string
	Path         string
	Replicas     int64
	Desired      int64
	Deficit      int64
	DeficitKnown bool
	DesiredKnown bool
	Standalone   bool
	Ready        bool
	Job          bool
	Completed    bool
	Platform     bool
	Status       string
	Updated      int64
}

type observabilityEvidence struct {
	Queried        bool
	Known          bool
	Empty          bool
	Unavailable    bool
	Truncated      bool
	Rows           []observabilityEvidenceRow
	RowsSeen       int64
	Coverage       string
	ObservedAt     int64
	WindowStart    string
	WindowEnd      string
	UnavailableWhy string
	HasPrev        bool
	HasNext        bool
	PrevOffset     int
	NextOffset     int
	FirstRow       int
	LastRow        int
}

type observabilityEvidenceRow struct {
	Timestamp string
	Severity  string
	SevClass  string
	Service   string
	Body      string
	Preview   string
	TimeShort string
	Multiline bool
}

// Page renders the tabbed observability console. Each tab loads only its own
// telemetry; the daemon authorization probe and inventory resolution run for
// every tab before any Parseable call. A full page load and an HTMX navigation
// use the same fragment.
func (c Observability) Page(g *gin.Context) {
	d := c.load(g, obsTab(g.Query("tab")))
	c.Views.Fragment(g, "observability", d)
}

// Summary re-renders the Overview tab content for independent refreshes.
func (c Observability) Summary(g *gin.Context) {
	d := c.load(g, obsTabOverview)
	c.Views.Fragment(g, "observabilityoverview", d)
}

// Traces renders only the traces panel for in-page filtering and paging, after
// the same authorization probe as every other tab.
func (c Observability) Traces(g *gin.Context) {
	d := c.load(g, obsTabTraces)
	c.Views.Fragment(g, "observabilitytraces", d)
}

// Evidence renders only the logs panel for in-page filtering and paging. It
// resolves the selected service against the full daemon inventory after the
// /api/me probe, so a scoped token or unknown service fails closed before
// Parseable is contacted.
func (c Observability) Evidence(g *gin.Context) {
	d := c.load(g, obsTabLogs)
	c.Views.Fragment(g, "observabilitylogs", d)
}

func (c Observability) load(g *gin.Context, tab string) observabilityData {
	ctx := g.Request.Context()
	d := observabilityData{Range: observabilityDefaultRange, Service: strings.TrimSpace(g.Query("service")), Tab: tab}
	d.Q = observability.CleanSearch(g.Query("q"))
	d.Search = observability.CleanSearch(g.Query("search"))
	if sev := strings.TrimSpace(g.Query("severity")); observability.ValidSeverity(sev) {
		d.Severity = sev
	}
	d.Offset = min(max(atoiDefault(g.Query("offset"), 0), 0), observability.MaxLogOffset)
	d.Live = g.Query("live") == "1"
	if d.Live {
		d.Offset = 0
	}
	d.TraceService = observability.CleanSearch(g.Query("svc"))
	d.ErrorsOnly = g.Query("errors") == "1"
	d.TraceOffset = min(max(atoiDefault(g.Query("toffset"), 0), 0), observability.MaxTraceOffset)
	if raw := strings.TrimSpace(g.Query("range")); raw != "" {
		d.Range = raw
	}
	if c.ObservabilityRate != nil && !c.ObservabilityRate.Allow() {
		d.ErrKey = "observability.busy"
		d.Evidence.Unavailable = true
		return d
	}
	window, err := observability.ParseWindow(d.Range, time.Now())
	if err != nil || (d.Range != "15m" && d.Range != "1h" && d.Range != "6h" && d.Range != "24h") {
		d.Range = observabilityDefaultRange
		window, _ = observability.ParseWindow(d.Range, time.Now())
		d.ErrKey = "observability.invalid_range"
	}
	apiURL, token, configured := c.loadParams(ctx)
	if !configured {
		d.ErrKey = "err.api_not_configured"
		d.Evidence.Unavailable = true
		d.Evidence.UnavailableWhy = "api"
		return d
	}

	api := pmapi.New(apiURL, token, 8*time.Second)
	d.api = api
	user := middleware.CurrentUser(g)
	d.CanConfigure = user != nil && user.Role == "admin"
	d.CanInvestigate = user != nil && (user.Role == "admin" || user.Role == "operator")
	redactor := c.Redactor.WithExact(token)
	d.redactor = redactor
	// /api/me is an authorization probe, not presentation data. The daemon
	// intentionally denies it to stack-scoped tokens, so do not use any
	// cluster-wide inventory or Parseable evidence until it succeeds.
	scopeCtx, scopeCancel := context.WithTimeout(ctx, 4*time.Second)
	_, scopeErr := api.Me(scopeCtx)
	scopeCancel()
	if scopeErr != nil {
		d.ScopeError = "daemon authorization could not be established"
		d.ErrKey = "observability.scope_unavailable"
		d.Evidence.Unavailable = true
		d.Evidence.UnavailableWhy = "scope"
		return d
	}
	d.ScopeKnown = true

	// Health sources are independent, so fetch them under one bounded context.
	// The edge has a 60-second write deadline; five sequential API timeouts can
	// otherwise make a healthy partial page look like a hung request.
	healthCtx, healthCancel := context.WithTimeout(ctx, 8*time.Second)
	defer healthCancel()
	var (
		services    []pmapi.Service
		servicesErr error
		info        *pmapi.ClusterInfo
		infoErr     error
		nodes       []pmapi.Node
		nodesErr    error
		backups     []pmapi.Backup
		backupsErr  error
		site        *pmapi.SiteCert
		siteErr     error
		hosts       []pmapi.HostCert
		hostsErr    error
	)
	var wg sync.WaitGroup
	wg.Add(6)
	go func() { defer wg.Done(); services, servicesErr = api.ListServices(healthCtx, "") }()
	go func() { defer wg.Done(); info, infoErr = api.ClusterInfo(healthCtx) }()
	go func() { defer wg.Done(); nodes, nodesErr = api.Nodes(healthCtx) }()
	go func() { defer wg.Done(); backups, backupsErr = api.ListBackups(healthCtx, 50) }()
	go func() { defer wg.Done(); site, siteErr = api.SiteCert(healthCtx) }()
	go func() { defer wg.Done(); hosts, hostsErr = api.ListHostCerts(healthCtx) }()
	wg.Wait()

	if servicesErr != nil {
		d.ErrKey = "err.services"
		d.ErrRaw = "cluster service inventory unavailable"
		d.Evidence.Unavailable = true
		d.Evidence.UnavailableWhy = "inventory"
	} else {
		d.Health.ServicesKnown = true
		d.Options = serviceOptions(services)
		projectServices(services, &d.Health)
	}

	// These are independent reads. One failed chart must retain the other
	// daemon-derived values, with each known flag staying explicit.
	if infoErr == nil {
		d.Health.HostKnown = true
		d.Health.HostName = info.NodeName
		d.Health.HostCPU = info.CPUs
		d.Health.HostMemory = info.MemoryBytes
	} else {
		d.Health.HostError = true
	}
	if nodesErr == nil {
		d.Health.NodesKnown = true
		d.Health.NodesTotal = int64(len(nodes))
		for _, node := range nodes {
			row := observabilityNodeRow{Hostname: node.Hostname, Role: node.Role, Status: node.Status, Availability: node.Availability, Leader: node.IsLeader}
			d.Health.Nodes = append(d.Health.Nodes, row)
			if strings.EqualFold(node.Status, "ready") {
				d.Health.NodesReady++
			}
		}
	} else {
		d.Health.NodesError = true
	}
	if backupsErr == nil {
		d.Health.BackupsKnown = true
		for _, backup := range backups {
			key, _, _ := backupState(backup)
			if key != "st.verified" {
				continue
			}
			at := backup.FinishedAt
			if at == 0 {
				at = backup.StartedAt
			}
			// Keep the newest verified timestamp without claiming that an old
			// timestamp is current merely because the list is ordered.
			if !d.Health.BackupHasVerified || at > d.Health.ObservedAt {
				d.Health.ObservedAt = at
			}
			d.Health.BackupHasVerified = true
		}
		if d.Health.BackupHasVerified && d.Health.ObservedAt > 0 {
			d.Health.BackupAgeDays = daysAgo(d.Health.ObservedAt)
		}
	} else {
		d.Health.BackupError = true
	}

	// Site and per-host certificate metadata are separate daemon endpoints. A
	// successful empty response is known empty; an individual endpoint failure
	// produces a partial state instead of a false zero.
	var certTimes []time.Time
	if siteErr == nil {
		d.Health.TLSKnown = true
		if site != nil && site.NotAfter != "" {
			if at, parseErr := time.Parse(time.RFC3339, site.NotAfter); parseErr == nil {
				certTimes = append(certTimes, at)
			} else {
				d.Health.TLSPartial = true
			}
		}
	} else {
		d.Health.TLSPartial = true
	}
	if hostsErr == nil {
		d.Health.TLSKnown = true
		for _, host := range hosts {
			if at, parseErr := time.Parse(time.RFC3339, host.NotAfter); parseErr == nil {
				certTimes = append(certTimes, at)
			} else {
				d.Health.TLSPartial = true
			}
		}
	} else {
		d.Health.TLSPartial = true
	}
	if len(certTimes) > 0 {
		d.Health.TLSHasCertificate = true
		d.Health.TLSCertificateCount = int64(len(certTimes))
		soonest := certTimes[0]
		for _, at := range certTimes[1:] {
			if at.Before(soonest) {
				soonest = at
			}
		}
		d.Health.TLSDays = int64(time.Until(soonest) / (24 * time.Hour))
		d.Health.TLSExpired = soonest.Before(time.Now())
	}

	switch tab {
	case obsTabLogs, obsTabInvestigate:
		c.loadEvidence(g, &d, window, services, servicesErr, redactor)
	case obsTabMetrics:
		c.loadMetrics(ctx, &d, window, true)
	case obsTabTraces:
		if c.Telemetry == nil {
			d.Traces.Unavailable = true
		} else {
			c.loadTraces(g, &d, window, redactor)
		}
	default:
		d.Stacks, d.StackPage = groupStacks(d.Health.Services, d.Q, atoiDefault(g.Query("page"), 1))
		c.loadMetrics(ctx, &d, window, false)
	}
	if c.Investigation != nil && c.Investigation.Configured() {
		policyCtx, policyCancel := context.WithTimeout(ctx, 5*time.Second)
		p, err := (pmapi.InvestigationLedger{Client: api}).Policy(policyCtx)
		policyCancel()
		if err == nil {
			d.AIEnabled = p.Enabled && p.Sharing
			d.Model = p.Model
		}
	}
	return d
}

func (c Observability) loadEvidence(g *gin.Context, d *observabilityData, window observability.Window, services []pmapi.Service, servicesErr error, redactor observability.Redactor) {
	d.Evidence.Queried = true
	d.Evidence.WindowStart = window.Start.UTC().Format(time.RFC3339)
	d.Evidence.WindowEnd = window.End.UTC().Format(time.RFC3339)
	if d.ErrKey == "observability.invalid_range" {
		d.Evidence.Unavailable = true
		d.Evidence.UnavailableWhy = "range"
		return
	}
	if servicesErr != nil {
		d.Evidence.Unavailable = true
		d.Evidence.UnavailableWhy = "inventory"
		return
	}
	selected := strings.TrimSpace(d.Service)
	backendService := ""
	if selected != "" {
		var err error
		backendService, err = resolveService(selected, services)
		if servicesErr != nil || err != nil {
			d.ErrKey = "observability.invalid_service"
			d.Evidence.Unavailable = true
			d.Evidence.UnavailableWhy = "service"
			return
		}
	}
	if backendService != "" {
		for _, service := range services {
			if service.Name == backendService {
				d.Service = service.Stack + "/" + unqualifiedServiceName(service.Name, service.Stack)
				break
			}
		}
	}
	if c.Telemetry == nil {
		d.Evidence.Unavailable = true
		d.Evidence.UnavailableWhy = "telemetry"
		return
	}
	var (
		result observability.Result
		err    error
	)
	if logs, ok := c.Telemetry.(observability.LogSource); ok && d.Tab == obsTabLogs {
		result, err = logs.Logs(g.Request.Context(), observability.LogQuery{
			Service: backendService, Window: window, Severity: d.Severity, Search: d.Search,
			Limit: obsLogPageSize, Offset: d.Offset,
		})
	} else {
		result, err = c.Telemetry.Query(g.Request.Context(), observability.Query{Service: backendService, Window: window, Limit: observabilityRowLimit})
	}
	if err != nil || result.Status == observability.StatusUnavailable {
		d.Evidence.Unavailable = true
		d.Evidence.UnavailableWhy = "telemetry"
		return
	}
	d.Evidence.Known = true
	d.Evidence.Empty = len(result.Rows) == 0
	d.Evidence.RowsSeen = int64(len(result.Rows))
	d.Evidence.Truncated = result.Truncated
	d.Evidence.Coverage = result.Coverage
	d.Evidence.ObservedAt = result.ObservedAt.Unix()
	for _, row := range redactor.Rows(result.Rows) {
		d.Evidence.Rows = append(d.Evidence.Rows, evidenceRow(row))
	}
	d.Evidence.HasNext = result.Truncated
	d.Evidence.HasPrev = d.Offset > 0
	d.Evidence.PrevOffset = max(d.Offset-obsLogPageSize, 0)
	d.Evidence.NextOffset = min(d.Offset+obsLogPageSize, observability.MaxLogOffset)
	d.Evidence.FirstRow = d.Offset + 1
	d.Evidence.LastRow = d.Offset + len(d.Evidence.Rows)
	if d.Offset+obsLogPageSize > observability.MaxLogOffset {
		d.Evidence.HasNext = false
	}
}

func serviceOptions(services []pmapi.Service) []observabilityServiceOption {
	seen := map[string]bool{}
	options := make([]observabilityServiceOption, 0, len(services))
	for _, service := range services {
		if service.Stack == "" {
			continue
		}
		value := service.Stack + "/" + unqualifiedServiceName(service.Name, service.Stack)
		if seen[value] {
			continue
		}
		seen[value] = true
		options = append(options, observabilityServiceOption{Value: value, Label: value})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Value < options[j].Value })
	return options
}

func projectServices(services []pmapi.Service, health *observabilityHealth) {
	health.DeficitKnown = true
	health.ServicesDesiredKnown = true
	for _, service := range services {
		desired, running := int64(service.Desired), int64(service.Replicas)
		desiredKnown := !service.Standalone
		ready := !service.RunOnce && desiredKnown && desired > 0 && running >= desired
		row := observabilityServiceRow{
			Stack: service.Stack, Service: unqualifiedServiceName(service.Name, service.Stack),
			Path:     service.Stack + "/" + unqualifiedServiceName(service.Name, service.Stack),
			Replicas: running, Desired: desired, Ready: ready, Job: service.RunOnce,
			Platform: service.Platform, Updated: service.Updated, DesiredKnown: desiredKnown,
			Standalone: service.Standalone, DeficitKnown: desiredKnown && !service.RunOnce,
		}
		if service.RunOnce || !desiredKnown {
			health.DeficitKnown = false
		}
		if !desiredKnown {
			health.ServicesDesiredKnown = false
		}
		if !service.RunOnce && desiredKnown && desired > 0 {
			health.ServicesTotal++
			if desired > running {
				row.Deficit = desired - running
				health.ReplicaDeficit += row.Deficit
			}
			if ready {
				health.ServicesReady++
			}
		}
		if desired == 0 && !service.RunOnce {
			row.Status = "inactive"
		}
		health.Services = append(health.Services, row)
	}
}

func resolveService(value string, services []pmapi.Service) (string, error) {
	path, err := observability.ParseServicePath(value)
	if err != nil {
		return "", err
	}
	var match string
	for _, service := range services {
		unqualified := unqualifiedServiceName(service.Name, service.Stack)
		if path.Stack != "" {
			if service.Stack != path.Stack || unqualified != path.Service {
				continue
			}
		} else if unqualified != path.Service && service.Name != path.Service {
			continue
		}
		if match != "" && match != service.Name {
			return "", errors.New("service filter is ambiguous")
		}
		match = service.Name
	}
	if match == "" {
		return "", errors.New("service is outside the daemon inventory")
	}
	return match, nil
}

func evidenceRow(row observability.Row) observabilityEvidenceRow {
	log := observability.ProjectLog(row)
	return observabilityEvidenceRow{
		Preview: logPreview(log.Body), TimeShort: shortTime(log.Timestamp),
		Timestamp: log.Timestamp, Severity: log.Severity, SevClass: severityClass(log.Severity),
		Service: log.Service, Body: log.Body, Multiline: strings.ContainsAny(strings.TrimRight(log.Body, "\n"), "\n") || len([]rune(log.Body)) > 160,
	}
}

func atoiDefault(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return n
}

// logPreview is the first line of a message, capped for the collapsed row.
func logPreview(body string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimLeft(body, "\r\n"), "\n", 2)[0])
	if r := []rune(line); len(r) > 160 {
		line = string(r[:160]) + "…"
	}
	return line
}

// shortTime returns HH:MM:SS from a Parseable timestamp, or the input when it
// has no time part.
func shortTime(ts string) string {
	if _, after, ok := strings.Cut(ts, "T"); ok && len(after) >= 8 {
		return after[:8]
	}
	return ts
}
