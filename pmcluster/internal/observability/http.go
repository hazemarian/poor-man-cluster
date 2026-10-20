package observability

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// HTTP exposes the bounded read-only observability API. The parent daemon
// router applies Bearer authentication and stack-scope enforcement before
// mounting this handler.
type HTTP struct {
	Svc Reader
	Now func() time.Time
}

func (h *HTTP) Mount(r chi.Router) {
	h.MountAt(r, "/observability")
}

// MountAt registers the same DTO surface below a caller-owned path. The edge
// process uses this for its reserved internal forwarding route; the daemon
// uses Mount for the public /api/observability paths.
func (h *HTTP) MountAt(r chi.Router, prefix string) {
	prefix = strings.TrimRight(prefix, "/")
	r.Get(prefix+"/status", h.status)
	r.Get(prefix+"/logs", h.logs)
	r.Get(prefix+"/metrics/nodes", h.nodeMetrics)
	r.Get(prefix+"/metrics/services", h.serviceMetrics)
	r.Get(prefix+"/traces", h.traces)
	r.Get(prefix+"/traces/{trace_id}", h.trace)
}

func (h *HTTP) now() time.Time {
	if h != nil && h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

type windowDTO struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type evidenceDTO struct {
	Status            string           `json:"status"`
	ObservedAt        string           `json:"observed_at,omitempty"`
	Window            windowDTO        `json:"window"`
	Rows              []map[string]any `json:"rows"`
	Truncated         bool             `json:"truncated"`
	TruncationReasons []string         `json:"truncation_reasons,omitempty"`
	Coverage          string           `json:"coverage,omitempty"`
	Error             string           `json:"error,omitempty"`
}

type nodeDTO struct {
	ID              string `json:"id"`
	Hostname        string `json:"hostname"`
	Role            string `json:"role"`
	Availability    string `json:"availability"`
	SwarmStatus     string `json:"swarm_status"`
	IsLeader        bool   `json:"is_leader"`
	TelemetryStatus string `json:"telemetry_status"`
	LastSeen        string `json:"last_seen,omitempty"`
}

type statusDTO struct {
	Status             string    `json:"status"`
	InventoryStatus    string    `json:"inventory_status"`
	TelemetryStatus    string    `json:"telemetry_status"`
	ObservedAt         string    `json:"observed_at"`
	Window             windowDTO `json:"window"`
	ExpectedNodes      int       `json:"expected_nodes"`
	ReportingNodes     int       `json:"reporting_nodes"`
	UnknownNodeSamples int       `json:"unknown_node_samples"`
	Truncated          bool      `json:"truncated"`
	TruncationReasons  []string  `json:"truncation_reasons,omitempty"`
	Nodes              []nodeDTO `json:"nodes"`
}

func (h *HTTP) status(w http.ResponseWriter, r *http.Request) {
	wnd, err := parseWindow(r, h.now())
	if err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	if h.Svc == nil {
		writeJSON(w, http.StatusOK, statusDTO{Status: string(StatusUnavailable), InventoryStatus: string(StatusUnavailable), TelemetryStatus: string(StatusUnavailable), ObservedAt: h.now().Format(time.RFC3339Nano), Window: toWindowDTO(wnd), Nodes: []nodeDTO{}})
		return
	}
	result, err := h.Svc.Status(r.Context(), wnd)
	if err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, toStatusDTO(result))
}

func (h *HTTP) logs(w http.ResponseWriter, r *http.Request) {
	wnd, err := parseWindow(r, h.now())
	if err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	limit, err := queryInt(r, "limit", MaxLogPageSize)
	if err != nil || limit <= 0 || limit > MaxLogPageSize {
		writeObservationError(w, http.StatusBadRequest, errors.New("limit must be between 1 and 200"))
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil || offset < 0 || offset > MaxLogOffset {
		writeObservationError(w, http.StatusBadRequest, errors.New("offset is outside the bounded range"))
		return
	}
	severity := strings.TrimSpace(r.URL.Query().Get("severity"))
	if !ValidSeverity(severity) {
		writeObservationError(w, http.StatusBadRequest, errors.New("unsupported severity filter"))
		return
	}
	search := r.URL.Query().Get("search")
	if len([]rune(search)) > MaxSearchRunes {
		writeObservationError(w, http.StatusBadRequest, errors.New("search text is too long"))
		return
	}
	service, err := parseServiceFilter(r.URL.Query().Get("service"))
	if err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	q := LogQuery{Service: service, Window: wnd, Severity: severity, Search: CleanSearch(search), Limit: limit, Offset: offset}
	if err := q.Validate(); err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	if h.Svc == nil {
		writeEvidence(w, Result{Status: StatusUnavailable, Window: wnd}, ErrUnavailable)
		return
	}
	res, err := h.Svc.Logs(r.Context(), q)
	writeEvidence(w, res, err)
}

func (h *HTTP) nodeMetrics(w http.ResponseWriter, r *http.Request) {
	wnd, q, err := parseMetricQuery(r, h.now())
	if err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	if h.Svc == nil {
		writeEvidence(w, Result{Status: StatusUnavailable, Window: wnd}, ErrUnavailable)
		return
	}
	res, err := h.Svc.NodeMetrics(r.Context(), q)
	writeEvidence(w, res, err)
}

func (h *HTTP) serviceMetrics(w http.ResponseWriter, r *http.Request) {
	_, q, err := parseMetricQuery(r, h.now())
	if err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	if h.Svc == nil {
		writeEvidence(w, Result{Status: StatusUnavailable, Window: q.Window}, ErrUnavailable)
		return
	}
	res, err := h.Svc.ServiceMetrics(r.Context(), q)
	writeEvidence(w, res, err)
}

func (h *HTTP) traces(w http.ResponseWriter, r *http.Request) {
	wnd, err := parseWindow(r, h.now())
	if err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	limit, err := queryInt(r, "limit", MaxTracePageSize)
	if err != nil || limit <= 0 || limit > MaxTracePageSize {
		writeObservationError(w, http.StatusBadRequest, errors.New("limit must be between 1 and 50"))
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil || offset < 0 || offset > MaxTraceOffset {
		writeObservationError(w, http.StatusBadRequest, errors.New("offset is outside the bounded range"))
		return
	}
	service, err := parseServiceFilter(r.URL.Query().Get("service"))
	if err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	q := TraceQuery{Service: service, Window: wnd, ErrorsOnly: queryBool(r, "errors_only"), Limit: limit, Offset: offset}
	if err := q.Validate(); err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	if h.Svc == nil {
		writeEvidence(w, Result{Status: StatusUnavailable, Window: wnd}, ErrUnavailable)
		return
	}
	res, err := h.Svc.Traces(r.Context(), q)
	writeEvidence(w, res, err)
}

func (h *HTTP) trace(w http.ResponseWriter, r *http.Request) {
	wnd, err := parseWindow(r, h.now())
	if err != nil {
		writeObservationError(w, http.StatusBadRequest, err)
		return
	}
	id := chi.URLParam(r, "trace_id")
	if !ValidTraceID(id) {
		writeObservationError(w, http.StatusBadRequest, errors.New("trace id must be 32 lowercase hex characters"))
		return
	}
	if h.Svc == nil {
		writeEvidence(w, Result{Status: StatusUnavailable, Window: wnd}, ErrUnavailable)
		return
	}
	res, err := h.Svc.Trace(r.Context(), wnd, id)
	writeEvidence(w, res, err)
}

func parseWindow(r *http.Request, now time.Time) (Window, error) {
	q := r.URL.Query()
	start, end := strings.TrimSpace(q.Get("start")), strings.TrimSpace(q.Get("end"))
	if start != "" || end != "" {
		if start == "" || end == "" {
			return Window{}, errors.New("start and end must be supplied together")
		}
		s, err := time.Parse(time.RFC3339Nano, start)
		if err != nil {
			return Window{}, errors.New("start must be an RFC3339 timestamp")
		}
		e, err := time.Parse(time.RFC3339Nano, end)
		if err != nil {
			return Window{}, errors.New("end must be an RFC3339 timestamp")
		}
		w := Window{Start: s.UTC(), End: e.UTC()}
		if err := (Query{Window: w, Limit: 1}).Validate(); err != nil {
			return Window{}, err
		}
		return w, nil
	}
	rangeValue := q.Get("range")
	if rangeValue == "" {
		rangeValue = q.Get("window")
	}
	return ParseWindow(rangeValue, now)
}

func parseMetricQuery(r *http.Request, now time.Time) (Window, MetricQuery, error) {
	wnd, err := parseWindow(r, now)
	if err != nil {
		return Window{}, MetricQuery{}, err
	}
	bucket := BucketFor(wnd)
	if value := strings.TrimSpace(r.URL.Query().Get("bucket")); value != "" {
		bucket, err = time.ParseDuration(value)
		if err != nil {
			return Window{}, MetricQuery{}, errors.New("bucket must be a duration")
		}
	}
	q := MetricQuery{Window: wnd, Bucket: bucket}
	return wnd, q, q.Validate()
}

func queryInt(r *http.Request, key string, def int) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return def, nil
	}
	return strconv.Atoi(value)
}

func queryBool(r *http.Request, key string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseServiceFilter(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.Contains(value, "/") {
		parsed, err := ParseServicePath(value)
		if err != nil {
			return "", err
		}
		return parsed.Stack + "_" + parsed.Service, nil
	}
	if _, err := ParseServicePath(value); err != nil {
		return "", err
	}
	return value, nil
}

func writeEvidence(w http.ResponseWriter, res Result, err error) {
	dto := toEvidenceDTO(res)
	if err != nil || res.Status == StatusUnavailable {
		dto.Status = string(StatusUnavailable)
		dto.Error = "observability source unavailable"
	}
	writeJSON(w, http.StatusOK, dto)
}

func writeObservationError(w http.ResponseWriter, status int, err error) {
	message := "invalid observability request"
	if err != nil {
		message = cleanError(err.Error())
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func toWindowDTO(w Window) windowDTO {
	return windowDTO{Start: w.Start.UTC().Format(time.RFC3339Nano), End: w.End.UTC().Format(time.RFC3339Nano)}
}

func toEvidenceDTO(res Result) evidenceDTO {
	rows := make([]map[string]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		rows = append(rows, cleanMap(map[string]any(row)))
	}
	status := string(res.Status)
	if res.Status == StatusAvailable && len(rows) == 0 {
		status = string(StatusEmpty)
	}
	return evidenceDTO{Status: status, ObservedAt: res.ObservedAt.UTC().Format(time.RFC3339Nano), Window: toWindowDTO(res.Window), Rows: rows, Truncated: res.Truncated, TruncationReasons: append([]string(nil), res.TruncationReasons...), Coverage: cleanError(res.Coverage)}
}

func toStatusDTO(res StatusResult) statusDTO {
	nodes := make([]nodeDTO, 0, len(res.Nodes))
	for _, node := range res.Nodes {
		d := nodeDTO{ID: cleanError(node.ID), Hostname: cleanError(node.Hostname), Role: cleanError(node.Role), Availability: cleanError(node.Availability), SwarmStatus: cleanError(node.SwarmStatus), IsLeader: node.IsLeader, TelemetryStatus: string(node.TelemetryStatus)}
		if !node.LastSeen.IsZero() {
			d.LastSeen = node.LastSeen.UTC().Format(time.RFC3339Nano)
		}
		nodes = append(nodes, d)
	}
	return statusDTO{Status: string(res.Status), InventoryStatus: string(res.InventoryStatus), TelemetryStatus: string(res.TelemetryStatus), ObservedAt: res.ObservedAt.UTC().Format(time.RFC3339Nano), Window: toWindowDTO(res.Window), ExpectedNodes: res.ExpectedNodes, ReportingNodes: res.ReportingNodes, UnknownNodeSamples: res.UnknownNodeSamples, Truncated: res.Truncated, TruncationReasons: append([]string(nil), res.TruncationReasons...), Nodes: nodes}
}

// writeJSON mirrors the other daemon packages: a JSON body with an explicit
// content type and status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func cleanError(value string) string {
	return FlattenTerminalText(value)
}

func cleanMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[cleanError(key)] = cleanValue(value)
	}
	return out
}

func cleanValue(value any) any {
	switch v := value.(type) {
	case string:
		return cleanError(v)
	case map[string]any:
		return cleanMap(v)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = cleanValue(v[i])
		}
		return out
	default:
		return value
	}
}
