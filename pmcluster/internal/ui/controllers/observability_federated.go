package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// federatedHostView is the request-scoped projection for one configured host.
// Data has no client or endpoint fields, and each host is collected with its
// own immutable paired clients before the view is rendered.
type federatedHostView struct {
	Name         string
	Hostname     string
	Online       bool
	StatusReason string
	Data         observabilityData
}

type federatedRequest struct {
	tab          string
	rangeValue   string
	service      string
	q            string
	page         int
	search       string
	severity     string
	offset       int
	live         bool
	traceService string
	traceOffset  int
	errorsOnly   bool
	traceID      string
	invalidRange bool
}

// loadFederated is the main Observability entry point when a fleet is
// configured. It parses all request filters once, creates one common UTC
// window, charges the page rate limit once, and then fans out only to the
// immutable host clients selected by the server-side host name.
func (c Observability) loadFederated(g *gin.Context, tab string) observabilityData {
	request, d, window := parseFederatedRequest(g, tab)
	d.Federated = true
	for _, host := range c.Fleet.hosts {
		d.HostOptions = append(d.HostOptions, FleetOption{Name: host.config.Name, Hostname: host.config.Hostname})
	}
	sort.Slice(d.HostOptions, func(i, j int) bool { return d.HostOptions[i].Name < d.HostOptions[j].Name })

	if d.Node != "" && !fleetHasHost(c.Fleet.hosts, d.Node) {
		d.ErrKey = "fleet.host_unknown"
		d.FederatedAll = true
		return d
	}
	if c.ObservabilityRate != nil && !c.ObservabilityRate.Allow() {
		d.ErrKey = "observability.busy"
		d.FederatedAll = d.Node == ""
		return d
	}

	ctx, cancel := context.WithTimeout(g.Request.Context(), fleetRequestWindow)
	defer cancel()
	results := make(chan federatedHostView, len(c.Fleet.hosts))
	selected := 0
	for _, host := range c.Fleet.hosts {
		if d.Node != "" && host.config.Name != d.Node {
			continue
		}
		selected++
		go func(h fleetHost) {
			results <- c.loadFederatedHost(ctx, h, request, window)
		}(host)
	}
	for i := 0; i < selected; i++ {
		d.FederatedHosts = append(d.FederatedHosts, <-results)
	}
	sort.Slice(d.FederatedHosts, func(i, j int) bool { return d.FederatedHosts[i].Name < d.FederatedHosts[j].Name })

	if d.Node != "" && len(d.FederatedHosts) == 1 {
		// Selected-host pages use the existing detailed Observability fragments.
		// The host data was built from the immutable host client, so this copy
		// does not consult or mutate the local Controller API/settings.
		selectedData := d.FederatedHosts[0].Data
		selectedData.Federated = true
		selectedData.FederatedAll = false
		selectedData.HostOptions = d.HostOptions
		selectedData.Node = d.Node
		return selectedData
	}
	if d.Service != "" && !strings.HasPrefix(d.Service, "svc-") {
		for _, host := range d.FederatedHosts {
			if strings.HasPrefix(host.Data.Service, "svc-") {
				d.Service = host.Data.Service
				break
			}
		}
		if !strings.HasPrefix(d.Service, "svc-") {
			// An invalid/raw service filter is useful to the resolver but must
			// never be echoed into the federated HTML model.
			d.Service = ""
		}
	}

	d.FederatedAll = true
	aggregateFederated(&d)
	return d
}

func parseFederatedRequest(g *gin.Context, tab string) (federatedRequest, observabilityData, observability.Window) {
	now := time.Now().UTC()
	d := observabilityData{
		Range:          observabilityDefaultRange,
		Service:        strings.TrimSpace(g.Query("service")),
		Tab:            tab,
		Q:              observability.CleanSearch(g.Query("q")),
		Search:         observability.CleanSearch(g.Query("search")),
		Node:           strings.TrimSpace(g.Query("node")),
		EvidenceTarget: "obs-logs",
		TracesTarget:   "obs-traces",
		PanelID:        "observability-summary",
		IDPrefix:       "obs",
	}
	request := federatedRequest{tab: tab, service: d.Service, q: d.Q, page: max(atoiDefault(g.Query("page"), 1), 1), search: d.Search}
	d.StackPage.Page = request.page
	if sev := strings.TrimSpace(g.Query("severity")); observability.ValidSeverity(sev) {
		d.Severity = sev
	}
	request.severity = d.Severity
	d.Offset = min(max(atoiDefault(g.Query("offset"), 0), 0), observability.MaxLogOffset)
	d.Live = g.Query("live") == "1"
	if d.Live {
		d.Offset = 0
	}
	request.offset, request.live = d.Offset, d.Live
	d.TraceService = observability.CleanSearch(g.Query("svc"))
	d.ErrorsOnly = g.Query("errors") == "1"
	d.TraceOffset = min(max(atoiDefault(g.Query("toffset"), 0), 0), observability.MaxTraceOffset)
	request.traceService, request.traceOffset, request.errorsOnly = d.TraceService, d.TraceOffset, d.ErrorsOnly
	request.traceID = strings.TrimSpace(g.Query("trace"))
	if raw := strings.TrimSpace(g.Query("range")); raw != "" {
		d.Range = raw
	}
	request.rangeValue = d.Range
	window, err := observability.ParseWindow(d.Range, now)
	if err != nil || !fleetRangeAllowed(d.Range) {
		d.Range = observabilityDefaultRange
		request.rangeValue = d.Range
		window, _ = observability.ParseWindow(d.Range, now)
		d.ErrKey = "observability.invalid_range"
		request.invalidRange = true
	}
	return request, d, window
}

func (c Observability) loadFederatedHost(ctx context.Context, host fleetHost, request federatedRequest, window observability.Window) federatedHostView {
	d := observabilityData{
		Range: request.rangeValue, Service: request.service, Q: request.q, Search: request.search,
		Severity: request.severity, Offset: request.offset, Live: request.live,
		TraceService: request.traceService, TraceOffset: request.traceOffset,
		ErrorsOnly: request.errorsOnly, Tab: request.tab, Federated: false,
		Node:           host.config.Name,
		PanelID:        "observability-summary-" + safePanelPart(host.config.Name),
		EvidenceTarget: "obs-logs-" + safePanelPart(host.config.Name),
		TracesTarget:   "obs-traces-" + safePanelPart(host.config.Name),
		IDPrefix:       "obs-" + safePanelPart(host.config.Name),
		api:            host.api, redactor: host.redactor,
		CanInvestigate: false, CanConfigure: false, AIEnabled: false,
	}
	if request.invalidRange {
		d.ErrKey = "observability.invalid_range"
	}
	view := federatedHostView{Name: host.config.Name, Hostname: host.config.Hostname, Data: d}

	// Authorization and exact inventory identity are deliberate gates. No
	// Parseable method is called before all three checks have succeeded.
	if _, err := host.api.Me(ctx); err != nil {
		view.StatusReason = "authorization"
		view.Data.ScopeError = "daemon authorization could not be established"
		view.Data.Evidence.Unavailable = true
		view.Data.Evidence.UnavailableWhy = "scope"
		return view
	}
	nodes, err := host.api.Nodes(ctx)
	if err != nil {
		view.StatusReason = "inventory"
		view.Data.ScopeError = "daemon inventory could not be established"
		view.Data.Evidence.Unavailable = true
		view.Data.Evidence.UnavailableWhy = "scope"
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
		view.Data.ScopeError = "configured host identity did not match daemon inventory"
		view.Data.Evidence.Unavailable = true
		view.Data.Evidence.UnavailableWhy = "scope"
		return view
	}
	services, err := host.api.ListServices(ctx, "")
	if err != nil {
		view.StatusReason = "inventory"
		view.Data.ScopeError = "service inventory could not be established"
		view.Data.Evidence.Unavailable = true
		view.Data.Evidence.UnavailableWhy = "inventory"
		return view
	}

	view.Online = true
	d.ScopeKnown = true
	d.Health.Known = true
	d.Health.NodesKnown = true
	d.Health.Nodes = []observabilityNodeRow{{Hostname: matched.Hostname, Role: matched.Role, Status: matched.Status, Availability: matched.Availability, Leader: matched.IsLeader}}
	d.Health.NodesTotal = 1
	if strings.EqualFold(matched.Status, "ready") {
		d.Health.NodesReady = 1
	}
	d.Health.ServicesKnown = true
	d.Options = federatedServiceOptions(services)
	projectServices(services, &d.Health)

	hostController := *c.Controller
	hostController.API = host.api
	hostController.Telemetry = host.telemetry
	hostController.Redactor = host.redactor
	hostController.Investigation = nil
	obs := Observability{Controller: &hostController}
	switch request.tab {
	case obsTabLogs, obsTabInvestigate:
		if request.service != "" {
			backendService, serviceErr := resolveFederatedService(request.service, services)
			if serviceErr != nil {
				// A service filter is meaningful only to evidence. Keep the
				// inventory/host projection available and fail the log query
				// closed, matching the local controller's behavior.
				d.ErrKey = "observability.invalid_service"
				d.Evidence.Unavailable = true
				d.Evidence.UnavailableWhy = "service"
				d.Service = ""
				opaqueFederatedServiceFields(&d, services)
				redactFederatedDisplay(&d, host.redactor)
				view.Online = true
				view.Data = d
				return view
			}
			d.Service = backendService
		}
		obs.loadEvidenceContext(ctx, &d, window, services, nil, host.redactor)
	case obsTabMetrics:
		obs.loadMetrics(ctx, &d, window, true)
	case obsTabTraces:
		obs.loadTracesContext(ctx, &d, window, host.redactor, request.traceID)
	default:
		d.Stacks, d.StackPage = groupStacks(d.Health.Services, d.Q, request.page)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); obs.loadMetrics(ctx, &d, window, false) }()
		go func() { defer wg.Done(); obs.loadDashboard(ctx, &d, window, host.redactor) }()
		wg.Wait()
	}
	opaqueFederatedServiceFields(&d, services)
	redactFederatedDisplay(&d, host.redactor)
	view.Data = d
	return view
}

func aggregateFederated(d *observabilityData) {
	d.FederatedTotal = int64(len(d.FederatedHosts))
	var totalLogs, totalErrors int64
	servicesKnown, servicesUnknown := false, false
	logsKnown, logsUnknown := false, false
	tracesKnown, tracesUnknown := false, false
	for _, host := range d.FederatedHosts {
		if host.Online {
			d.FederatedOnline++
		} else {
			d.FederatedPartial = true
		}
		if host.Data.Health.ServicesKnown {
			servicesKnown = true
			d.FederatedServices += int64(len(host.Data.Health.Services))
		} else {
			servicesUnknown = true
		}
		switch d.Tab {
		case obsTabOverview:
			if host.Data.Dash.Volume.Known {
				logsKnown = true
				totalLogs += host.Data.Dash.Volume.Total
				totalErrors += host.Data.Dash.Volume.Errors
			} else {
				logsUnknown = true
			}
			if host.Data.Dash.TraceKPI.Known {
				tracesKnown = true
				d.FederatedTraces += host.Data.Dash.TraceKPI.Traces
			} else {
				tracesUnknown = true
			}
		case obsTabLogs:
			if host.Data.Evidence.Known {
				logsKnown = true
				totalLogs += int64(len(host.Data.Evidence.Rows))
			} else {
				logsUnknown = true
			}
		case obsTabMetrics:
			if !host.Data.Metrics.Queried || host.Data.Metrics.Unavailable || !host.Data.Metrics.ServicesOK {
				d.FederatedPartial = true
			}
		case obsTabTraces:
			if host.Data.Traces.Queried && !host.Data.Traces.Unavailable {
				tracesKnown = true
				d.FederatedTraces += int64(len(host.Data.Traces.Rows))
			} else {
				tracesUnknown = true
			}
		}
	}
	d.FederatedServicesKnown = servicesKnown
	d.FederatedServicesPartial = servicesUnknown
	d.FederatedLogsKnown = logsKnown
	d.FederatedLogsPartial = logsUnknown
	d.FederatedLogs, d.FederatedErrors = totalLogs, totalErrors
	d.FederatedErrorRateKnown = logsKnown && totalLogs > 0
	if d.FederatedErrorRateKnown {
		d.FederatedErrorPctText = formatErrorPct(totalErrors, totalLogs)
	}
	d.FederatedTracesKnown = tracesKnown
	d.FederatedTracesPartial = tracesUnknown || d.FederatedOnline != d.FederatedTotal
	d.FederatedPartial = d.FederatedPartial || servicesUnknown || logsUnknown || tracesUnknown
}

func formatErrorPct(errors, total int64) string {
	if total <= 0 {
		return "—"
	}
	return formatPercent(float64(errors) / float64(total) * 100)
}

func formatPercent(value float64) string {
	if value > 0 && value < 0.1 {
		return "<0.1%"
	}
	return fmtFloat(value) + "%"
}

// fmtFloat keeps this file independent from template formatting while making
// the aggregate error ratio stable and bounded.
func fmtFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', 1, 64)
}

func safePanelPart(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		b.WriteString("host")
	}
	// Configured names may normalize to the same CSS-safe slug (for example
	// a.b and a-b). A short digest keeps every host's fragment target unique
	// without putting the raw configured name in an HTML id.
	sum := sha256.Sum256([]byte(value))
	return b.String() + "-" + hex.EncodeToString(sum[:4])
}

func redactFederatedDisplay(d *observabilityData, redactor observability.Redactor) {
	d.Health.HostName = redactor.Text(d.Health.HostName)
	for i := range d.Health.Nodes {
		d.Health.Nodes[i].Hostname = redactor.Text(d.Health.Nodes[i].Hostname)
		d.Health.Nodes[i].Role = redactor.Text(d.Health.Nodes[i].Role)
		d.Health.Nodes[i].Status = redactor.Text(d.Health.Nodes[i].Status)
		d.Health.Nodes[i].Availability = redactor.Text(d.Health.Nodes[i].Availability)
	}
	for i := range d.Health.Services {
		d.Health.Services[i].Stack = redactor.Text(d.Health.Services[i].Stack)
		d.Health.Services[i].Service = redactor.Text(d.Health.Services[i].Service)
		d.Health.Services[i].Path = redactor.Text(d.Health.Services[i].Path)
		d.Health.Services[i].Status = redactor.Text(d.Health.Services[i].Status)
	}
	for i := range d.Stacks {
		d.Stacks[i].Name = redactor.Text(d.Stacks[i].Name)
		for j := range d.Stacks[i].Services {
			d.Stacks[i].Services[j].Stack = redactor.Text(d.Stacks[i].Services[j].Stack)
			d.Stacks[i].Services[j].Service = redactor.Text(d.Stacks[i].Services[j].Service)
			d.Stacks[i].Services[j].Status = redactor.Text(d.Stacks[i].Services[j].Status)
		}
	}
	for i := range d.Options {
		// Value is the inventory-resolved filter sent back to this same host;
		// redact only the visible label so a normal service selection remains
		// functional while all display text crosses the fleet boundary.
		d.Options[i].Label = redactor.Text(d.Options[i].Label)
	}
	redactFleetMetricLabels(redactor, d.NodeCards, d.Metrics.Services)
	for i := range d.Evidence.Rows {
		d.Evidence.Rows[i].Service = redactor.Text(d.Evidence.Rows[i].Service)
		d.Evidence.Rows[i].Severity = redactor.Text(d.Evidence.Rows[i].Severity)
		d.Evidence.Rows[i].Body = redactor.Text(d.Evidence.Rows[i].Body)
		d.Evidence.Rows[i].Preview = redactor.Text(d.Evidence.Rows[i].Preview)
	}
	for i := range d.Traces.Rows {
		d.Traces.Rows[i].Root = redactor.Text(d.Traces.Rows[i].Root)
		d.Traces.Rows[i].Service = redactor.Text(d.Traces.Rows[i].Service)
	}
	for i := range d.Traces.Spans {
		d.Traces.Spans[i].Name = redactor.Text(d.Traces.Spans[i].Name)
		d.Traces.Spans[i].Service = redactor.Text(d.Traces.Spans[i].Service)
		d.Traces.Spans[i].Kind = redactor.Text(d.Traces.Spans[i].Kind)
		d.Traces.Spans[i].Message = redactor.Text(d.Traces.Spans[i].Message)
	}
	for i := range d.Dash.TopErrors {
		d.Dash.TopErrors[i].Message = redactor.Text(d.Dash.TopErrors[i].Message)
		d.Dash.TopErrors[i].Service = redactor.Text(d.Dash.TopErrors[i].Service)
		d.Dash.TopErrors[i].Example = redactor.Text(d.Dash.TopErrors[i].Example)
	}
}

// federatedServiceOptions uses opaque, deterministic selectors. Rendering a
// raw inventory path as an HTML option value would leak a credential if an
// upstream service name happened to contain one; the server resolves the key
// back against the already authorized inventory for the selected host.
func federatedServiceOptions(services []pmapi.Service) []observabilityServiceOption {
	seen := map[string]bool{}
	options := make([]observabilityServiceOption, 0, len(services))
	for _, service := range services {
		path := service.Stack + "/" + unqualifiedServiceName(service.Name, service.Stack)
		if service.Stack == "" || seen[path] {
			continue
		}
		seen[path] = true
		options = append(options, observabilityServiceOption{Value: federatedServiceKey(path), Label: path})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Label < options[j].Label })
	return options
}

func federatedServiceKey(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "svc-" + hex.EncodeToString(sum[:8])
}

func resolveFederatedService(value string, services []pmapi.Service) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	for _, service := range services {
		path := service.Stack + "/" + unqualifiedServiceName(service.Name, service.Stack)
		if value == federatedServiceKey(path) {
			return service.Name, nil
		}
	}
	// Accept the pre-federation path format for bookmarked links, then render
	// the opaque key on the next response.
	return resolveService(value, services)
}

func opaqueFederatedServiceFields(d *observabilityData, services []pmapi.Service) {
	for i := range d.Health.Services {
		d.Health.Services[i].Path = federatedServiceKey(d.Health.Services[i].Path)
	}
	for i := range d.Stacks {
		for j := range d.Stacks[i].Services {
			d.Stacks[i].Services[j].Path = federatedServiceKey(d.Stacks[i].Services[j].Path)
		}
	}
	for i := range d.Options {
		if strings.HasPrefix(d.Options[i].Value, "svc-") {
			continue
		}
		d.Options[i].Value = federatedServiceKey(d.Options[i].Value)
	}
	if d.Service != "" {
		backend, err := resolveFederatedService(d.Service, services)
		if err != nil {
			// Do not echo a raw/unknown inventory selector into the HTML
			// model. The resolver has already run after daemon authorization.
			d.Service = ""
			return
		}
		for _, service := range services {
			if service.Name == backend {
				d.Service = federatedServiceKey(service.Stack + "/" + unqualifiedServiceName(service.Name, service.Stack))
				break
			}
		}
	}
}
