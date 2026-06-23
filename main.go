package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type App struct {
	tmpl         *template.Template
	ipamTmpl     *template.Template
	runbooksTmpl *template.Template
	resourceTmpl *template.Template

	// cache holds the expensive, slowly-changing host metrics (per-container
	// `docker stats` and host CPU/mem/disk usage) so they never block a page
	// request. A background goroutine refreshes it on a ticker; requests only
	// ever read from RAM.
	cache   *statsCache
	metrics *metrics
}

// hostInfo holds the slowly-changing, expensive-to-collect host facts. On this
// class of host `docker system df` alone can take 15s+, so these are refreshed
// on a much slower cadence than per-container stats.
type hostInfo struct {
	cpuCores  int
	memTotal  int64
	diskUsed  int64
	diskTotal int64
}

// statsCache is a warm, mutex-guarded snapshot of the costly Docker metrics.
// `docker stats --no-stream` (~2s) and `docker system df` (~15s) are collected
// off the request path by background goroutines so page loads only read RAM.
type statsCache struct {
	mu          sync.RWMutex
	stats       map[string]containerStat
	images      int
	usage       HostUsage
	collectedAt time.Time
	collectMS   int64
	collectErr  string
	ready       bool

	host   hostInfo
	hostAt time.Time
}

func newStatsCache() *statsCache {
	return &statsCache{stats: map[string]containerStat{}}
}

func (c *statsCache) read() (map[string]containerStat, HostUsage, int, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats, c.usage, c.images, c.collectedAt
}

func (c *statsCache) usageSnapshot() HostUsage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.usage
}

func (c *statsCache) store(stats map[string]containerStat, images int, usage HostUsage, took time.Duration, collectErr string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats = stats
	c.images = images
	c.usage = usage
	c.collectMS = took.Milliseconds()
	c.collectErr = collectErr
	c.collectedAt = now
	c.ready = true
}

func (c *statsCache) hostSnapshot() (hostInfo, time.Duration) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.hostAt.IsZero() {
		return c.host, time.Duration(1<<62 - 1) // effectively "infinitely stale"
	}
	return c.host, time.Since(c.hostAt)
}

func (c *statsCache) storeHost(h hostInfo, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.host = h
	c.hostAt = now
}

func (c *statsCache) snapshot() (ageMS, collectMS int64, ready bool, collectErr string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.collectedAt.IsZero() {
		return 0, c.collectMS, c.ready, c.collectErr
	}
	return time.Since(c.collectedAt).Milliseconds(), c.collectMS, c.ready, c.collectErr
}

// metrics holds process-wide counters exposed at /metrics in Prometheus text
// format and summarised at /healthz.
type metrics struct {
	requestsTotal int64
	errorsTotal   int64
	collectsTotal int64
	collectErrors int64
	requestNanos  int64 // cumulative handler latency, for an avg
	startedAtUnix int64
}

type RunbookStep struct {
	Label   string
	Command string
}

type Runbook struct {
	ID          string
	Name        string
	Description string
	Category    string
	Risk        string
	Steps       []RunbookStep
	UserCreated bool `json:",omitempty"`
}

type RunbookStepResult struct {
	Index    int
	Label    string
	Command  string
	Output   string
	Err      string
	Executed bool
}

type RunbookView struct {
	Runbook
	Selected bool
	Results  []RunbookStepResult
}

type RunbooksData struct {
	Runbooks   []RunbookView
	Selected   *RunbookView
	Now        string
	DockerHost string
	Success    string
	Error      string
	AIModel    string
	AIAnalysis string
	Proposal   *RunbookProposal
}

// RunbookProposal holds an AI-generated set of replacement steps for a runbook,
// shown as a before/after preview that the user can apply or discard.
type RunbookProposal struct {
	RunbookID    string
	Rationale    string
	CurrentSteps []RunbookStep
	NewSteps     []RunbookStep
	EncodedSteps string // JSON of NewSteps, carried in the apply form
}

// runbookProposalResponse is the JSON contract returned by the model.
type runbookProposalResponse struct {
	Rationale string        `json:"rationale"`
	Steps     []RunbookStep `json:"steps"`
}

type PageData struct {
	Containers    []ContainerView
	Total         int
	Running       int
	Stopped       int
	Images        int
	Usage         HostUsage
	Search        string
	CommandInput  string
	CommandOutput string
	AIPrompt      string
	AISuggestion  string
	AIExplanation string
	AIModel       string
	Error         string
	Success       string
	Now           string
	DockerHost    string
	// AISteps holds Docker commands extracted from an AI container analysis so the
	// operator can run them ad-hoc or save them permanently as a runbook.
	AISteps        []RunbookStep
	AIStepsEncoded string
	AIStepsTitle   string
}

type AISuggestion struct {
	Command     string `json:"command"`
	Explanation string `json:"explanation"`
}

type ContainerView struct {
	ID       string
	Name     string
	Image    string
	Status   string
	State    string
	Created  string
	Ports    string
	CPUPerc  string
	MemUsage string
	MemPerc  string
}

type HostUsage struct {
	CPUPercent     float64
	CPUCores       int
	MemUsedBytes   int64
	MemTotalBytes  int64
	MemPercent     float64
	DiskUsedBytes  int64
	DiskTotalBytes int64
	DiskPercent    float64
	CPUUsedLabel   string
	CPUTotalLabel  string
	MemUsedLabel   string
	MemTotalLabel  string
	DiskUsedLabel  string
	DiskTotalLabel string
	Error          string
}

type PortMapping struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string
	ContainerName string
	ContainerID   string
	State         string
	InternalIP    string
	Network       string
}

type PortRange struct {
	Start    int
	End      int
	Protocol string
	Display  string
}

type NetworkContainer struct {
	Name       string
	ID         string
	InternalIP string
}

type DockerNetwork struct {
	Name       string
	Driver     string
	Subnet     string
	Gateway    string
	Scope      string
	Containers []NetworkContainer
}

type IPAMData struct {
	PortMappings []PortMapping
	UsedRanges   []PortRange
	TotalPorts   int
	Networks     []DockerNetwork
	Now          string
	DockerHost   string
	Error        string
	Success      string
	AIModel      string
	AIAnalysis   string
}

func main() {
	funcMap := template.FuncMap{"add": func(a, b int) int { return a + b }}
	app := &App{
		tmpl:         template.Must(template.New("index").Parse(indexHTML)),
		ipamTmpl:     template.Must(template.New("ipam").Funcs(funcMap).Parse(ipamHTML)),
		runbooksTmpl: template.Must(template.New("runbooks").Funcs(funcMap).Parse(runbooksHTML)),
		resourceTmpl: template.Must(template.New("resource").Funcs(funcMap).Parse(resourceHTML)),
		cache:        newStatsCache(),
		metrics:      &metrics{startedAtUnix: time.Now().Unix()},
	}

	if err := loadUserRunbooks(); err != nil {
		log.Printf("warning: could not load user runbooks: %v", err)
	}
	if err := loadRunbookOverrides(); err != nil {
		log.Printf("warning: could not load runbook overrides: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
	mux.HandleFunc("/landing", app.handleLanding)
	mux.HandleFunc("/ipam", app.handleIPAM)
	mux.HandleFunc("/ipam/analyze", app.handleIPAMAnalyze)
	mux.HandleFunc("/images", app.handleImages)
	mux.HandleFunc("/images/action", func(w http.ResponseWriter, r *http.Request) { app.handleResourceAction("images", w, r) })
	mux.HandleFunc("/volumes", app.handleVolumes)
	mux.HandleFunc("/volumes/action", func(w http.ResponseWriter, r *http.Request) { app.handleResourceAction("volumes", w, r) })
	mux.HandleFunc("/networks", app.handleNetworks)
	mux.HandleFunc("/networks/action", func(w http.ResponseWriter, r *http.Request) { app.handleResourceAction("networks", w, r) })
	mux.HandleFunc("/runbooks", app.handleRunbooks)
	mux.HandleFunc("/runbooks/execute", app.handleRunbookExecute)
	mux.HandleFunc("/runbooks/analyze", app.handleRunbookAnalyze)
	mux.HandleFunc("/runbooks/propose", app.handleRunbookPropose)
	mux.HandleFunc("/runbooks/apply", app.handleRunbookApply)
	mux.HandleFunc("/", app.handleDashboard)
	mux.HandleFunc("/docker/exec", app.handleDockerCommand)
	mux.HandleFunc("/ai/interpret", app.handleAIInterpret)
	mux.HandleFunc("/ai/runbook/run", app.handleAIRunbookRun)
	mux.HandleFunc("/ai/runbook/save", app.handleAIRunbookSave)
	mux.HandleFunc("/containers/create", app.handleCreate)
	mux.HandleFunc("/containers/", app.handleContainerAction)
	mux.HandleFunc("/api/dashboard.json", app.handleDashboardAPI)
	mux.HandleFunc("/healthz", app.handleHealthz)
	mux.HandleFunc("/metrics", app.handleMetrics)

	// Continuously refresh the warm caches so page requests never pay the cost
	// of `docker stats` (~2s) or `docker system df` (can be 15s+). Host facts
	// change slowly, so they refresh on a much longer cadence than live stats.
	refreshInterval := envDurationOrDefault("STATS_REFRESH_INTERVAL", 3*time.Second)
	hostInterval := envDurationOrDefault("HOST_REFRESH_INTERVAL", 60*time.Second)
	app.startStatsCollector(refreshInterval, hostInterval)

	addr := envOrDefault("ADDR", ":8090")
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.withBasicAuth(withLogging(app.metrics, mux)),
		ReadHeaderTimeout: 10 * time.Second,
		// Generous write timeout: long enough for slow AI/runbook operations
		// (each Docker step is itself capped at 30s) but bounds hung sockets.
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	log.Printf("dockpilot listening on %s (stats every %s, host facts every %s)", addr, refreshInterval, hostInterval)
	log.Fatal(srv.ListenAndServe())
}

func (a *App) withBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Monitoring probes must be scrapeable without credentials.
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		user, pass, ok := r.BasicAuth()
		expectedUser, expectedPass, err := readAuthFile(envOrDefault("AUTH_FILE", "./dockpilot.auth"))
		if err != nil {
			log.Printf("auth file error: %v", err)
			authChallenge(w)
			return
		}

		if !ok || user != expectedUser || pass != expectedPass {
			authChallenge(w)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func authChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="DockPilot"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func readAuthFile(path string) (string, string, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("cannot read auth file %s: %w", path, err)
	}

	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		u := strings.TrimSpace(parts[0])
		p := strings.TrimSpace(parts[1])
		if u != "" && p != "" {
			return u, p, nil
		}
	}

	return "", "", fmt.Errorf("auth file must contain 'username:password'")
}

// statusRecorder captures the response status code so the logging middleware
// can report it and count server errors for /metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Flush exposes the underlying flusher so streaming responses keep working.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withLogging(m *metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}

		if m != nil {
			atomic.AddInt64(&m.requestsTotal, 1)
			atomic.AddInt64(&m.requestNanos, dur.Nanoseconds())
			if rec.status >= 500 {
				atomic.AddInt64(&m.errorsTotal, 1)
			}
		}

		// Structured, key=value log line — greppable and easy to ship to a
		// log pipeline.
		log.Printf("method=%s path=%s status=%d dur=%s remote=%s",
			r.Method, r.URL.Path, rec.status, dur.Round(time.Millisecond), clientIP(r))
	})
}

// clientIP extracts the best-effort client address, honouring a reverse proxy's
// X-Forwarded-For header (DockPilot runs behind one in production).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	search := strings.TrimSpace(r.URL.Query().Get("q"))
	d, err := a.buildDashboardData(search)
	if err != nil {
		a.render(w, PageData{Error: err.Error(), Usage: a.cache.usageSnapshot()})
		return
	}

	d.Success = r.URL.Query().Get("success")
	d.Error = r.URL.Query().Get("error")
	a.render(w, d)
}

func (a *App) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/landing" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "./docs/landing/index.html")
}

var runbookCatalog = []Runbook{
	{
		ID:          "system-cleanup",
		Name:        "System Cleanup — Reclaim Disk",
		Description: "Removes stopped containers, dangling images, unused networks and volumes. Safe for routine housekeeping.",
		Category:    "cleanup",
		Risk:        "low",
		Steps: []RunbookStep{
			{Label: "Snapshot disk usage (before)", Command: "system df"},
			{Label: "Prune stopped containers", Command: "container prune -f"},
			{Label: "Prune dangling images", Command: "image prune -f"},
			{Label: "Prune unused networks", Command: "network prune -f"},
			{Label: "Snapshot disk usage (after)", Command: "system df"},
		},
	},
	{
		ID:          "deep-cleanup",
		Name:        "Deep Cleanup — Purge Unused Images & Volumes",
		Description: "Aggressive reclaim. Also removes unused (not just dangling) images and orphaned volumes. Irreversible.",
		Category:    "cleanup",
		Risk:        "high",
		Steps: []RunbookStep{
			{Label: "Snapshot disk usage (before)", Command: "system df"},
			{Label: "Prune stopped containers", Command: "container prune -f"},
			{Label: "Prune ALL unused images", Command: "image prune -a -f"},
			{Label: "Prune unused volumes", Command: "volume prune -f"},
			{Label: "Prune unused networks", Command: "network prune -f"},
			{Label: "Snapshot disk usage (after)", Command: "system df"},
		},
	},
	{
		ID:          "health-check",
		Name:        "Docker Health Check",
		Description: "Read-only diagnostics: daemon version, system info, disk usage, and container inventory.",
		Category:    "diagnostics",
		Risk:        "low",
		Steps: []RunbookStep{
			{Label: "Docker version", Command: "version"},
			{Label: "Docker info", Command: "info"},
			{Label: "Disk usage", Command: "system df"},
			{Label: "All containers", Command: "ps -a"},
			{Label: "Resource stats", Command: "stats --no-stream"},
		},
	},
	{
		ID:          "restart-unhealthy",
		Name:        "Restart Unhealthy Containers",
		Description: "Identifies unhealthy containers and restarts them one by one. Useful after transient failures.",
		Category:    "recovery",
		Risk:        "medium",
		Steps: []RunbookStep{
			{Label: "List unhealthy containers", Command: "ps --filter health=unhealthy --format {{.Names}}"},
			{Label: "Inspect unhealthy container state", Command: "ps --filter health=unhealthy"},
			{Label: "Restart unhealthy containers", Command: "restart $(docker ps -q --filter health=unhealthy)"},
			{Label: "Re-check health", Command: "ps --filter health=unhealthy"},
		},
	},
	{
		ID:          "network-audit",
		Name:        "Network Audit",
		Description: "Inventory Docker networks, subnets, and attached containers. Helpful when diagnosing connectivity.",
		Category:    "diagnostics",
		Risk:        "low",
		Steps: []RunbookStep{
			{Label: "List networks", Command: "network ls"},
			{Label: "Inspect bridge network", Command: "network inspect bridge"},
			{Label: "Show port mappings", Command: "ps --format {{.Names}}\\t{{.Ports}}"},
		},
	},
	{
		ID:          "image-audit",
		Name:        "Image Audit",
		Description: "Review image inventory, identify largest images and dangling layers before a cleanup run.",
		Category:    "diagnostics",
		Risk:        "low",
		Steps: []RunbookStep{
			{Label: "List images", Command: "images"},
			{Label: "Dangling images only", Command: "images -f dangling=true"},
			{Label: "Disk usage by image", Command: "system df -v"},
		},
	},
	{
		ID:          "stop-all",
		Name:        "Stop All Running Containers",
		Description: "Stops every running container. Use for host maintenance or emergency drain. Reversible with start.",
		Category:    "recovery",
		Risk:        "high",
		Steps: []RunbookStep{
			{Label: "List running containers", Command: "ps"},
			{Label: "Stop all running containers", Command: "stop $(docker ps -q)"},
			{Label: "Verify all stopped", Command: "ps -a"},
		},
	},
}

// runbookMu guards runbookCatalog, which becomes mutable once a user applies
// AI-suggested step changes.
var runbookMu sync.RWMutex

// getRunbook returns a copy (including a copied Steps slice) of the runbook with
// the given id, so callers can read it without holding the lock.
func getRunbook(id string) (Runbook, bool) {
	runbookMu.RLock()
	defer runbookMu.RUnlock()
	for i := range runbookCatalog {
		if runbookCatalog[i].ID == id {
			rb := runbookCatalog[i]
			rb.Steps = append([]RunbookStep(nil), rb.Steps...)
			return rb, true
		}
	}
	return Runbook{}, false
}

// snapshotRunbooks returns a deep copy of the whole catalog for safe iteration.
func snapshotRunbooks() []Runbook {
	runbookMu.RLock()
	defer runbookMu.RUnlock()
	out := make([]Runbook, len(runbookCatalog))
	for i := range runbookCatalog {
		out[i] = runbookCatalog[i]
		out[i].Steps = append([]RunbookStep(nil), runbookCatalog[i].Steps...)
	}
	return out
}

// replaceRunbookSteps swaps in a new step list for the given runbook id and
// persists the change to disk so it survives restarts.
func replaceRunbookSteps(id string, steps []RunbookStep) bool {
	runbookMu.Lock()
	defer runbookMu.Unlock()
	for i := range runbookCatalog {
		if runbookCatalog[i].ID == id {
			runbookCatalog[i].Steps = steps
			if err := saveRunbookOverridesLocked(); err != nil {
				log.Printf("warning: failed to persist runbook overrides: %v", err)
			}
			return true
		}
	}
	return false
}

func runbooksFilePath() string {
	return envOrDefault("RUNBOOKS_FILE", "./dockpilot-runbooks.json")
}

// loadRunbookOverrides reads persisted step overrides (a map of runbook id ->
// steps) and applies them onto the in-memory catalog. Missing/empty file is not
// an error. Overrides for unknown runbook ids are ignored.
func loadRunbookOverrides() error {
	path := runbooksFilePath()
	b, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil
	}

	var overrides map[string][]RunbookStep
	if err := json.Unmarshal(b, &overrides); err != nil {
		return fmt.Errorf("invalid runbooks file %s: %w", path, err)
	}

	runbookMu.Lock()
	defer runbookMu.Unlock()
	for i := range runbookCatalog {
		if steps, ok := overrides[runbookCatalog[i].ID]; ok {
			steps = sanitizeRunbookSteps(steps)
			if len(steps) > 0 {
				runbookCatalog[i].Steps = steps
			}
		}
	}
	return nil
}

// saveRunbookOverridesLocked writes the full set of current runbook steps to the
// overrides file. Callers MUST hold runbookMu (write lock).
func saveRunbookOverridesLocked() error {
	overrides := make(map[string][]RunbookStep, len(runbookCatalog))
	for i := range runbookCatalog {
		if runbookCatalog[i].UserCreated {
			continue // user runbooks live in their own file
		}
		overrides[runbookCatalog[i].ID] = runbookCatalog[i].Steps
	}

	b, err := json.MarshalIndent(overrides, "", "  ")
	if err != nil {
		return err
	}

	path := runbooksFilePath()
	tmp := path + ".tmp"
	if err := ioutil.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *App) renderRunbooks(w http.ResponseWriter, data RunbooksData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.runbooksTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) handleRunbooks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/runbooks" {
		http.NotFound(w, r)
		return
	}
	selectedID := strings.TrimSpace(r.URL.Query().Get("id"))

	data := RunbooksData{
		Now:        time.Now().Format("2006-01-02 15:04:05"),
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		AIModel:    envOrDefault("OLLAMA_MODEL", "llama3"),
	}
	for _, rb := range snapshotRunbooks() {
		view := RunbookView{Runbook: rb, Selected: rb.ID == selectedID}
		if view.Selected {
			view.Results = make([]RunbookStepResult, len(rb.Steps))
			for i, s := range rb.Steps {
				view.Results[i] = RunbookStepResult{Index: i, Label: s.Label, Command: s.Command}
			}
			data.Selected = &view
		}
		data.Runbooks = append(data.Runbooks, view)
	}
	a.renderRunbooks(w, data)
}

func (a *App) handleRunbookExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	runbookID := strings.TrimSpace(r.FormValue("runbook_id"))
	mode := strings.TrimSpace(r.FormValue("mode"))
	stepStr := strings.TrimSpace(r.FormValue("step"))

	rbVal, ok := getRunbook(runbookID)
	if !ok {
		http.Error(w, "runbook not found: "+runbookID, http.StatusNotFound)
		return
	}
	rb := &rbVal

	data := RunbooksData{
		Now:        time.Now().Format("2006-01-02 15:04:05"),
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		AIModel:    envOrDefault("OLLAMA_MODEL", "llama3"),
	}

	results := make([]RunbookStepResult, len(rb.Steps))
	for i, s := range rb.Steps {
		results[i] = RunbookStepResult{Index: i, Label: s.Label, Command: s.Command}
	}

	if mode == "all" {
		for i, s := range rb.Steps {
			out, runErr := executeRunbookCommand(s.Command)
			results[i].Output = out
			results[i].Executed = true
			if runErr != nil {
				results[i].Err = runErr.Error()
			}
		}
		data.Success = fmt.Sprintf("Runbook \"%s\" executed (%d steps)", rb.Name, len(rb.Steps))
	} else {
		stepIdx, err := strconv.Atoi(stepStr)
		if err != nil || stepIdx < 0 || stepIdx >= len(rb.Steps) {
			data.Error = "invalid step index"
		} else {
			out, runErr := executeRunbookCommand(rb.Steps[stepIdx].Command)
			results[stepIdx].Output = out
			results[stepIdx].Executed = true
			if runErr != nil {
				results[stepIdx].Err = runErr.Error()
				data.Error = fmt.Sprintf("step %d failed", stepIdx+1)
			} else {
				data.Success = fmt.Sprintf("step %d executed", stepIdx+1)
			}
		}
	}

	for _, cat := range snapshotRunbooks() {
		view := RunbookView{Runbook: cat, Selected: cat.ID == rb.ID}
		if view.Selected {
			view.Results = results
			data.Selected = &view
		}
		data.Runbooks = append(data.Runbooks, view)
	}
	a.renderRunbooks(w, data)
}

func (a *App) handleRunbookAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	runbookID := strings.TrimSpace(r.FormValue("runbook_id"))

	rbVal, ok := getRunbook(runbookID)
	if !ok {
		http.Error(w, "runbook not found: "+runbookID, http.StatusNotFound)
		return
	}

	data := newRunbooksData(runbookID)

	analysis, aiErr := analyzeRunbookWithOllama(&rbVal)
	if aiErr != nil {
		data.Error = fmt.Sprintf("AI analysis failed: %v", aiErr)
	} else {
		data.AIAnalysis = analysis
		data.Success = fmt.Sprintf("AI analysis ready for \"%s\"", rbVal.Name)
	}

	a.renderRunbooks(w, data)
}

// newRunbooksData builds RunbooksData with the catalog populated and the given
// runbook selected (with its steps materialized as results).
func newRunbooksData(selectedID string) RunbooksData {
	data := RunbooksData{
		Now:        time.Now().Format("2006-01-02 15:04:05"),
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		AIModel:    envOrDefault("OLLAMA_MODEL", "llama3"),
	}
	for _, cat := range snapshotRunbooks() {
		view := RunbookView{Runbook: cat, Selected: cat.ID == selectedID}
		if view.Selected {
			view.Results = make([]RunbookStepResult, len(cat.Steps))
			for i, s := range cat.Steps {
				view.Results[i] = RunbookStepResult{Index: i, Label: s.Label, Command: s.Command}
			}
			data.Selected = &view
		}
		data.Runbooks = append(data.Runbooks, view)
	}
	return data
}

func (a *App) handleRunbookPropose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	runbookID := strings.TrimSpace(r.FormValue("runbook_id"))

	rbVal, ok := getRunbook(runbookID)
	if !ok {
		http.Error(w, "runbook not found: "+runbookID, http.StatusNotFound)
		return
	}

	data := newRunbooksData(runbookID)

	proposal, aiErr := proposeRunbookStepsWithOllama(&rbVal)
	if aiErr != nil {
		data.Error = fmt.Sprintf("AI proposal failed: %v", aiErr)
		a.renderRunbooks(w, data)
		return
	}

	encoded, _ := json.Marshal(proposal.Steps)
	data.Proposal = &RunbookProposal{
		RunbookID:    runbookID,
		Rationale:    proposal.Rationale,
		CurrentSteps: rbVal.Steps,
		NewSteps:     proposal.Steps,
		EncodedSteps: string(encoded),
	}
	data.Success = fmt.Sprintf("AI proposed %d step(s) for \"%s\" — review and apply below", len(proposal.Steps), rbVal.Name)
	a.renderRunbooks(w, data)
}

func (a *App) handleRunbookApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	runbookID := strings.TrimSpace(r.FormValue("runbook_id"))
	encoded := r.FormValue("steps")

	var steps []RunbookStep
	if err := json.Unmarshal([]byte(encoded), &steps); err != nil {
		http.Error(w, "invalid steps payload", http.StatusBadRequest)
		return
	}
	steps = sanitizeRunbookSteps(steps)
	if len(steps) == 0 {
		data := newRunbooksData(runbookID)
		data.Error = "no valid steps to apply"
		a.renderRunbooks(w, data)
		return
	}

	if !replaceRunbookSteps(runbookID, steps) {
		http.Error(w, "runbook not found: "+runbookID, http.StatusNotFound)
		return
	}

	data := newRunbooksData(runbookID)
	data.Success = fmt.Sprintf("Applied AI-suggested steps (%d) to the runbook", len(steps))
	a.renderRunbooks(w, data)
}

// sanitizeRunbookSteps drops empty steps and strips a leading "docker " prefix so
// stored commands match the existing catalog convention.
func sanitizeRunbookSteps(steps []RunbookStep) []RunbookStep {
	out := make([]RunbookStep, 0, len(steps))
	for _, s := range steps {
		cmd := strings.TrimSpace(s.Command)
		cmd = strings.TrimPrefix(cmd, "docker ")
		cmd = strings.TrimSpace(cmd)
		label := strings.TrimSpace(s.Label)
		if cmd == "" {
			continue
		}
		if label == "" {
			label = cmd
		}
		out = append(out, RunbookStep{Label: label, Command: cmd})
	}
	return out
}

// dockerCmdRe matches a "docker ..." command, whether wrapped in backticks or
// inline in prose. It stops at a backtick, newline, or sentence-ending period.
var dockerCmdRe = regexp.MustCompile("docker[ \\t]+[^`\\n]+")

// interactiveFlags are stripped from AI-suggested commands so they can run as a
// safe one-shot (no follow/attach/tty that would block forever).
var interactiveFlags = map[string]bool{
	"-f": true, "--follow": true,
	"-it": true, "-ti": true,
	"-i": true, "--interactive": true,
	"-t": true, "--tty": true,
	"-d": true, "--detach": true,
}

// sanitizeAIStepCommand normalizes a raw "docker ..." command into a safe,
// non-blocking subcommand (without the leading "docker"). Returns "" if nothing
// runnable remains.
func sanitizeAIStepCommand(raw string) string {
	cmd := strings.TrimSpace(raw)
	cmd = strings.TrimPrefix(cmd, "docker ")
	cmd = strings.Trim(cmd, " \t.`'\"")
	if cmd == "" {
		return ""
	}
	fields := strings.Fields(cmd)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if interactiveFlags[f] {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return ""
	}
	joined := strings.Join(out, " ")
	// Force streaming subcommands into single-shot mode (handles both "docker
	// stats ..." and the verbose "docker container stats ..." forms).
	if hasWord(out, "stats") && !strings.Contains(joined, "--no-stream") {
		joined += " --no-stream"
	}
	if hasWord(out, "logs") && !strings.Contains(joined, "--tail") {
		joined += " --tail 200"
	}
	return strings.TrimSpace(joined)
}

func hasWord(fields []string, w string) bool {
	for _, f := range fields {
		if f == w {
			return true
		}
	}
	return false
}

// extractRunbookStepsFromText pulls Docker CLI commands out of a free-text AI
// analysis and returns them as sanitized, de-duplicated runbook steps.
func extractRunbookStepsFromText(text string) []RunbookStep {
	var steps []RunbookStep
	seen := map[string]bool{}
	for _, m := range dockerCmdRe.FindAllString(text, -1) {
		cmd := sanitizeAIStepCommand(m)
		if cmd == "" || seen[cmd] {
			continue
		}
		seen[cmd] = true
		steps = append(steps, RunbookStep{Label: "docker " + cmd, Command: cmd})
	}
	return steps
}

// executeAIStep runs a single Docker subcommand with a hard timeout so an
// interactive or long-running command can never hang the request.
func executeAIStep(raw string) (string, error) {
	cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "docker "))
	if cmd == "" {
		return "", fmt.Errorf("empty command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := strings.Fields(cmd)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("timed out after 30s (skipped interactive/long-running command)")
	}
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("%s", text)
	}
	return text, nil
}

// runbooksUserFilePath is where user-created (AI-saved) runbooks are persisted as
// full definitions, separate from the built-in step overrides file.
func runbooksUserFilePath() string {
	return envOrDefault("RUNBOOKS_USER_FILE", "./dockpilot-user-runbooks.json")
}

// loadUserRunbooks reads persisted user runbooks and appends them to the catalog.
func loadUserRunbooks() error {
	b, err := ioutil.ReadFile(runbooksUserFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil
	}
	var rbs []Runbook
	if err := json.Unmarshal(b, &rbs); err != nil {
		return fmt.Errorf("invalid user runbooks file: %w", err)
	}
	runbookMu.Lock()
	defer runbookMu.Unlock()
	for _, rb := range rbs {
		rb.UserCreated = true
		rb.Steps = sanitizeRunbookSteps(rb.Steps)
		if rb.ID == "" || len(rb.Steps) == 0 {
			continue
		}
		runbookCatalog = append(runbookCatalog, rb)
	}
	return nil
}

// saveUserRunbooksLocked persists all user-created runbooks. Callers MUST hold
// runbookMu (write lock).
func saveUserRunbooksLocked() error {
	var rbs []Runbook
	for i := range runbookCatalog {
		if runbookCatalog[i].UserCreated {
			rbs = append(rbs, runbookCatalog[i])
		}
	}
	b, err := json.MarshalIndent(rbs, "", "  ")
	if err != nil {
		return err
	}
	path := runbooksUserFilePath()
	tmp := path + ".tmp"
	if err := ioutil.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// addUserRunbook appends a new user-created runbook to the catalog and persists it.
func addUserRunbook(rb Runbook) error {
	runbookMu.Lock()
	defer runbookMu.Unlock()
	rb.UserCreated = true
	runbookCatalog = append(runbookCatalog, rb)
	return saveUserRunbooksLocked()
}

// proposeRunbookStepsWithOllama asks the model to return an improved, structured
// step list for the runbook as JSON.
func proposeRunbookStepsWithOllama(rb *Runbook) (runbookProposalResponse, error) {
	systemPrompt := `You are DockPilot AI, an expert Docker operations reviewer.
You are given a named runbook (an ordered list of Docker CLI steps). Produce an improved version.
Return ONLY a JSON object with keys:
- rationale: a short string explaining what you changed and why.
- steps: an array of objects, each with "label" (human description) and "command" (a Docker subcommand WITHOUT the leading word "docker").

Rules:
- Preserve the runbook's original goal and risk level; do not add destructive steps to a low-risk runbook.
- Keep commands limited to the Docker CLI. Never include host shell commands beyond what the original used.
- Prefer safer flags and sensible ordering (e.g. snapshot before/after for cleanup).
- If the runbook is already optimal, return its existing steps unchanged with a rationale saying so.`

	var sb strings.Builder
	fmt.Fprintf(&sb, "Runbook: %s\n", rb.Name)
	fmt.Fprintf(&sb, "Category: %s | Risk: %s\n", rb.Category, rb.Risk)
	fmt.Fprintf(&sb, "Description: %s\n\nCurrent steps:\n", rb.Description)
	for i, s := range rb.Steps {
		fmt.Fprintf(&sb, "%d. %s -> docker %s\n", i+1, s.Label, s.Command)
	}

	raw, err := analyzeWithOllama(systemPrompt, sb.String())
	if err != nil {
		return runbookProposalResponse{}, err
	}

	parsed, err := parseRunbookProposal(raw)
	if err != nil {
		return runbookProposalResponse{}, err
	}
	parsed.Steps = sanitizeRunbookSteps(parsed.Steps)
	if len(parsed.Steps) == 0 {
		return runbookProposalResponse{}, fmt.Errorf("model returned no usable steps")
	}
	if strings.TrimSpace(parsed.Rationale) == "" {
		parsed.Rationale = "AI-proposed step changes."
	}
	return parsed, nil
}

// parseRunbookProposal extracts the JSON proposal from the model output, tolerating
// code fences and surrounding prose.
func parseRunbookProposal(raw string) (runbookProposalResponse, error) {
	var out runbookProposalResponse
	raw = strings.TrimSpace(raw)
	if err := json.Unmarshal([]byte(raw), &out); err == nil {
		return out, nil
	}

	cleaned := raw
	if idx := strings.Index(cleaned, "```"); idx >= 0 {
		cleaned = cleaned[idx+3:]
		if nl := strings.Index(cleaned, "\n"); nl >= 0 {
			cleaned = cleaned[nl+1:]
		}
		if end := strings.Index(cleaned, "```"); end >= 0 {
			cleaned = cleaned[:end]
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(cleaned)), &out); err == nil {
			return out, nil
		}
	}

	if start := strings.Index(raw, "{"); start >= 0 {
		if end := strings.LastIndex(raw, "}"); end > start {
			if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err == nil {
				return out, nil
			}
		}
	}
	return runbookProposalResponse{}, fmt.Errorf("no valid proposal JSON found")
}

// analyzeRunbookWithOllama asks the local model to review a runbook's steps and
// suggest improvements, safer alternatives, or missing steps.
func analyzeRunbookWithOllama(rb *Runbook) (string, error) {
	systemPrompt := `You are DockPilot AI, an expert Docker operations reviewer.
You are given a named runbook (an ordered list of Docker CLI steps) and must review it.
Respond in concise plain text (no JSON, no markdown headers) covering:
1. Overall assessment of whether the steps achieve the runbook's stated goal.
2. Any steps that are redundant, risky, or out of order.
3. Concrete improvements: better flags, safer alternatives, or additional steps that should be added.
4. If the runbook is already optimal, say so clearly.
Keep every command suggestion limited to the Docker CLI. Be specific and actionable.`

	var sb strings.Builder
	fmt.Fprintf(&sb, "Runbook: %s\n", rb.Name)
	fmt.Fprintf(&sb, "Category: %s | Risk: %s\n", rb.Category, rb.Risk)
	fmt.Fprintf(&sb, "Description: %s\n\nSteps:\n", rb.Description)
	for i, s := range rb.Steps {
		fmt.Fprintf(&sb, "%d. %s -> docker %s\n", i+1, s.Label, s.Command)
	}

	return analyzeWithOllama(systemPrompt, sb.String())
}

// analyzeContainerWithOllama gathers a container's metadata, recent logs and
// live stats, then asks the local model for a health/troubleshooting summary.
func analyzeContainerWithOllama(id string) (string, error) {
	inspectOut, _ := runDockerCombined("inspect", "--format",
		"Name={{.Name}} Image={{.Config.Image}} State={{.State.Status}} Health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} RestartCount={{.RestartCount}} ExitCode={{.State.ExitCode}} Error={{.State.Error}}",
		id)
	statsOut, _ := runDockerCombined("stats", "--no-stream", "--format",
		"CPU={{.CPUPerc}} Mem={{.MemUsage}} Mem%={{.MemPerc}} NetIO={{.NetIO}} BlockIO={{.BlockIO}}", id)
	logsOut, _ := runDockerCombined("logs", "--tail", "120", id)

	logsOut = strings.TrimSpace(logsOut)
	if len(logsOut) > 6000 {
		logsOut = logsOut[len(logsOut)-6000:]
	}
	if logsOut == "" {
		logsOut = "(no logs)"
	}

	systemPrompt := `You are DockPilot AI, an expert Docker operations engineer.
You are given one container's metadata, live resource stats, and recent logs.
Respond in concise plain text (no JSON, no markdown headers) covering:
1. Health summary: is the container healthy, degraded, or failing, and why.
2. Notable signals in the logs or stats (errors, restarts, OOM, high CPU/memory).
3. Concrete next steps as Docker CLI commands the operator can run.
If everything looks healthy, say so clearly. Be specific and actionable.`

	userContent := fmt.Sprintf("Metadata: %s\n\nStats: %s\n\nRecent logs (last 120 lines):\n%s",
		strings.TrimSpace(inspectOut), strings.TrimSpace(statsOut), logsOut)

	return analyzeWithOllama(systemPrompt, userContent)
}

func executeRunbookCommand(raw string) (string, error) {
	cmd := strings.TrimSpace(raw)
	cmd = strings.TrimPrefix(cmd, "docker ")
	if cmd == "" {
		return "", fmt.Errorf("empty command")
	}

	if strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") {
		shellCmd := "docker " + cmd
		out, err := exec.Command("sh", "-c", shellCmd).CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err != nil {
			if text == "" {
				text = err.Error()
			}
			return text, fmt.Errorf("%s", text)
		}
		return text, nil
	}

	args := strings.Fields(cmd)
	return runDockerCombined(args...)
}

func (a *App) handleDockerCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	search := strings.TrimSpace(r.FormValue("q"))
	commandText := strings.TrimSpace(r.FormValue("command"))
	commandText = strings.TrimPrefix(commandText, "docker")
	commandText = strings.TrimSpace(commandText)

	d, err := a.buildDashboardData(search)
	if err != nil {
		a.render(w, PageData{Error: err.Error()})
		return
	}
	d.CommandInput = commandText

	if commandText == "" {
		d.Error = "command is required (example: system prune -f)"
		a.render(w, d)
		return
	}

	args := strings.Fields(commandText)
	out, err := runDocker(args...)
	if err != nil {
		d.Error = fmt.Sprintf("docker %s failed: %v", commandText, err)
		if out != "" {
			d.CommandOutput = out
		}
		a.render(w, d)
		return
	}

	d.Success = fmt.Sprintf("docker %s executed", commandText)
	if out == "" {
		d.CommandOutput = "(no output)"
	} else {
		d.CommandOutput = out
	}
	a.render(w, d)
}

func (a *App) handleAIInterpret(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	search := strings.TrimSpace(r.FormValue("q"))
	prompt := strings.TrimSpace(r.FormValue("ai_prompt"))

	d, err := a.buildDashboardData(search)
	if err != nil {
		a.render(w, PageData{Error: err.Error()})
		return
	}
	d.AIPrompt = prompt
	d.AIModel = envOrDefault("OLLAMA_MODEL", "llama3")

	if prompt == "" {
		d.Error = "AI prompt is required"
		a.render(w, d)
		return
	}

	resp, err := interpretDockerCommandWithOllama(prompt)
	if err != nil {
		d.Error = fmt.Sprintf("AI interpret failed: %v", err)
		a.render(w, d)
		return
	}

	d.AISuggestion = strings.TrimSpace(resp.Command)
	d.AIExplanation = strings.TrimSpace(resp.Explanation)
	d.CommandInput = d.AISuggestion
	d.Success = "AI suggestion ready. Review and click Run in Docker Command box."
	a.render(w, d)
}

func (a *App) buildDashboardData(search string) (PageData, error) {

	// Only the container list is fetched fresh on the request path: `docker ps`
	// is fast (~100ms) and must reflect start/stop/remove actions immediately.
	// Everything expensive — per-container stats (~2s), the image count (~2.6s)
	// and host usage (`docker system df` ~15s) — comes from the warm background
	// cache, keeping page loads well under ~300ms.
	containers, listErr := listContainers()
	if listErr != nil {
		return PageData{}, listErr
	}

	stats, usage, images, _ := a.cache.read()
	for i := range containers {
		if s, ok := stats[containers[i].ID]; ok {
			containers[i].CPUPerc = s.cpuPerc
			containers[i].MemUsage = s.memUsage
			containers[i].MemPerc = s.memPerc
		}
	}

	if search != "" {
		containers = filterContainers(containers, search)
	}

	running := 0
	stopped := 0
	for _, c := range containers {
		if c.State == "running" {
			running++
		} else {
			stopped++
		}
	}

	sort.Slice(containers, func(i, j int) bool {
		return containers[i].Created > containers[j].Created
	})

	return PageData{
		Containers:    containers,
		Total:         len(containers),
		Running:       running,
		Stopped:       stopped,
		Images:        images,
		Usage:         usage,
		Search:        search,
		CommandInput:  "",
		CommandOutput: "",
		AIPrompt:      "",
		AISuggestion:  "",
		AIExplanation: "",
		AIModel:       envOrDefault("OLLAMA_MODEL", "llama3"),
		Now:           time.Now().Format("2006-01-02 15:04:05"),
		DockerHost:    envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
	}, nil
}

// startStatsCollector launches two background loops and returns immediately, so
// the HTTP server never blocks on slow Docker calls at startup. The first page
// load may briefly show "—" for live metrics until the first cycle completes.
//
//   - the fast loop refreshes per-container stats (CPU/mem, ~2s) on `interval`
//   - the slow loop refreshes host facts (`docker system df`, ~15s) on a much
//     longer cadence, since disk/CPU-core/mem-total change slowly
func (a *App) startStatsCollector(interval, hostInterval time.Duration) {
	// Prime the slow host info once, up front, in the background.
	go func() {
		a.refreshHostInfo()
		t := time.NewTicker(hostInterval)
		defer t.Stop()
		for range t.C {
			a.refreshHostInfo()
		}
	}()

	// Fast stats loop. A self-paced sleep (rather than a Ticker) prevents
	// overlapping collections when the daemon is briefly slow.
	go func() {
		for {
			a.refreshStats()
			time.Sleep(interval)
		}
	}()
}

// refreshStats collects per-container stats and recomputes host usage from the
// cached (slowly-refreshed) host facts. Called only by the background collector.
func (a *App) refreshStats() {
	start := time.Now()
	atomic.AddInt64(&a.metrics.collectsTotal, 1)

	// `docker stats` (~2s) and `docker images` (~2.6s) are both slow and both
	// change slowly relative to a page view, so collect them together off-path.
	var (
		stats  map[string]containerStat
		images int
		wg     sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); stats = collectContainerStats() }()
	go func() { defer wg.Done(); images = countImages() }()
	wg.Wait()

	collectErr := ""
	if len(stats) == 0 {
		collectErr = "no stats sampled (daemon may be slow or no running containers)"
		atomic.AddInt64(&a.metrics.collectErrors, 1)
	}

	host, _ := a.cache.hostSnapshot()
	usage := computeHostUsage(stats, host)
	a.cache.store(stats, images, usage, time.Since(start), collectErr, start)
}

// refreshHostInfo runs the three expensive host-level Docker queries
// (`docker info` ×2 and `docker system df`) and caches the result.
func (a *App) refreshHostInfo() {
	h := collectHostInfo()
	a.cache.storeHost(h, time.Now())
	// Recompute usage immediately so the new disk/cpu/mem totals are reflected
	// without waiting for the next stats cycle.
	stats, _, _, _ := a.cache.read()
	usage := computeHostUsage(stats, h)
	a.cache.mu.Lock()
	a.cache.usage = usage
	a.cache.mu.Unlock()
}

// dashboardAPIResponse is the JSON contract polled by the dashboard to hydrate
// live metrics without a full-page reload.
type dashboardAPIResponse struct {
	Total       int                     `json:"total"`
	Running     int                     `json:"running"`
	Stopped     int                     `json:"stopped"`
	Images      int                     `json:"images"`
	Containers  []dashboardAPIContainer `json:"containers"`
	Usage       dashboardAPIUsage       `json:"usage"`
	CollectedAt string                  `json:"collectedAt"`
	CacheAgeMS  int64                   `json:"cacheAgeMs"`
	Now         string                  `json:"now"`
}

type dashboardAPIContainer struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Status  string `json:"status"`
	CPU     string `json:"cpu"`
	Mem     string `json:"mem"`
	MemPerc string `json:"memPerc"`
}

type dashboardAPIUsage struct {
	CPUPercent  float64 `json:"cpuPercent"`
	CPUUsed     string  `json:"cpuUsed"`
	CPUTotal    string  `json:"cpuTotal"`
	MemPercent  float64 `json:"memPercent"`
	MemUsed     string  `json:"memUsed"`
	MemTotal    string  `json:"memTotal"`
	DiskPercent float64 `json:"diskPercent"`
	DiskUsed    string  `json:"diskUsed"`
}

// handleDashboardAPI returns the live container metrics as JSON. The dashboard
// page polls it on an interval so CPU/memory cells and gauges stay current
// without reloading the whole document.
func (a *App) handleDashboardAPI(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	d, err := a.buildDashboardData(search)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	ageMS, _, _, _ := a.cache.snapshot()
	resp := dashboardAPIResponse{
		Total:       d.Total,
		Running:     d.Running,
		Stopped:     d.Stopped,
		Images:      d.Images,
		CollectedAt: d.Now,
		CacheAgeMS:  ageMS,
		Now:         d.Now,
		Usage: dashboardAPIUsage{
			CPUPercent:  d.Usage.CPUPercent,
			CPUUsed:     d.Usage.CPUUsedLabel,
			CPUTotal:    d.Usage.CPUTotalLabel,
			MemPercent:  d.Usage.MemPercent,
			MemUsed:     d.Usage.MemUsedLabel,
			MemTotal:    d.Usage.MemTotalLabel,
			DiskPercent: d.Usage.DiskPercent,
			DiskUsed:    d.Usage.DiskUsedLabel,
		},
	}
	resp.Containers = make([]dashboardAPIContainer, 0, len(d.Containers))
	for _, c := range d.Containers {
		resp.Containers = append(resp.Containers, dashboardAPIContainer{
			ID: c.ID, State: c.State, Status: c.Status,
			CPU: c.CPUPerc, Mem: c.MemUsage, MemPerc: c.MemPerc,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

// handleHealthz is a lightweight liveness/readiness probe. It reports whether
// the stats cache has been primed and how stale it is.
func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ageMS, collectMS, ready, collectErr := a.cache.snapshot()
	status := "ok"
	code := http.StatusOK
	if !ready {
		status = "starting"
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           status,
		"cacheReady":       ready,
		"cacheAgeMs":       ageMS,
		"lastCollectMs":    collectMS,
		"lastCollectError": collectErr,
		"now":              time.Now().Format(time.RFC3339),
	})
}

// handleMetrics exposes process counters in Prometheus text exposition format.
func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	reqs := atomic.LoadInt64(&a.metrics.requestsTotal)
	errs := atomic.LoadInt64(&a.metrics.errorsTotal)
	collects := atomic.LoadInt64(&a.metrics.collectsTotal)
	collectErrs := atomic.LoadInt64(&a.metrics.collectErrors)
	reqNanos := atomic.LoadInt64(&a.metrics.requestNanos)
	ageMS, collectMS, _, _ := a.cache.snapshot()
	uptime := time.Now().Unix() - atomic.LoadInt64(&a.metrics.startedAtUnix)

	avgMS := float64(0)
	if reqs > 0 {
		avgMS = float64(reqNanos) / float64(reqs) / 1e6
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP dockpilot_http_requests_total Total HTTP requests served.\n")
	fmt.Fprintf(w, "# TYPE dockpilot_http_requests_total counter\n")
	fmt.Fprintf(w, "dockpilot_http_requests_total %d\n", reqs)
	fmt.Fprintf(w, "# HELP dockpilot_http_errors_total HTTP responses with status >= 500.\n")
	fmt.Fprintf(w, "# TYPE dockpilot_http_errors_total counter\n")
	fmt.Fprintf(w, "dockpilot_http_errors_total %d\n", errs)
	fmt.Fprintf(w, "# HELP dockpilot_http_request_duration_avg_ms Average handler latency in milliseconds.\n")
	fmt.Fprintf(w, "# TYPE dockpilot_http_request_duration_avg_ms gauge\n")
	fmt.Fprintf(w, "dockpilot_http_request_duration_avg_ms %.3f\n", avgMS)
	fmt.Fprintf(w, "# HELP dockpilot_stats_collections_total Background stats refresh cycles.\n")
	fmt.Fprintf(w, "# TYPE dockpilot_stats_collections_total counter\n")
	fmt.Fprintf(w, "dockpilot_stats_collections_total %d\n", collects)
	fmt.Fprintf(w, "# HELP dockpilot_stats_collection_errors_total Background stats refresh failures.\n")
	fmt.Fprintf(w, "# TYPE dockpilot_stats_collection_errors_total counter\n")
	fmt.Fprintf(w, "dockpilot_stats_collection_errors_total %d\n", collectErrs)
	fmt.Fprintf(w, "# HELP dockpilot_stats_cache_age_ms Age of the warm stats cache in milliseconds.\n")
	fmt.Fprintf(w, "# TYPE dockpilot_stats_cache_age_ms gauge\n")
	fmt.Fprintf(w, "dockpilot_stats_cache_age_ms %d\n", ageMS)
	fmt.Fprintf(w, "# HELP dockpilot_stats_collection_duration_ms Duration of the last stats refresh.\n")
	fmt.Fprintf(w, "# TYPE dockpilot_stats_collection_duration_ms gauge\n")
	fmt.Fprintf(w, "dockpilot_stats_collection_duration_ms %d\n", collectMS)
	fmt.Fprintf(w, "# HELP dockpilot_uptime_seconds Process uptime in seconds.\n")
	fmt.Fprintf(w, "# TYPE dockpilot_uptime_seconds gauge\n")
	fmt.Fprintf(w, "dockpilot_uptime_seconds %d\n", uptime)
}

func interpretDockerCommandWithOllama(userPrompt string) (AISuggestion, error) {
	baseURL := strings.TrimRight(envOrDefault("OLLAMA_BASE_URL", "http://192.168.1.177:11434/v1"), "/")
	apiKey := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	model := envOrDefault("OLLAMA_MODEL", "llama3")

	systemPrompt := `You are DockPilot AI assistant.
Return ONLY JSON object with keys:
- command: docker subcommand WITHOUT the leading word "docker".
- explanation: short reason.

Rules:
- Never include dangerous host commands.
- Keep command focused on Docker CLI only.
- If task is ambiguous, return a safe read-only command like "ps -a" and explain.`

	reqBody := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": 0.2,
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return AISuggestion{}, err
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return AISuggestion{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: 45 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return AISuggestion{}, err
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return AISuggestion{}, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return AISuggestion{}, fmt.Errorf("ollama returned %s", res.Status)
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return AISuggestion{}, fmt.Errorf("invalid ollama response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return AISuggestion{}, fmt.Errorf("empty ollama choices")
	}

	raw := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	suggestion, err := parseAISuggestion(raw)
	if err != nil {
		return AISuggestion{Command: "ps -a", Explanation: raw}, nil
	}

	suggestion.Command = strings.TrimSpace(strings.TrimPrefix(suggestion.Command, "docker"))
	if suggestion.Command == "" {
		suggestion.Command = "ps -a"
	}
	if suggestion.Explanation == "" {
		suggestion.Explanation = "Generated by local Ollama model."
	}
	return suggestion, nil
}

func parseAISuggestion(raw string) (AISuggestion, error) {
	var out AISuggestion
	raw = strings.TrimSpace(raw)
	if err := json.Unmarshal([]byte(raw), &out); err == nil {
		return out, nil
	}

	cleaned := raw
	if idx := strings.Index(cleaned, "```"); idx >= 0 {
		cleaned = cleaned[idx+3:]
		if nl := strings.Index(cleaned, "\n"); nl >= 0 {
			cleaned = cleaned[nl+1:]
		}
		if end := strings.Index(cleaned, "```"); end >= 0 {
			cleaned = cleaned[:end]
		}
		cleaned = strings.TrimSpace(cleaned)
		if err := json.Unmarshal([]byte(cleaned), &out); err == nil {
			return out, nil
		}
	}

	if start := strings.Index(raw, "{"); start >= 0 {
		if end := strings.LastIndex(raw, "}"); end > start {
			fragment := raw[start : end+1]
			if err := json.Unmarshal([]byte(fragment), &out); err == nil {
				return out, nil
			}
		}
	}

	return AISuggestion{}, fmt.Errorf("no valid AI JSON found")
}

// analyzeWithOllama sends a system prompt plus arbitrary context to the local
// Ollama (OpenAI-compatible) endpoint and returns the model's free-text reply.
// It shares the same env-var configuration as interpretDockerCommandWithOllama.
func analyzeWithOllama(systemPrompt, userContent string) (string, error) {
	baseURL := strings.TrimRight(envOrDefault("OLLAMA_BASE_URL", "http://192.168.1.177:11434/v1"), "/")
	apiKey := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	model := envOrDefault("OLLAMA_MODEL", "llama3")

	reqBody := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userContent},
		},
		"temperature": 0.3,
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: 90 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("ollama returned %s", res.Status)
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", fmt.Errorf("invalid ollama response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("empty ollama choices")
	}

	return strings.TrimSpace(chatResp.Choices[0].Message.Content), nil
}

func (a *App) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, fmt.Sprintf("invalid form: %v", err))
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	image := strings.TrimSpace(r.FormValue("image"))
	ports := splitComma(strings.TrimSpace(r.FormValue("ports")))
	envs := splitComma(strings.TrimSpace(r.FormValue("env")))
	cmd := splitComma(strings.TrimSpace(r.FormValue("command")))
	autoStart := r.FormValue("auto_start") == "on"

	if image == "" {
		redirectError(w, r, "image is required")
		return
	}

	args := []string{"run"}
	if autoStart {
		args = append(args, "-d")
	}
	if name != "" {
		args = append(args, "--name", name)
	}
	for _, p := range ports {
		args = append(args, "-p", p)
	}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, image)
	args = append(args, cmd...)

	if _, err := runDocker(args...); err != nil {
		redirectError(w, r, fmt.Sprintf("create failed: %v", err))
		return
	}

	http.Redirect(w, r, "/?success=container+created", http.StatusSeeOther)
}

func (a *App) handleContainerAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, fmt.Sprintf("invalid form: %v", err))
		return
	}

	search := strings.TrimSpace(r.FormValue("q"))

	path := strings.TrimPrefix(r.URL.Path, "/containers/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]

	var args []string
	switch action {
	case "start", "stop", "restart", "remove":
		args = []string{action, id}
	case "remove-force":
		args = []string{"rm", "-f", id}
	case "inspect":
		out, cmdErr := runDockerCombined("inspect", id)
		d, err := a.buildDashboardData(search)
		if err != nil {
			a.render(w, PageData{Error: err.Error()})
			return
		}

		d.CommandInput = "inspect " + id
		if strings.TrimSpace(out) == "" {
			d.CommandOutput = "(no output)"
		} else {
			d.CommandOutput = out
		}
		if cmdErr != nil {
			d.Error = fmt.Sprintf("inspect failed: %v", cmdErr)
		} else {
			d.Success = "container inspect output loaded"
		}
		a.render(w, d)
		return
	case "logs":
		out, cmdErr := runDockerCombined("logs", "--tail", "200", id)
		d, err := a.buildDashboardData(search)
		if err != nil {
			a.render(w, PageData{Error: err.Error()})
			return
		}

		d.CommandInput = "logs --tail 200 " + id
		if strings.TrimSpace(out) == "" {
			d.CommandOutput = "(no output)"
		} else {
			d.CommandOutput = out
		}
		if cmdErr != nil {
			d.Error = fmt.Sprintf("logs failed: %v", cmdErr)
		} else {
			d.Success = "container logs loaded (last 200 lines)"
		}
		a.render(w, d)
		return
	case "analyze":
		d, err := a.buildDashboardData(search)
		if err != nil {
			a.render(w, PageData{Error: err.Error()})
			return
		}
		d.CommandInput = "AI analyze " + id
		analysis, aiErr := analyzeContainerWithOllama(id)
		if aiErr != nil {
			d.CommandOutput = "(analysis unavailable)"
			d.Error = fmt.Sprintf("AI analysis failed: %v", aiErr)
		} else {
			d.CommandOutput = analysis
			d.Success = "AI container analysis ready"
			if steps := extractRunbookStepsFromText(analysis); len(steps) > 0 {
				d.AISteps = steps
				if enc, e := json.Marshal(steps); e == nil {
					d.AIStepsEncoded = string(enc)
				}
				d.AIStepsTitle = "AI remediation: " + containerNameByID(d.Containers, id)
			}
		}
		a.render(w, d)
		return
	default:
		http.NotFound(w, r)
		return
	}

	if action == "remove" {
		args = []string{"rm", "-f", id}
	}

	if _, err := runDocker(args...); err != nil {
		redirectError(w, r, fmt.Sprintf("%s failed: %v", action, err))
		return
	}

	redirectPath := "/?success=container+" + action + "+ok"
	if search != "" {
		redirectPath += "&q=" + urlQueryEscape(search)
	}
	http.Redirect(w, r, redirectPath, http.StatusSeeOther)
}

// containerNameByID returns a friendly container name for an id (matching by id
// prefix), falling back to the short id when not found.
func containerNameByID(containers []ContainerView, id string) string {
	for _, c := range containers {
		if c.ID == id || strings.HasPrefix(c.ID, id) || strings.HasPrefix(id, c.ID) {
			if c.Name != "" {
				return c.Name
			}
		}
	}
	return shortID(id)
}

// handleAIRunbookRun executes the AI-suggested steps ad-hoc (without saving) and
// shows the combined output in the dashboard output panel.
func (a *App) handleAIRunbookRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, fmt.Sprintf("invalid form: %v", err))
		return
	}
	search := strings.TrimSpace(r.FormValue("q"))

	var steps []RunbookStep
	if err := json.Unmarshal([]byte(r.FormValue("steps")), &steps); err != nil {
		redirectError(w, r, "invalid steps payload")
		return
	}
	steps = sanitizeRunbookSteps(steps)

	d, err := a.buildDashboardData(search)
	if err != nil {
		a.render(w, PageData{Error: err.Error()})
		return
	}
	d.CommandInput = "AI runbook (ad-hoc run)"

	if len(steps) == 0 {
		d.Error = "no runnable steps found in the analysis"
		a.render(w, d)
		return
	}

	var sb strings.Builder
	failed := 0
	for i, s := range steps {
		out, runErr := executeAIStep(s.Command)
		fmt.Fprintf(&sb, "[%d/%d] $ docker %s\n", i+1, len(steps), s.Command)
		if runErr != nil {
			failed++
			fmt.Fprintf(&sb, "ERROR: %s\n", runErr.Error())
		}
		if strings.TrimSpace(out) != "" {
			sb.WriteString(out)
			sb.WriteString("\n")
		} else if runErr == nil {
			sb.WriteString("(no output)\n")
		}
		sb.WriteString("\n")
	}
	d.CommandOutput = strings.TrimSpace(sb.String())
	if failed == 0 {
		d.Success = fmt.Sprintf("Ran %d AI step(s) successfully", len(steps))
	} else {
		d.Error = fmt.Sprintf("%d of %d step(s) failed", failed, len(steps))
	}
	a.render(w, d)
}

// handleAIRunbookSave persists the AI-suggested steps as a new user runbook so it
// appears permanently in the Runbooks page.
func (a *App) handleAIRunbookSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, fmt.Sprintf("invalid form: %v", err))
		return
	}

	var steps []RunbookStep
	if err := json.Unmarshal([]byte(r.FormValue("steps")), &steps); err != nil {
		redirectError(w, r, "invalid steps payload")
		return
	}
	steps = sanitizeRunbookSteps(steps)
	if len(steps) == 0 {
		redirectError(w, r, "no valid steps to save")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "AI Runbook"
	}

	rb := Runbook{
		ID:          fmt.Sprintf("ai-%d", time.Now().UnixNano()),
		Name:        name,
		Description: "Saved from AI container analysis.",
		Category:    "AI",
		Risk:        "medium",
		Steps:       steps,
		UserCreated: true,
	}
	if err := addUserRunbook(rb); err != nil {
		redirectError(w, r, fmt.Sprintf("failed to save runbook: %v", err))
		return
	}
	http.Redirect(w, r, "/runbooks?id="+rb.ID, http.StatusSeeOther)
}

func listContainers() ([]ContainerView, error) {
	out, err := runDocker("ps", "-a", "--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.Status}}|{{.State}}|{{.Ports}}|{{.RunningFor}}")
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	if strings.TrimSpace(out) == "" {
		return []ContainerView{}, nil
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	items := make([]ContainerView, 0, len(lines))
	for _, l := range lines {
		p := strings.Split(l, "|")
		if len(p) < 7 {
			continue
		}
		items = append(items, ContainerView{
			ID:      shortID(p[0]),
			Name:    p[1],
			Image:   p[2],
			Status:  p[3],
			State:   p[4],
			Ports:   emptyDash(p[5]),
			Created: p[6],
		})
	}
	return items, nil
}

func filterContainers(items []ContainerView, query string) []ContainerView {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return items
	}

	filtered := make([]ContainerView, 0, len(items))
	for _, c := range items {
		if strings.Contains(strings.ToLower(c.Name), q) ||
			strings.Contains(strings.ToLower(c.ID), q) ||
			strings.Contains(strings.ToLower(c.Image), q) ||
			strings.Contains(strings.ToLower(c.Status), q) ||
			strings.Contains(strings.ToLower(c.State), q) ||
			strings.Contains(strings.ToLower(c.Ports), q) {
			filtered = append(filtered, c)
		}
	}

	return filtered
}

func countImages() int {
	out, err := runDocker("images", "-q")
	if err != nil {
		return 0
	}
	if strings.TrimSpace(out) == "" {
		return 0
	}
	return len(splitLines(out))
}

type containerStat struct {
	cpuPerc  string
	memUsage string
	memPerc  string
	memBytes int64
	cpuFloat float64
}

func collectContainerStats() map[string]containerStat {
	out, err := runDocker("stats", "--no-stream", "--format", "{{.ID}}|{{.CPUPerc}}|{{.MemUsage}}|{{.MemPerc}}")
	if err != nil || strings.TrimSpace(out) == "" {
		return map[string]containerStat{}
	}

	stats := make(map[string]containerStat)
	for _, line := range splitLines(out) {
		p := strings.Split(line, "|")
		if len(p) < 4 {
			continue
		}
		id := shortID(strings.TrimSpace(p[0]))
		cpuStr := strings.TrimSpace(p[1])
		memUsage := strings.TrimSpace(p[2])
		memPerc := strings.TrimSpace(p[3])

		cpuFloat := parsePercent(cpuStr)
		memBytes := parseMemUsageBytes(memUsage)
		stats[id] = containerStat{
			cpuPerc:  cpuStr,
			memUsage: memUsage,
			memPerc:  memPerc,
			memBytes: memBytes,
			cpuFloat: cpuFloat,
		}
	}
	return stats
}

func parsePercent(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(s, "%"))
	if s == "" || s == "--" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func parseMemUsageBytes(s string) int64 {
	// docker stats prints "used / limit" — take the left side
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	return parseSizeBytes(strings.TrimSpace(s))
}

var sizeRe = regexp.MustCompile(`(?i)^([0-9.]+)\s*([KMGT]?i?B)?$`)

func parseSizeBytes(s string) int64 {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToUpper(m[2])
	mult := float64(1)
	switch unit {
	case "", "B":
		mult = 1
	case "KB":
		mult = 1e3
	case "KIB":
		mult = 1 << 10
	case "MB":
		mult = 1e6
	case "MIB":
		mult = 1 << 20
	case "GB":
		mult = 1e9
	case "GIB":
		mult = 1 << 30
	case "TB":
		mult = 1e12
	case "TIB":
		mult = 1 << 40
	}
	return int64(v * mult)
}

func humanBytes(b int64) string {
	if b <= 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), units[exp])
}

func dockerInfoTotalMem() int64 {
	out, err := runDocker("info", "--format", "{{.MemTotal}}")
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func dockerInfoCPUs() int {
	out, err := runDocker("info", "--format", "{{.NCPU}}")
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return v
}

func dockerSystemDFSize() (used, total int64) {
	out, err := runDocker("system", "df", "--format", "{{.Type}}|{{.Size}}|{{.Reclaimable}}")
	if err != nil || strings.TrimSpace(out) == "" {
		return 0, 0
	}
	for _, line := range splitLines(out) {
		p := strings.Split(line, "|")
		if len(p) < 3 {
			continue
		}
		sz := parseSizeBytes(strings.TrimSpace(p[1]))
		used += sz
	}
	// docker doesn't expose the host disk cap via this command. Use the sum
	// itself as the upper bound for display purposes; callers can render a
	// fullness meter by dividing active (non-reclaimable) / total.
	total = used
	return used, total
}

// collectHostInfo runs the three expensive host-level Docker queries in
// parallel. Intended to be called only by the background collector.
func collectHostInfo() hostInfo {
	var (
		h  hostInfo
		wg sync.WaitGroup
	)
	wg.Add(3)
	go func() { defer wg.Done(); h.cpuCores = dockerInfoCPUs() }()
	go func() { defer wg.Done(); h.memTotal = dockerInfoTotalMem() }()
	go func() { defer wg.Done(); h.diskUsed, h.diskTotal = dockerSystemDFSize() }()
	wg.Wait()
	return h
}

// computeHostUsage derives the displayable host usage from the live per-container
// stats and the cached host facts. It performs no Docker calls and is safe to run
// on every refresh cycle.
func computeHostUsage(stats map[string]containerStat, h hostInfo) HostUsage {
	var (
		cpuSum float64
		memSum int64
	)
	for _, s := range stats {
		cpuSum += s.cpuFloat
		memSum += s.memBytes
	}

	cpus := h.cpuCores
	memTotal := h.memTotal
	diskUsed, diskTotal := h.diskUsed, h.diskTotal

	cpuPercent := float64(0)
	if cpus > 0 {
		cpuPercent = cpuSum / float64(cpus)
		if cpuPercent > 100 {
			cpuPercent = 100
		}
	}

	memPercent := float64(0)
	if memTotal > 0 {
		memPercent = float64(memSum) / float64(memTotal) * 100
		if memPercent > 100 {
			memPercent = 100
		}
	}

	diskPercent := float64(0)
	if diskTotal > 0 {
		diskPercent = float64(diskUsed) / float64(diskTotal) * 100
	}

	return HostUsage{
		CPUPercent:     cpuPercent,
		CPUCores:       cpus,
		MemUsedBytes:   memSum,
		MemTotalBytes:  memTotal,
		MemPercent:     memPercent,
		DiskUsedBytes:  diskUsed,
		DiskTotalBytes: diskTotal,
		DiskPercent:    diskPercent,
		CPUUsedLabel:   fmt.Sprintf("%.1f%%", cpuSum),
		CPUTotalLabel:  fmt.Sprintf("%d cores", cpus),
		MemUsedLabel:   humanBytes(memSum),
		MemTotalLabel:  humanBytes(memTotal),
		DiskUsedLabel:  humanBytes(diskUsed),
		DiskTotalLabel: humanBytes(diskTotal),
	}
}

// dockerCmdTimeout bounds every read-only Docker CLI call so a wedged daemon
// can never hang a page request or the background collector forever.
const dockerCmdTimeout = 25 * time.Second

func runDocker(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("docker %s timed out after %s", strings.Join(args, " "), dockerCmdTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf(msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func runDockerCombined(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf(text)
	}
	return text, nil
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func splitLines(s string) []string {
	parts := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

// envDurationOrDefault parses a duration env var (e.g. "3s", "500ms"). A bare
// integer is treated as seconds. Falls back on empty or invalid input.
func envDurationOrDefault(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return fallback
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func emptyDash(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "-"
	}
	return v
}

func redirectError(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?error="+urlQueryEscape(msg), http.StatusSeeOther)
}

func urlQueryEscape(v string) string {
	r := strings.NewReplacer(
		" ", "+",
		"&", "%26",
		"?", "%3F",
		"=", "%3D",
		"#", "%23",
	)
	return r.Replace(v)
}

func (a *App) render(w http.ResponseWriter, d PageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.Execute(w, d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type ipamDataWithSearch struct {
	IPAMData
	Search string
}

// buildIPAMData gathers Docker networks and port mappings, applying the given
// search filter. It is shared by the IPAM page and the IPAM AI analysis handler.
func buildIPAMData(search string) IPAMData {
	data := IPAMData{
		Now:        time.Now().Format("2006-01-02 15:04:05"),
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		AIModel:    envOrDefault("OLLAMA_MODEL", "llama3"),
	}

	networks, netErr := listDockerNetworks()
	if netErr == nil {
		data.Networks = networks
	}

	mappings, err := listPortMappings()
	if err != nil {
		data.Error = fmt.Sprintf("failed to list port mappings: %v", err)
		return data
	}

	// Enrich port mappings with internal IP and network from network data
	if len(networks) > 0 {
		cMap := buildContainerNetworkMap(networks)
		for i, m := range mappings {
			if info, ok := cMap[m.ContainerName]; ok {
				mappings[i].InternalIP = info.IP
				mappings[i].Network = info.Network
			}
		}
	}
	if search != "" {
		data.PortMappings = filterPortMappings(mappings, search)
	} else {
		data.PortMappings = mappings
	}
	data.TotalPorts = len(data.PortMappings)
	data.UsedRanges = buildPortRanges(data.PortMappings)
	return data
}

func (a *App) renderIPAM(w http.ResponseWriter, data IPAMData, search string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.ipamTmpl.Execute(w, ipamDataWithSearch{data, search}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) handleIPAM(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ipam" {
		http.NotFound(w, r)
		return
	}

	search := strings.TrimSpace(r.URL.Query().Get("q"))
	a.renderIPAM(w, buildIPAMData(search), search)
}

func (a *App) handleIPAMAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	search := strings.TrimSpace(r.FormValue("q"))
	data := buildIPAMData(search)

	analysis, aiErr := analyzeIPAMWithOllama(data)
	if aiErr != nil {
		data.Error = fmt.Sprintf("AI analysis failed: %v", aiErr)
	} else {
		data.AIAnalysis = analysis
		data.Success = "AI network analysis ready"
	}
	a.renderIPAM(w, data, search)
}

// analyzeIPAMWithOllama asks the local model to review port allocations and
// Docker networks for conflicts, exposure risks, and connectivity issues.
func analyzeIPAMWithOllama(data IPAMData) (string, error) {
	systemPrompt := `You are DockPilot AI, an expert in Docker networking and host security.
You are given a host's published Docker port mappings and its Docker networks.
Respond in concise plain text (no JSON, no markdown headers) covering:
1. Exposure risks: ports bound to 0.0.0.0 / all interfaces, or sensitive services (databases, admin panels) reachable from the host's external interface.
2. Conflicts or anomalies: duplicate host ports, overlapping subnets, containers on unexpected networks.
3. Connectivity observations: containers with no published ports, isolated networks, missing gateways.
4. Concrete next steps as Docker CLI commands where applicable.
If the setup looks healthy and well-isolated, say so clearly. Be specific and actionable.`

	var sb strings.Builder
	fmt.Fprintf(&sb, "Published port mappings (%d):\n", data.TotalPorts)
	if len(data.PortMappings) == 0 {
		sb.WriteString("(none)\n")
	}
	for _, p := range data.PortMappings {
		fmt.Fprintf(&sb, "- host %s:%d -> container %s:%d/%s | container=%s state=%s network=%s internalIP=%s\n",
			p.HostIP, p.HostPort, p.InternalIP, p.ContainerPort, p.Protocol, p.ContainerName, p.State, p.Network, p.InternalIP)
	}

	fmt.Fprintf(&sb, "\nDocker networks (%d):\n", len(data.Networks))
	for _, n := range data.Networks {
		fmt.Fprintf(&sb, "- %s | driver=%s scope=%s subnet=%s gateway=%s containers=%d\n",
			n.Name, n.Driver, n.Scope, n.Subnet, n.Gateway, len(n.Containers))
		for _, c := range n.Containers {
			fmt.Fprintf(&sb, "    * %s (%s)\n", c.Name, c.InternalIP)
		}
	}

	return analyzeWithOllama(systemPrompt, sb.String())
}

func filterPortMappings(mappings []PortMapping, query string) []PortMapping {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return mappings
	}
	var filtered []PortMapping
	for _, m := range mappings {
		if strings.Contains(strconv.Itoa(m.HostPort), q) ||
			strings.Contains(strconv.Itoa(m.ContainerPort), q) ||
			strings.Contains(strings.ToLower(m.HostIP), q) ||
			strings.Contains(strings.ToLower(m.ContainerName), q) ||
			strings.Contains(strings.ToLower(m.ContainerID), q) ||
			strings.Contains(strings.ToLower(m.State), q) ||
			strings.Contains(strings.ToLower(m.InternalIP), q) ||
			strings.Contains(strings.ToLower(m.Network), q) {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

func listPortMappings() ([]PortMapping, error) {
	out, err := runDocker("ps", "-a", "--format", "{{.ID}}|{{.Names}}|{{.Ports}}|{{.State}}")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return []PortMapping{}, nil
	}

	portRe := regexp.MustCompile(`([\d.]+|::)?:(\d+)->(\d+)/(tcp|udp)`)
	var mappings []PortMapping

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		cID := shortID(parts[0])
		cName := parts[1]
		portsStr := parts[2]
		state := parts[3]

		if strings.TrimSpace(portsStr) == "" {
			continue
		}

		matches := portRe.FindAllStringSubmatch(portsStr, -1)
		for _, m := range matches {
			hostIP := m[1]
			if hostIP == "" || hostIP == "::" {
				hostIP = "0.0.0.0"
			}
			hostPort, _ := strconv.Atoi(m[2])
			containerPort, _ := strconv.Atoi(m[3])
			protocol := m[4]

			mappings = append(mappings, PortMapping{
				HostIP:        hostIP,
				HostPort:      hostPort,
				ContainerPort: containerPort,
				Protocol:      protocol,
				ContainerName: cName,
				ContainerID:   cID,
				State:         state,
			})
		}
	}

	sort.Slice(mappings, func(i, j int) bool {
		return mappings[i].HostPort < mappings[j].HostPort
	})

	return mappings, nil
}

func buildPortRanges(mappings []PortMapping) []PortRange {
	if len(mappings) == 0 {
		return nil
	}

	seen := make(map[int]bool)
	var ports []int
	for _, m := range mappings {
		if !seen[m.HostPort] {
			seen[m.HostPort] = true
			ports = append(ports, m.HostPort)
		}
	}
	sort.Ints(ports)

	var ranges []PortRange
	start := ports[0]
	end := ports[0]

	for i := 1; i < len(ports); i++ {
		if ports[i] == end+1 {
			end = ports[i]
		} else {
			r := PortRange{Start: start, End: end}
			if start == end {
				r.Display = strconv.Itoa(start)
			} else {
				r.Display = fmt.Sprintf("%d-%d", start, end)
			}
			ranges = append(ranges, r)
			start = ports[i]
			end = ports[i]
		}
	}
	r := PortRange{Start: start, End: end}
	if start == end {
		r.Display = strconv.Itoa(start)
	} else {
		r.Display = fmt.Sprintf("%d-%d", start, end)
	}
	ranges = append(ranges, r)

	return ranges
}

func listDockerNetworks() ([]DockerNetwork, error) {
	out, err := runDocker("network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}

	var networks []DockerNetwork
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		inspectOut, err := runDocker("network", "inspect", name, "--format",
			"{{.Driver}}|{{.Scope}}|{{range .IPAM.Config}}{{.Subnet}}|{{.Gateway}}{{end}}")
		if err != nil {
			continue
		}
		parts := strings.SplitN(strings.TrimSpace(inspectOut), "|", 4)
		net := DockerNetwork{Name: name}
		if len(parts) >= 1 {
			net.Driver = parts[0]
		}
		if len(parts) >= 2 {
			net.Scope = parts[1]
		}
		if len(parts) >= 3 {
			net.Subnet = parts[2]
		}
		if len(parts) >= 4 {
			net.Gateway = parts[3]
		}

		// Get containers connected to this network
		cOut, err := runDocker("network", "inspect", name, "--format",
			"{{range $k, $v := .Containers}}{{$v.Name}}|{{$k}}|{{$v.IPv4Address}}\n{{end}}")
		if err == nil {
			for _, cLine := range strings.Split(strings.TrimSpace(cOut), "\n") {
				cLine = strings.TrimSpace(cLine)
				if cLine == "" {
					continue
				}
				cParts := strings.SplitN(cLine, "|", 3)
				if len(cParts) < 3 {
					continue
				}
				ip := cParts[2]
				if idx := strings.Index(ip, "/"); idx > 0 {
					ip = ip[:idx]
				}
				networks = append(networks[:0:0], networks...) // no-op, just for clarity
				net.Containers = append(net.Containers, NetworkContainer{
					Name:       cParts[0],
					ID:         shortID(cParts[1]),
					InternalIP: ip,
				})
			}
		}
		networks = append(networks, net)
	}

	sort.Slice(networks, func(i, j int) bool {
		return networks[i].Name < networks[j].Name
	})
	return networks, nil
}

// buildContainerNetworkMap builds a map of containerName -> "ip (network)" for enriching port mappings
func buildContainerNetworkMap(networks []DockerNetwork) map[string]struct{ IP, Network string } {
	m := make(map[string]struct{ IP, Network string })
	for _, n := range networks {
		for _, c := range n.Containers {
			if c.InternalIP != "" {
				// Prefer bridge/overlay networks over host
				if existing, ok := m[c.Name]; ok {
					if n.Driver == "host" || (existing.Network != "" && n.Driver != "bridge") {
						continue
					}
				}
				m[c.Name] = struct{ IP, Network string }{c.InternalIP, n.Name}
			}
		}
	}
	return m
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>DockPilot — Docker Cockpit</title>
  <meta name="theme-color" content="#0a0f1c" />
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800;900&family=JetBrains+Mono:wght@400;600&display=swap" rel="stylesheet">
  <style>
    :root {
      --bg:#0a0f1c;
      --surface:#111827;
      --surface-2:#1f2937;
      --surface-3:#283548;
      --border:rgba(255,255,255,0.08);
      --border-hover:rgba(255,255,255,0.16);
      --text:#f1f5f9;
      --text-secondary:#cbd5e1;
      --muted:#94a3b8;
      --accent:#3b82f6;
      --accent-light:#60a5fa;
      --accent-glow:rgba(59,130,246,0.15);
      --success:#22c55e;
      --success-bg:rgba(34,197,94,0.1);
      --success-border:rgba(34,197,94,0.25);
      --warning:#fbbf24;
      --warning-bg:rgba(251,191,36,0.1);
      --danger:#ef4444;
      --danger-bg:rgba(239,68,68,0.1);
      --radius:16px;
      --radius-sm:10px;
      --mono:'JetBrains Mono', ui-monospace, "SF Mono", Menlo, monospace;
    }
    *, *::before, *::after { box-sizing:border-box; margin:0; padding:0; }
    html { scroll-behavior:smooth; }
    body {
      font-family:'Inter', -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      color:var(--text);
      background:radial-gradient(1100px 500px at 88% -120px, var(--accent-glow) 0%, transparent 60%), var(--bg);
      line-height:1.6;
      -webkit-font-smoothing:antialiased;
      min-height:100vh;
    }
    a { color:inherit; text-decoration:none; }
    .wrap { width:100%; max-width:none; margin:0; padding:0 28px; }

    /* ── Nav ── */
    .nav {
      position:sticky; top:0; z-index:50;
      background:rgba(10,15,28,0.85);
      backdrop-filter:blur(16px); -webkit-backdrop-filter:blur(16px);
      border-bottom:1px solid var(--border);
    }
    .nav-inner { display:flex; align-items:center; gap:20px; height:64px; }
    .brand { display:flex; align-items:center; gap:11px; }
    .brand .logo-badge {
      width:34px; height:34px; border-radius:9px;
      background:linear-gradient(135deg, var(--accent) 0%, #6366f1 100%);
      display:flex; align-items:center; justify-content:center;
      box-shadow:0 4px 18px var(--accent-glow);
    }
    .brand .logo-badge svg { width:19px; height:19px; stroke:#fff; fill:none; stroke-width:2; }
    .brand .name { font-weight:900; font-size:1.2rem; letter-spacing:-0.02em; }
    .brand .name span { color:var(--accent-light); }
    .brand .tag {
      font-size:11px; color:var(--muted); border:1px solid var(--border);
      border-radius:6px; padding:2px 8px; font-weight:600; white-space:nowrap;
    }
    .nav-links { display:flex; align-items:center; gap:4px; }
    .nav-links a {
      font-size:0.92rem; font-weight:600; color:var(--muted);
      padding:8px 14px; border-radius:var(--radius-sm); transition:all .15s;
      display:inline-flex; align-items:center; gap:7px;
    }
    .nav-links a:hover { color:var(--text); background:rgba(255,255,255,0.06); }
    .nav-links a.active { color:var(--text); background:var(--accent-glow); }
    .nav-links a svg { width:16px; height:16px; stroke:currentColor; fill:none; stroke-width:2; stroke-linecap:round; stroke-linejoin:round; }
    .nav-spacer { flex:1; }
    .socket-badge {
      display:inline-flex; align-items:center; gap:8px;
      font-size:0.8rem; color:var(--text-secondary); font-weight:500;
      background:var(--success-bg); border:1px solid var(--success-border);
      padding:7px 14px; border-radius:999px; white-space:nowrap;
    }
    .socket-badge .dot { width:8px; height:8px; border-radius:50%; background:var(--success); box-shadow:0 0 0 3px rgba(34,197,94,0.18); animation:pulse 2s infinite; }
    @keyframes pulse { 0%,100%{opacity:1;} 50%{opacity:.4;} }
    .socket-badge .mono { font-family:var(--mono); color:var(--muted); font-size:0.74rem; }

    /* ── Page ── */
    main { padding:28px 0 64px; }
    .page-head { margin-bottom:22px; }
    .page-head h1 { font-size:1.5rem; font-weight:800; letter-spacing:-0.02em; }
    .page-head p { color:var(--muted); font-size:0.95rem; margin-top:3px; }

    .msg { padding:13px 16px; border-radius:var(--radius-sm); margin-bottom:18px; font-size:0.9rem; display:flex; align-items:center; gap:10px; }
    .msg::before { font-size:1rem; }
    .ok { background:var(--success-bg); border:1px solid var(--success-border); color:#86efac; }
    .ok::before { content:'✓'; }
    .err { background:var(--danger-bg); border:1px solid rgba(239,68,68,0.3); color:#fca5a5; }
    .err::before { content:'!'; font-weight:900; }

    /* ── Section ── */
    .section { margin-bottom:26px; }
    .section-title {
      display:flex; align-items:center; gap:10px; margin-bottom:14px;
      font-size:0.78rem; font-weight:700; text-transform:uppercase; letter-spacing:0.08em; color:var(--muted);
    }
    .section-title svg { width:15px; height:15px; stroke:var(--accent-light); fill:none; stroke-width:2; stroke-linecap:round; stroke-linejoin:round; }

    /* ── Overview grid ── */
    .overview { display:grid; grid-template-columns:repeat(4, 1fr); gap:14px; margin-bottom:14px; }
    .kpi {
      background:var(--surface); border:1px solid var(--border); border-radius:var(--radius);
      padding:18px 20px; transition:border-color .2s, transform .2s; position:relative; overflow:hidden;
    }
    .kpi:hover { border-color:var(--border-hover); transform:translateY(-2px); }
    .kpi .label { color:var(--muted); font-size:0.82rem; font-weight:600; display:flex; align-items:center; gap:8px; }
    .kpi .label svg { width:16px; height:16px; stroke:currentColor; fill:none; stroke-width:2; stroke-linecap:round; stroke-linejoin:round; }
    .kpi .value { font-size:2rem; font-weight:800; margin-top:8px; letter-spacing:-0.02em; }
    .kpi.accent .label svg { stroke:var(--accent-light); }
    .kpi.green .label svg { stroke:var(--success); }
    .kpi.green .value { color:var(--success); }
    .kpi.warn .label svg { stroke:var(--warning); }

    .usage-row { display:grid; grid-template-columns:repeat(3, 1fr); gap:14px; }
    .usage-card {
      background:var(--surface); border:1px solid var(--border); border-radius:var(--radius);
      padding:18px 20px; display:flex; gap:18px; align-items:center; transition:border-color .2s;
    }
    .usage-card:hover { border-color:var(--border-hover); }
    .usage-donut { position:relative; width:80px; height:80px; flex-shrink:0; }
    .usage-donut svg { transform:rotate(-90deg); width:100%; height:100%; }
    .usage-donut .track { fill:none; stroke:var(--surface-2); stroke-width:3.2; }
    .usage-donut .arc { fill:none; stroke-width:3.2; stroke-linecap:round; transition:stroke-dasharray .5s ease; }
    .usage-donut .arc-cpu { stroke:var(--accent); }
    .usage-donut .arc-mem { stroke:var(--success); }
    .usage-donut .arc-disk { stroke:var(--warning); }
    .usage-donut .pct { position:absolute; inset:0; display:flex; align-items:center; justify-content:center; font-size:1.05rem; font-weight:800; }
    .usage-meta { display:flex; flex-direction:column; gap:2px; min-width:0; }
    .usage-meta .title { font-size:0.72rem; color:var(--muted); text-transform:uppercase; letter-spacing:0.06em; font-weight:700; }
    .usage-meta .used { font-size:1.25rem; font-weight:800; letter-spacing:-0.01em; }
    .usage-meta .total { font-size:0.75rem; color:var(--muted); }

    /* ── Card / panel ── */
    .card { background:var(--surface); border:1px solid var(--border); border-radius:var(--radius); }
    .card-head {
      display:flex; align-items:center; gap:14px; flex-wrap:wrap;
      padding:18px 20px; border-bottom:1px solid var(--border);
    }
    .card-head h3 { font-size:1.05rem; font-weight:700; display:flex; align-items:center; gap:9px; }
    .card-head h3 svg { width:18px; height:18px; stroke:var(--accent-light); fill:none; stroke-width:2; stroke-linecap:round; stroke-linejoin:round; }
    .count-pill { font-size:0.78rem; font-weight:700; color:var(--accent-light); background:var(--accent-glow); border-radius:999px; padding:3px 11px; }
    .card-body { padding:20px; }

    /* ── Search ── */
    .search-wrap { position:relative; margin-left:auto; min-width:260px; flex:1; max-width:440px; }
    .search-wrap svg { position:absolute; left:14px; top:50%; transform:translateY(-50%); width:17px; height:17px; stroke:var(--muted); fill:none; stroke-width:2; stroke-linecap:round; stroke-linejoin:round; pointer-events:none; }
    .search-wrap input {
      width:100%; padding:11px 14px 11px 42px; border-radius:var(--radius-sm);
      border:1px solid var(--border); background:var(--bg); color:var(--text); font-size:0.9rem; font-family:inherit;
      transition:border-color .15s, box-shadow .15s;
    }
    .search-wrap input:focus { outline:none; border-color:var(--accent); box-shadow:0 0 0 3px var(--accent-glow); }
    .filter-hint { font-size:0.74rem; color:var(--muted); white-space:nowrap; }
    .live-pill { display:inline-flex; align-items:center; gap:6px; font-size:0.7rem; font-weight:600; color:var(--muted); background:rgba(16,185,129,0.08); border:1px solid rgba(16,185,129,0.18); border-radius:999px; padding:3px 9px; white-space:nowrap; }
    .live-pill.stale { color:#fbbf24; background:rgba(245,158,11,0.08); border-color:rgba(245,158,11,0.25); }
    .live-dot { width:7px; height:7px; border-radius:50%; background:#34d399; box-shadow:0 0 0 0 rgba(52,211,153,0.6); animation:livePulse 2s infinite; }
    .live-pill.stale .live-dot { background:#fbbf24; animation:none; }
    @keyframes livePulse { 0%{box-shadow:0 0 0 0 rgba(52,211,153,0.5);} 70%{box-shadow:0 0 0 6px rgba(52,211,153,0);} 100%{box-shadow:0 0 0 0 rgba(52,211,153,0);} }
    @media (prefers-reduced-motion: reduce) { .live-dot { animation:none; } }

    /* ── Inputs / buttons ── */
    input, textarea, button { font-family:inherit; font-size:0.9rem; }
    .fld {
      border-radius:var(--radius-sm); border:1px solid var(--border);
      background:var(--bg); color:var(--text); padding:11px 13px; transition:border-color .15s, box-shadow .15s;
    }
    .fld:focus { outline:none; border-color:var(--accent); box-shadow:0 0 0 3px var(--accent-glow); }
    textarea.fld { width:100%; min-height:74px; resize:vertical; }
    .btn {
      display:inline-flex; align-items:center; justify-content:center; gap:7px;
      font-weight:600; padding:11px 18px; border-radius:var(--radius-sm);
      border:1px solid var(--border); color:var(--text); background:var(--surface-2);
      cursor:pointer; transition:all .15s; white-space:nowrap;
    }
    .btn:hover { border-color:var(--border-hover); background:var(--surface-3); }
    .btn-primary { background:var(--accent); border-color:var(--accent); color:#fff; box-shadow:0 4px 18px var(--accent-glow); }
    .btn-primary:hover { background:#2563eb; border-color:#2563eb; }
    .row { display:flex; gap:10px; flex-wrap:wrap; }

    /* ── Tabs (tools) ── */
    .tabs { display:flex; gap:6px; padding:14px 20px 0; flex-wrap:wrap; }
    .tab {
      font-size:0.88rem; font-weight:600; padding:9px 16px; border-radius:var(--radius-sm) var(--radius-sm) 0 0;
      background:transparent; border:1px solid transparent; border-bottom:none; color:var(--muted); cursor:pointer; transition:all .15s;
      display:inline-flex; align-items:center; gap:8px;
    }
    .tab svg { width:15px; height:15px; stroke:currentColor; fill:none; stroke-width:2; stroke-linecap:round; stroke-linejoin:round; }
    .tab:hover { color:var(--text); }
    .tab.active { color:var(--text); background:var(--bg); border-color:var(--border); }
    .tool-panes { border-top:1px solid var(--border); padding:20px; }
    .tool-pane { display:none; }
    .tool-pane.active { display:block; }
    .cmd-prefix { display:inline-flex; align-items:center; font-family:var(--mono); border:1px solid var(--border); background:var(--bg); color:var(--muted); border-radius:var(--radius-sm); padding:0 13px; font-size:0.85rem; }
    .cmd-output { margin-top:12px; background:var(--bg); border:1px solid var(--border); border-radius:var(--radius-sm); padding:13px; font-family:var(--mono); font-size:0.78rem; white-space:pre-wrap; word-break:break-word; color:var(--text-secondary); }
    .tool-meta { font-size:0.78rem; color:var(--muted); display:flex; align-items:center; }
    /* ── Output viewer (inspect / logs / command) ── */
    #output-panel { scroll-margin-top:84px; }
    .output-card { border-color:var(--accent); box-shadow:0 0 0 1px var(--accent-glow), 0 10px 40px rgba(0,0,0,0.35); animation:flashIn .9s ease; }
    @keyframes flashIn { 0% { box-shadow:0 0 0 3px var(--accent-glow), 0 10px 40px rgba(0,0,0,0.35); } 100% { box-shadow:0 0 0 1px var(--accent-glow), 0 10px 40px rgba(0,0,0,0.35); } }
    .mono-pill { font-family:var(--mono); font-size:0.74rem; font-weight:600; color:var(--accent-light); background:var(--accent-glow); border-radius:7px; padding:4px 10px; }
    .output-pre { margin:0; padding:18px 20px; font-family:var(--mono); font-size:0.78rem; line-height:1.6; color:var(--text-secondary); white-space:pre; overflow:auto; max-height:60vh; }
    .output-pre::-webkit-scrollbar { width:10px; height:10px; }
    .output-pre::-webkit-scrollbar-thumb { background:var(--surface-3); border-radius:6px; }
    /* ── Runbook from AI analysis ── */
    .ai-runbook { border-top:1px solid var(--border); padding:16px 20px; }
    .ai-runbook h4 { font-size:0.92rem; font-weight:700; margin-bottom:4px; display:flex; align-items:center; gap:8px; }
    .ai-runbook h4 svg { width:16px; height:16px; stroke:var(--accent-light); fill:none; stroke-width:2; stroke-linecap:round; stroke-linejoin:round; }
    .ai-runbook .hint { font-size:0.78rem; color:var(--muted); margin-bottom:13px; }
    ol.ai-steps { list-style:none; counter-reset:aistep; margin:0 0 14px; padding:0; display:flex; flex-direction:column; gap:6px; }
    ol.ai-steps li { counter-increment:aistep; display:flex; align-items:center; gap:11px; background:var(--bg); border:1px solid var(--border); border-radius:9px; padding:9px 12px; }
    ol.ai-steps li::before { content:counter(aistep); width:22px; height:22px; flex-shrink:0; border-radius:50%; background:var(--accent-glow); color:var(--accent-light); font-size:0.72rem; font-weight:800; display:flex; align-items:center; justify-content:center; }
    ol.ai-steps li code { font-family:var(--mono); font-size:0.78rem; color:var(--text-secondary); word-break:break-all; }
    .ai-actions { display:flex; gap:10px; flex-wrap:wrap; align-items:stretch; }
    .ai-actions form { margin:0; display:flex; gap:9px; }
    .ai-actions .save-form { flex:1; min-width:280px; }
    .ai-actions .save-form input { flex:1; min-width:160px; }
    .tool-hint { font-size:0.82rem; color:var(--muted); line-height:1.7; margin-top:10px; }
    .tool-hint code { font-family:var(--mono); background:var(--bg); padding:1px 6px; border-radius:5px; border:1px solid var(--border); font-size:0.76rem; color:var(--accent-light); }

    /* ── Table ── */
    .table-scroll { overflow-x:auto; }
    table { width:100%; border-collapse:collapse; font-size:0.88rem; }
    thead th {
      position:sticky; top:64px;
      padding:11px 14px; text-align:left; color:var(--muted); font-weight:700;
      font-size:0.72rem; text-transform:uppercase; letter-spacing:0.05em;
      background:var(--surface); border-bottom:1px solid var(--border); white-space:nowrap;
    }
    tbody td { padding:13px 14px; border-bottom:1px solid var(--border); vertical-align:middle; }
    tbody tr { transition:background .12s; }
    tbody tr:hover { background:rgba(255,255,255,0.025); }
    tbody tr:last-child td { border-bottom:none; }
    .c-name { font-weight:700; font-size:0.92rem; }
    .c-id { font-family:var(--mono); font-size:0.72rem; color:var(--muted); margin-top:2px; }
    .c-image { font-family:var(--mono); font-size:0.8rem; color:var(--text-secondary); word-break:break-all; }
    .c-sub { font-size:0.74rem; color:var(--muted); margin-top:2px; }
    .pill {
      display:inline-flex; align-items:center; gap:6px; font-size:0.74rem; font-weight:700;
      padding:4px 10px; border-radius:999px; text-transform:capitalize;
    }
    .pill::before { content:''; width:7px; height:7px; border-radius:50%; }
    .pill-running { color:#86efac; background:var(--success-bg); border:1px solid var(--success-border); }
    .pill-running::before { background:var(--success); }
    .pill-other { color:#fcd34d; background:var(--warning-bg); border:1px solid rgba(251,191,36,0.25); }
    .pill-other::before { background:var(--warning); }
    .metric { font-family:var(--mono); font-variant-numeric:tabular-nums; font-size:0.82rem; }
    .c-ports { font-family:var(--mono); font-size:0.78rem; color:var(--text-secondary); }

    .actions { display:flex; gap:6px; align-items:center; }
    .actions form { margin:0; }
    .btn-icon {
      width:34px; height:34px; padding:0; border-radius:9px; border:1px solid var(--border);
      background:var(--surface-2); color:var(--text-secondary);
      display:inline-flex; align-items:center; justify-content:center; cursor:pointer; position:relative; transition:all .15s;
    }
    .btn-icon svg { width:15px; height:15px; fill:none; stroke:currentColor; stroke-width:2; stroke-linecap:round; stroke-linejoin:round; pointer-events:none; }
    .btn-icon:hover { transform:translateY(-1px); border-color:var(--border-hover); }
    .ic-start:hover { color:var(--success); border-color:var(--success-border); background:var(--success-bg); }
    .ic-stop:hover, .ic-restart:hover { color:var(--warning); border-color:rgba(251,191,36,0.3); background:var(--warning-bg); }
    .ic-info:hover, .ic-logs:hover { color:var(--accent-light); border-color:var(--accent); background:var(--accent-glow); }
    .ic-ai:hover { color:#c4b5fd; border-color:#7c3aed; background:rgba(124,58,237,0.16); }
    .ic-remove:hover { color:var(--danger); border-color:rgba(239,68,68,0.3); background:var(--danger-bg); }
    .tip:hover::after {
      content:attr(data-tip); position:absolute; bottom:calc(100% + 7px); left:50%; transform:translateX(-50%);
      white-space:nowrap; background:#0b1324; border:1px solid var(--border-hover); color:var(--text);
      font-size:0.72rem; padding:5px 9px; border-radius:7px; z-index:20; font-weight:600;
    }
    .empty-row td { text-align:center; padding:48px; color:var(--muted); }

    /* ── Loading overlay (long Docker ops) ── */
    .loading-overlay { position:fixed; inset:0; z-index:1000; display:none; align-items:center; justify-content:center; background:rgba(10,15,28,0.74); backdrop-filter:blur(5px); -webkit-backdrop-filter:blur(5px); }
    .loading-overlay.show { display:flex; }
    .loading-box { background:var(--surface); border:1px solid var(--border-hover); border-radius:var(--radius); padding:30px 40px; text-align:center; box-shadow:0 24px 70px rgba(0,0,0,0.55); min-width:280px; }
    .spinner { width:48px; height:48px; margin:0 auto 18px; border-radius:50%; border:4px solid var(--surface-3); border-top-color:var(--accent); border-right-color:var(--accent); animation:spin .8s linear infinite; }
    @keyframes spin { to { transform:rotate(360deg); } }
    .loading-msg { font-weight:700; font-size:1.02rem; }
    .loading-sub { font-size:0.8rem; color:var(--muted); margin-top:4px; margin-bottom:18px; }
    .loading-bar { height:6px; border-radius:999px; background:var(--surface-3); overflow:hidden; position:relative; }
    .loading-bar::before { content:''; position:absolute; top:0; bottom:0; left:-40%; width:40%; border-radius:999px; background:linear-gradient(90deg, transparent, var(--accent-light), transparent); animation:indeterminate 1.1s ease-in-out infinite; }
    @keyframes indeterminate { 0% { left:-40%; } 100% { left:100%; } }

    /* ── Quick links ── */
    .quick-links { display:flex; gap:12px; flex-wrap:wrap; margin-top:14px; }
    .quick-link {
      display:flex; align-items:center; gap:13px; flex:1; min-width:240px;
      background:var(--surface); border:1px solid var(--border); border-radius:var(--radius);
      padding:16px 18px; transition:border-color .2s, transform .2s;
    }
    .quick-link:hover { border-color:var(--border-hover); transform:translateY(-2px); }
    .quick-link .ql-icon { width:42px; height:42px; border-radius:11px; background:var(--accent-glow); display:flex; align-items:center; justify-content:center; flex-shrink:0; }
    .quick-link .ql-icon svg { width:20px; height:20px; stroke:var(--accent-light); fill:none; stroke-width:2; stroke-linecap:round; stroke-linejoin:round; }
    .quick-link .ql-title { font-weight:700; font-size:0.95rem; }
    .quick-link .ql-desc { font-size:0.8rem; color:var(--muted); margin-top:1px; }

    .small { font-size:0.8rem; color:var(--muted); }

    @media (max-width:1100px) { .overview { grid-template-columns:repeat(2,1fr); } .usage-row { grid-template-columns:1fr; } }
    @media (max-width:760px) {
      .wrap { padding:0 16px; }
      .nav-links a span { display:none; }
      .brand .tag { display:none; }
      .overview { grid-template-columns:1fr 1fr; }
      .search-wrap { max-width:none; min-width:0; }
      thead th { position:static; }
    }
  </style>
</head>
<body>
  <div class="loading-overlay" id="loading-overlay" role="status" aria-live="polite">
    <div class="loading-box">
      <div class="spinner"></div>
      <div class="loading-msg" id="loading-msg">Working…</div>
      <div class="loading-sub">Running Docker operation, please wait</div>
      <div class="loading-bar"></div>
    </div>
  </div>
  <header class="nav">
    <div class="wrap nav-inner">
      <div class="brand">
        <span class="logo-badge"><svg viewBox="0 0 24 24"><path d="M3 7l9-4 9 4-9 4-9-4z"/><path d="M3 12l9 4 9-4"/><path d="M3 17l9 4 9-4"/></svg></span>
        <span class="name">Dock<span>Pilot</span></span>
        <span class="tag">Docker Cockpit</span>
      </div>
      <nav class="nav-links">
        <a href="/" class="active"><svg viewBox="0 0 24 24"><rect x="3" y="3" width="7" height="7"/><rect x="14" y="3" width="7" height="7"/><rect x="14" y="14" width="7" height="7"/><rect x="3" y="14" width="7" height="7"/></svg><span>Dashboard</span></a>
        <a href="/images"><svg viewBox="0 0 24 24"><rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18"/><path d="M9 21V9"/></svg><span>Images</span></a>
        <a href="/volumes"><svg viewBox="0 0 24 24"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M3 5v14a9 3 0 0 0 18 0V5"/><path d="M3 12a9 3 0 0 0 18 0"/></svg><span>Volumes</span></a>
        <a href="/networks"><svg viewBox="0 0 24 24"><circle cx="5" cy="12" r="2"/><circle cx="19" cy="6" r="2"/><circle cx="19" cy="18" r="2"/><path d="M7 12h6"/><path d="M13 12l4-5"/><path d="M13 12l4 5"/></svg><span>Networks</span></a>
        <a href="/ipam"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg><span>IPAM</span></a>
        <a href="/runbooks"><svg viewBox="0 0 24 24"><path d="M9 11l3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/></svg><span>Runbooks</span></a>
        <a href="/landing"><svg viewBox="0 0 24 24"><path d="M3 12l2-2m0 0l7-7 7 7M5 10v10a1 1 0 0 0 1 1h3m10-11l2 2m-2-2v10a1 1 0 0 1-1 1h-3"/></svg><span>About</span></a>
      </nav>
      <div class="nav-spacer"></div>
      <div class="socket-badge"><span class="dot"></span> Socket <span class="mono">{{.DockerHost}}</span></div>
    </div>
  </header>

  <main class="wrap">
    <div class="page-head">
      <h1>Container Dashboard</h1>
      <p>Live host container operations &middot; {{.Now}}</p>
    </div>

    {{if .Success}}<div class="msg ok">{{.Success}}</div>{{end}}
    {{if .Error}}<div class="msg err">{{.Error}}</div>{{end}}

    {{if .CommandOutput}}
    <!-- Output viewer (inspect / logs / command result) -->
    <section class="section" id="output-panel">
      <div class="card output-card">
        <div class="card-head">
          <h3><svg viewBox="0 0 24 24"><path d="M4 17l6-6-6-6M12 19h8"/></svg> Output</h3>
          {{if .CommandInput}}<span class="mono-pill">docker {{.CommandInput}}</span>{{end}}
          <button type="button" class="btn" style="margin-left:auto;" onclick="closeOutput()"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M18 6L6 18M6 6l12 12"/></svg> Close</button>
        </div>
        <pre class="output-pre">{{.CommandOutput}}</pre>
        {{if .AISteps}}
        <div class="ai-runbook">
          <h4><svg viewBox="0 0 24 24"><path d="M9 11l3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/></svg> Runbook from this analysis</h4>
          <div class="hint">{{len .AISteps}} Docker command(s) extracted &middot; interactive/streaming flags removed for safe one-shot runs.</div>
          <ol class="ai-steps">
            {{range .AISteps}}<li><code>docker {{.Command}}</code></li>{{end}}
          </ol>
          <div class="ai-actions">
            <form method="post" action="/ai/runbook/run">
              <input type="hidden" name="q" value="{{.Search}}" />
              <input type="hidden" name="steps" value="{{.AIStepsEncoded}}" />
              <button class="btn btn-primary" type="submit" data-loading="Running AI steps…"><svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 5v14l11-7z"/></svg> Run all now</button>
            </form>
            <form method="post" action="/ai/runbook/save" class="save-form">
              <input type="hidden" name="steps" value="{{.AIStepsEncoded}}" />
              <input class="fld" name="name" value="{{.AIStepsTitle}}" placeholder="Runbook name" />
              <button class="btn" type="submit" data-loading="Saving runbook…"><svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/><path d="M17 21v-8H7v8M7 3v5h8"/></svg> Save as Runbook</button>
            </form>
          </div>
        </div>
        {{end}}
      </div>
    </section>
    {{end}}

    <!-- Overview -->
    <section class="section">
      <div class="section-title"><svg viewBox="0 0 24 24"><path d="M3 3v18h18"/><path d="M18 17V9M13 17V5M8 17v-3"/></svg> Overview</div>
      <div class="overview">
        <div class="kpi accent"><div class="label"><svg viewBox="0 0 24 24"><rect x="2" y="7" width="20" height="14" rx="2"/><path d="M7 7V5a2 2 0 0 1 2-2h6a2 2 0 0 1 2 2v2"/></svg> Total Containers</div><div class="value" id="kpi-total">{{.Total}}</div></div>
        <div class="kpi green"><div class="label"><svg viewBox="0 0 24 24"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg> Running</div><div class="value" id="kpi-running">{{.Running}}</div></div>
        <div class="kpi warn"><div class="label"><svg viewBox="0 0 24 24"><path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><path d="M12 9v4M12 17h.01"/></svg> Stopped</div><div class="value" id="kpi-stopped">{{.Stopped}}</div></div>
        <div class="kpi accent"><div class="label"><svg viewBox="0 0 24 24"><path d="M3 7l9-4 9 4-9 4-9-4z"/><path d="M3 12l9 4 9-4M3 17l9 4 9-4"/></svg> Images</div><div class="value" id="kpi-images">{{.Images}}</div></div>
      </div>

      <div class="usage-row">
        {{with .Usage}}
        <div class="usage-card">
          <div class="usage-donut">
            <svg viewBox="0 0 36 36"><circle class="track" cx="18" cy="18" r="15.9155"/><circle id="arc-cpu" class="arc arc-cpu" cx="18" cy="18" r="15.9155" stroke-dasharray="{{printf "%.2f" .CPUPercent}} 100"/></svg>
            <div class="pct" id="pct-cpu">{{printf "%.0f" .CPUPercent}}%</div>
          </div>
          <div class="usage-meta"><div class="title">CPU</div><div class="used" id="lbl-cpu-used">{{.CPUUsedLabel}}</div><div class="total">of {{.CPUTotalLabel}}</div></div>
        </div>
        <div class="usage-card">
          <div class="usage-donut">
            <svg viewBox="0 0 36 36"><circle class="track" cx="18" cy="18" r="15.9155"/><circle id="arc-mem" class="arc arc-mem" cx="18" cy="18" r="15.9155" stroke-dasharray="{{printf "%.2f" .MemPercent}} 100"/></svg>
            <div class="pct" id="pct-mem">{{printf "%.0f" .MemPercent}}%</div>
          </div>
          <div class="usage-meta"><div class="title">Memory</div><div class="used" id="lbl-mem-used">{{.MemUsedLabel}}</div><div class="total">of {{.MemTotalLabel}}</div></div>
        </div>
        <div class="usage-card">
          <div class="usage-donut">
            <svg viewBox="0 0 36 36"><circle class="track" cx="18" cy="18" r="15.9155"/><circle id="arc-disk" class="arc arc-disk" cx="18" cy="18" r="15.9155" stroke-dasharray="{{printf "%.2f" .DiskPercent}} 100"/></svg>
            <div class="pct" id="pct-disk">{{printf "%.0f" .DiskPercent}}%</div>
          </div>
          <div class="usage-meta"><div class="title">Storage (Docker)</div><div class="used" id="lbl-disk-used">{{.DiskUsedLabel}}</div><div class="total">images + containers + volumes</div></div>
        </div>
        {{end}}
      </div>
    </section>

    <!-- Containers (hero) -->
    <section class="section">
      <div class="card">
        <div class="card-head">
          <h3><svg viewBox="0 0 24 24"><rect x="2" y="7" width="20" height="14" rx="2"/><path d="M7 7V5a2 2 0 0 1 2-2h6a2 2 0 0 1 2 2v2"/></svg> Containers</h3>
          <span class="count-pill" id="ccount">{{.Total}}</span>
          <div class="search-wrap">
            <svg viewBox="0 0 24 24"><circle cx="11" cy="11" r="7"/><path d="M21 21l-4.3-4.3"/></svg>
            <input id="cfilter" type="text" autocomplete="off" placeholder="Filter by name, image, state or ports…" oninput="filterContainers()" value="{{.Search}}" />
          </div>
          <span class="filter-hint" id="filterhint"></span>
          <span class="live-pill" id="livepill" title="Live metrics auto-refresh"><span class="live-dot"></span><span id="liveage">live</span></span>
        </div>
        <div class="table-scroll">
          <table id="ctable">
            <thead>
              <tr>
                <th>Name</th>
                <th>Image</th>
                <th>State</th>
                <th>Ports</th>
                <th>CPU</th>
                <th>Memory</th>
                <th>Age</th>
                <th style="text-align:right;">Actions</th>
              </tr>
            </thead>
            <tbody>
              {{if .Containers}}
                {{range .Containers}}
                <tr data-row data-cid="{{.ID}}" data-search="{{.Name}} {{.ID}} {{.Image}} {{.State}} {{.Status}} {{.Ports}}">
                  <td><div class="c-name">{{.Name}}</div><div class="c-id">{{.ID}}</div></td>
                  <td><span class="c-image">{{.Image}}</span></td>
                  <td>
                    {{if eq .State "running"}}<span class="pill pill-running">running</span>{{else}}<span class="pill pill-other">{{.State}}</span>{{end}}
                    <div class="c-sub">{{.Status}}</div>
                  </td>
                  <td>{{if .Ports}}<span class="c-ports">{{.Ports}}</span>{{else}}<span class="small">—</span>{{end}}</td>
                  <td class="metric"><span class="js-cpu">{{if .CPUPerc}}{{.CPUPerc}}{{else}}<span class="small">—</span>{{end}}</span></td>
                  <td class="metric"><span class="js-mem">{{if .MemUsage}}{{.MemUsage}}{{else}}<span class="small">—</span>{{end}}</span><div class="c-sub js-memperc">{{.MemPerc}}</div></td>
                  <td><span class="small">{{.Created}}</span></td>
                  <td>
                    <div class="actions" style="justify-content:flex-end;">
                      <form method="post" action="/containers/{{.ID}}/start"><input type="hidden" name="q" value="{{$.Search}}" /><button class="btn-icon ic-start tip" type="submit" data-tip="Start" aria-label="Start {{.Name}}"><svg viewBox="0 0 24 24"><path d="M8 5v14l11-7z"/></svg></button></form>
                      <form method="post" action="/containers/{{.ID}}/stop"><input type="hidden" name="q" value="{{$.Search}}" /><button class="btn-icon ic-stop tip" type="submit" data-tip="Stop" aria-label="Stop {{.Name}}"><svg viewBox="0 0 24 24"><rect x="7" y="7" width="10" height="10" rx="1"/></svg></button></form>
                      <form method="post" action="/containers/{{.ID}}/restart"><input type="hidden" name="q" value="{{$.Search}}" /><button class="btn-icon ic-restart tip" type="submit" data-tip="Restart" aria-label="Restart {{.Name}}"><svg viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/></svg></button></form>
                      <form method="post" action="/containers/{{.ID}}/analyze"><input type="hidden" name="q" value="{{$.Search}}" /><button class="btn-icon ic-ai tip" type="submit" data-tip="Analyze with AI" aria-label="Analyze {{.Name}} with AI" data-loading="Analyzing container with AI… (this can take a while)"><svg viewBox="0 0 24 24"><path d="M12 3l1.9 4.6L18 9l-4.1 1.4L12 15l-1.9-4.6L6 9l4.1-1.4z"/><path d="M5 18l.9 2.2L8 21l-2.1.8L5 24l-.9-2.2L2 21l2.1-.8z"/></svg></button></form>
                      <form method="post" action="/containers/{{.ID}}/inspect"><input type="hidden" name="q" value="{{$.Search}}" /><button class="btn-icon ic-info tip" type="submit" data-tip="Inspect" aria-label="Inspect {{.Name}}"><svg viewBox="0 0 24 24"><circle cx="11" cy="11" r="6"/><path d="M20 20l-4.35-4.35"/></svg></button></form>
                      <form method="post" action="/containers/{{.ID}}/logs"><input type="hidden" name="q" value="{{$.Search}}" /><button class="btn-icon ic-logs tip" type="submit" data-tip="Logs" aria-label="Logs {{.Name}}"><svg viewBox="0 0 24 24"><path d="M5 4h14v16H5z"/><path d="M8 8h8"/><path d="M8 12h8"/><path d="M8 16h6"/></svg></button></form>
                      <form method="post" action="/containers/{{.ID}}/remove" onsubmit="return confirm('Remove container {{.Name}}?')"><input type="hidden" name="q" value="{{$.Search}}" /><button class="btn-icon ic-remove tip" type="submit" data-tip="Remove" aria-label="Remove {{.Name}}"><svg viewBox="0 0 24 24"><path d="M3 6h18"/><path d="M8 6V4h8v2"/><path d="M7 6l1 14h8l1-14"/></svg></button></form>
                    </div>
                  </td>
                </tr>
                {{end}}
              {{else}}
                <tr class="empty-row"><td colspan="8">No containers found.</td></tr>
              {{end}}
              <tr class="empty-row" id="cnomatch" style="display:none;"><td colspan="8">No containers match your filter.</td></tr>
            </tbody>
          </table>
        </div>
      </div>

      <div class="quick-links">
        <a class="quick-link" href="/ipam">
          <span class="ql-icon"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg></span>
          <span><div class="ql-title">IPAM</div><div class="ql-desc">IP &amp; port manager — map host ports and networks</div></span>
        </a>
        <a class="quick-link" href="/runbooks">
          <span class="ql-icon"><svg viewBox="0 0 24 24"><path d="M9 11l3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/></svg></span>
          <span><div class="ql-title">Runbooks</div><div class="ql-desc">Automated cleanup, diagnostics &amp; recovery workflows</div></span>
        </a>
      </div>
    </section>

    <!-- Tools -->
    <section class="section">
      <div class="section-title"><svg viewBox="0 0 24 24"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18v3h3l6.3-6.3a4 4 0 0 0 5.4-5.4l-2.6 2.6-2-2 2.6-2.6z"/></svg> Tools</div>
      <div class="card">
        <div class="tabs">
          <button class="tab active" id="tab-launch" onclick="showTool('launch')"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg> Launch Container</button>
          <button class="tab" id="tab-run" onclick="showTool('run')"><svg viewBox="0 0 24 24"><path d="M4 17l6-6-6-6M12 19h8"/></svg> Run Command</button>
          <button class="tab" id="tab-ai" onclick="showTool('ai')"><svg viewBox="0 0 24 24"><path d="M12 3l1.9 4.6L18 9l-4.1 1.4L12 15l-1.9-4.6L6 9l4.1-1.4z"/><path d="M5 18l.6 1.8L7 20l-1.4.5L5 22l-.6-1.5L3 20l1.4-.2z"/></svg> AI Assistant</button>
        </div>
        <div class="tool-panes">
          <div class="tool-pane active" id="tool-launch">
            <form method="post" action="/containers/create">
              <div class="row">
                <input class="fld" name="name" placeholder="name (optional)" style="flex:1; min-width:160px;" />
                <input class="fld" name="image" placeholder="image:tag (required)" required style="flex:2; min-width:220px;" />
              </div>
              <div class="row" style="margin-top:10px;">
                <input class="fld" name="ports" placeholder="ports e.g. 8081:80,8443:443" style="flex:1; min-width:280px;" />
              </div>
              <div class="row" style="margin-top:10px;">
                <input class="fld" name="env" placeholder="env e.g. KEY=a,MODE=prod" style="flex:1; min-width:280px;" />
              </div>
              <div class="row" style="margin-top:10px;">
                <input class="fld" name="command" placeholder="command args e.g. sleep,3600" style="flex:1; min-width:280px;" />
              </div>
              <div class="row" style="margin-top:14px; align-items:center;">
                <label class="small" style="display:flex; align-items:center; gap:6px;"><input type="checkbox" name="auto_start" checked /> auto-start</label>
                <button class="btn btn-primary" type="submit" data-loading="Creating container…"><svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg> Create Container</button>
              </div>
            </form>
            <div class="tool-hint">Use comma-separated values for ports/env/command. Example: ports <code>8080:80,8443:443</code>, env <code>MODE=prod,DEBUG=false</code>.</div>
          </div>

          <div class="tool-pane" id="tool-run">
            <form method="post" action="/docker/exec">
              <input type="hidden" name="q" value="{{.Search}}" />
              <div class="row" style="align-items:stretch;">
                <span class="cmd-prefix">docker</span>
                <input class="fld" name="command" value="{{.CommandInput}}" placeholder="system prune -f" style="flex:1; min-width:240px;" />
                <button class="btn btn-primary" type="submit" data-loading="Running command…">Run</button>
              </div>
            </form>
            <div class="tool-hint">Commands run with the Docker CLI on this host. Output opens in the highlighted panel at the top.</div>
          </div>

          <div class="tool-pane" id="tool-ai">
            <form method="post" action="/ai/interpret">
              <input type="hidden" name="q" value="{{.Search}}" />
              <textarea class="fld" name="ai_prompt" placeholder="Example: clean unused images and stopped containers safely">{{.AIPrompt}}</textarea>
              <div class="row" style="margin-top:12px; align-items:center;">
                <button class="btn btn-primary" type="submit" data-loading="Asking AI… (this can take a while)"><svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M12 3l1.9 4.6L18 9l-4.1 1.4L12 15l-1.9-4.6L6 9l4.1-1.4z"/></svg> Suggest Docker Command</button>
                <div class="tool-meta">Model: {{.AIModel}}</div>
              </div>
            </form>
            {{if .AISuggestion}}<div class="cmd-output">Suggested: docker {{.AISuggestion}}{{if .AIExplanation}}\n\nWhy: {{.AIExplanation}}{{end}}</div>{{end}}
            <div class="tool-hint">Powered by local Ollama. Set <code>OLLAMA_BASE_URL</code> env if needed. Suggestions are not executed automatically.</div>
          </div>
        </div>
      </div>
    </section>
  </main>

  <script>
    function showTool(name) {
      var names = ['launch','run','ai'];
      for (var i = 0; i < names.length; i++) {
        var n = names[i];
        document.getElementById('tool-' + n).classList.toggle('active', n === name);
        document.getElementById('tab-' + n).classList.toggle('active', n === name);
      }
    }
    var loadingTimer = null;
    function showLoading(msg) {
      var ov = document.getElementById('loading-overlay');
      if (!ov) return;
      document.getElementById('loading-msg').textContent = msg || 'Working…';
      ov.classList.add('show');
      // Safety: auto-hide if navigation never happens (e.g. error)
      if (loadingTimer) clearTimeout(loadingTimer);
      loadingTimer = setTimeout(hideLoading, 45000);
    }
    function hideLoading() {
      var ov = document.getElementById('loading-overlay');
      if (ov) ov.classList.remove('show');
      if (loadingTimer) { clearTimeout(loadingTimer); loadingTimer = null; }
    }
    var loadingVerbs = { Start:'Starting container…', Stop:'Stopping container…', Restart:'Restarting container…', Inspect:'Inspecting container…', Logs:'Loading logs…', Remove:'Removing container…' };
    // Show the spinner whenever a Docker operation is submitted
    document.addEventListener('submit', function(e) {
      if (e.defaultPrevented) return; // e.g. user cancelled a confirm()
      var btn = e.submitter;
      var msg = 'Working…';
      if (btn) {
        if (btn.getAttribute('data-loading')) msg = btn.getAttribute('data-loading');
        else if (btn.getAttribute('data-tip')) msg = loadingVerbs[btn.getAttribute('data-tip')] || (btn.getAttribute('data-tip') + '…');
      }
      showLoading(msg);
    }, false);
    // Show the spinner on internal page navigation (Dashboard / IPAM / Runbooks links)
    document.addEventListener('click', function(e) {
      var a = e.target.closest ? e.target.closest('a') : null;
      if (!a) return;
      var href = a.getAttribute('href') || '';
      if (a.target === '_blank' || e.metaKey || e.ctrlKey) return;
      if (href.charAt(0) === '/' ) showLoading('Loading…');
    }, false);
    // Hide overlay when a page is restored from the back/forward cache
    window.addEventListener('pageshow', function(e) { if (e.persisted) hideLoading(); });

    function closeOutput() {
      var p = document.getElementById('output-panel');
      if (p) p.parentNode.removeChild(p);
      window.scrollTo({ top: 0, behavior: 'smooth' });
    }
    function filterContainers() {
      var q = document.getElementById('cfilter').value.trim().toLowerCase();
      var rows = document.querySelectorAll('#ctable tbody tr[data-row]');
      var shown = 0;
      for (var i = 0; i < rows.length; i++) {
        var hay = (rows[i].getAttribute('data-search') || '').toLowerCase();
        var match = q === '' || hay.indexOf(q) !== -1;
        rows[i].style.display = match ? '' : 'none';
        if (match) shown++;
      }
      var count = document.getElementById('ccount');
      if (count) count.textContent = shown;
      var hint = document.getElementById('filterhint');
      if (hint) hint.textContent = q === '' ? '' : (shown + ' of ' + rows.length);
      var nomatch = document.getElementById('cnomatch');
      if (nomatch) nomatch.style.display = (shown === 0 && rows.length > 0) ? '' : 'none';
    }
    // Restore active tool tab based on server-side output
    (function() {
      var active = '{{if .AISuggestion}}ai{{else if .AIPrompt}}ai{{else if .CommandInput}}run{{else}}launch{{end}}';
      if (active !== 'launch') showTool(active);
      // Apply any pre-filled filter on load
      if (document.getElementById('cfilter').value) filterContainers();
      // Bring inspect/logs/command output to eye level
      var outPanel = document.getElementById('output-panel');
      if (outPanel) {
        requestAnimationFrame(function() {
          outPanel.scrollIntoView({ behavior: 'smooth', block: 'start' });
        });
      }
    })();

    // ---- Live metrics hydration -------------------------------------------
    // The page shell renders instantly from the warm server-side cache; this
    // poller keeps CPU/memory cells, KPI counts and usage gauges current
    // without ever reloading the document.
    (function() {
      var POLL_MS = 3000;
      var DASH = '<span class="small">—</span>';
      function setText(id, val) { var el = document.getElementById(id); if (el) el.textContent = val; }
      function setArc(id, pct) {
        var el = document.getElementById(id);
        if (el) el.setAttribute('stroke-dasharray', (pct || 0).toFixed(2) + ' 100');
      }
      function applyUsage(u) {
        if (!u) return;
        setArc('arc-cpu', u.cpuPercent); setText('pct-cpu', Math.round(u.cpuPercent) + '%'); setText('lbl-cpu-used', u.cpuUsed);
        setArc('arc-mem', u.memPercent); setText('pct-mem', Math.round(u.memPercent) + '%'); setText('lbl-mem-used', u.memUsed);
        setArc('arc-disk', u.diskPercent); setText('pct-disk', Math.round(u.diskPercent) + '%'); setText('lbl-disk-used', u.diskUsed);
      }
      function applyRows(list) {
        if (!list) return;
        for (var i = 0; i < list.length; i++) {
          var c = list[i];
          var row = document.querySelector('tr[data-cid="' + c.id + '"]');
          if (!row) continue;
          var cpu = row.querySelector('.js-cpu');
          if (cpu) cpu.innerHTML = c.cpu ? c.cpu : DASH;
          var mem = row.querySelector('.js-mem');
          if (mem) mem.innerHTML = c.mem ? c.mem : DASH;
          var mp = row.querySelector('.js-memperc');
          if (mp) mp.textContent = c.mem ? (c.memPerc || '') : '';
        }
      }
      function markLive(ageMs) {
        var pill = document.getElementById('livepill');
        var age = document.getElementById('liveage');
        if (!pill || !age) return;
        var secs = Math.max(0, Math.round((ageMs || 0) / 1000));
        age.textContent = secs <= 1 ? 'live' : 'live · ' + secs + 's';
        pill.classList.toggle('stale', (ageMs || 0) > POLL_MS * 4);
      }
      function tick() {
        var q = new URLSearchParams(location.search).get('q') || '';
        fetch('/api/dashboard.json' + (q ? ('?q=' + encodeURIComponent(q)) : ''), { headers: { 'Accept': 'application/json' } })
          .then(function(r) { if (!r.ok) throw new Error('http ' + r.status); return r.json(); })
          .then(function(d) {
            setText('kpi-total', d.total); setText('kpi-running', d.running);
            setText('kpi-stopped', d.stopped); setText('kpi-images', d.images);
            applyUsage(d.usage); applyRows(d.containers); markLive(d.cacheAgeMs);
          })
          .catch(function() {
            var pill = document.getElementById('livepill');
            if (pill) { pill.classList.add('stale'); var a = document.getElementById('liveage'); if (a) a.textContent = 'offline'; }
          });
      }
      // Only poll when the tab is visible to avoid needless load.
      var timer = null;
      function start() { if (!timer) { tick(); timer = setInterval(function() { if (!document.hidden) tick(); }, POLL_MS); } }
      function stop() { if (timer) { clearInterval(timer); timer = null; } }
      document.addEventListener('visibilitychange', function() { if (document.hidden) stop(); else start(); });
      start();
    })();
  </script>
</body>
</html>
`

const ipamHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>IPAM — DockPilot</title>
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600;700&display=swap" rel="stylesheet">
  <style>
    :root {
      --bg:#0a0e1a;
      --surface:#111827;
      --border:#1f2937;
      --accent:#3b82f6;
      --success:#10b981;
      --warning:#f59e0b;
      --danger:#ef4444;
      --muted:#6b7280;
      --text:#e5e7eb;
    }
    * { box-sizing:border-box; }
    body {
      margin:0;
      background: radial-gradient(1200px 600px at 90% -100px, #1d4ed833 0%, transparent 60%), var(--bg);
      color:var(--text);
      font-family:'JetBrains Mono', ui-monospace, monospace;
    }
    .wrap { width:100%; max-width:none; margin:0; padding:20px 24px; }
    .header {
      display:flex; justify-content:space-between; align-items:center;
      border:1px solid var(--border); background:var(--surface); border-radius:12px;
      padding:14px 16px; margin-bottom:14px;
    }
    .title { font-size:22px; font-weight:700; letter-spacing:.4px; line-height:1.1; }
    .title-row { display:flex; align-items:center; gap:10px; }
    .cockpit-badge {
      font-size:11px; color:var(--muted); background:var(--surface);
      border:1px solid var(--border); border-radius:6px; padding:2px 8px; line-height:1.2;
    }
    .badge { color:var(--muted); border:1px solid var(--border); border-radius:999px; padding:3px 10px; font-size:12px; }
    .icon { width:18px; height:18px; display:inline-block; color:var(--accent); }
    .icon.live { color:var(--success); }
    .small { font-size:12px; color:var(--muted); }
    .panel { background:var(--surface); border:1px solid var(--border); border-radius:12px; padding:14px; margin-bottom:14px; }
    .msg { padding:10px; border-radius:8px; margin:0 0 10px 0; font-size:13px; }
    .err { background:#3f0d15; border:1px solid #7f1d1d; }
    .ok { background:#022c22; border:1px solid #065f46; }
    .search-row { display:flex; gap:8px; align-items:center; flex-wrap:wrap; margin-bottom:18px; }
    .search-row .search { margin-bottom:0; }
    .btn-ai { background:linear-gradient(135deg,#7c3aed,#2563eb); border:1px solid #7c3aed; color:#fff; font-weight:600; padding:8px 16px; border-radius:8px; cursor:pointer; font-family:inherit; font-size:13px; }
    .btn-ai:hover { opacity:0.92; }
    .ai-panel { border:1px solid #4c1d95; border-radius:12px; background:linear-gradient(180deg,rgba(124,58,237,0.12),rgba(15,23,42,0.4)); padding:14px; margin-bottom:14px; }
    .ai-head { display:flex; align-items:center; gap:10px; margin-bottom:10px; }
    .ai-title { font-weight:700; color:#c4b5fd; font-size:13px; }
    .ai-model { margin-left:auto; font-size:11px; color:var(--muted); }
    .ai-body { background:#0b1324; border:1px solid var(--border); border-radius:8px; padding:12px; font-size:12.5px; line-height:1.55; white-space:pre-wrap; word-break:break-word; max-height:420px; overflow:auto; color:var(--text); margin:0; }
    .ai-note { margin-top:8px; font-size:11px; color:var(--muted); }
    .kpis { display:grid; grid-template-columns:repeat(4,minmax(0,1fr)); gap:10px; margin-bottom:14px; }
    .kpi { background:var(--surface); border:1px solid var(--border); border-radius:10px; padding:12px; }
    .kpi .label { color:var(--muted); font-size:12px; display:flex; align-items:center; gap:6px; }
    .kpi .value { font-size:22px; font-weight:700; margin-top:6px; }
    table { width:100%; border-collapse:collapse; font-size:13px; }
    th, td { padding:10px 8px; border-bottom:1px solid var(--border); text-align:left; vertical-align:top; }
    th { color:var(--muted); font-weight:600; }
    .state-running { color:var(--success); font-weight:700; }
    .state-other { color:var(--warning); font-weight:700; }
    .back-link {
      display:inline-flex; align-items:center; gap:6px;
      color:var(--accent); text-decoration:none; font-size:13px; font-weight:600;
      margin-bottom:14px;
    }
    .back-link:hover { text-decoration:underline; }
    .port-range-tag {
      display:inline-block; background:#1e3a5f; border:1px solid var(--accent);
      border-radius:6px; padding:3px 10px; margin:3px 4px; font-size:13px;
      font-weight:600; color:var(--accent);
    }
    .ranges-wrap { display:flex; flex-wrap:wrap; gap:2px; padding:6px 0; }
    .search { display:flex; gap:8px; align-items:center; margin-bottom:14px; }
    .search input { flex:1; min-width:220px; padding:8px 12px; background:var(--surface); border:1px solid var(--border); border-radius:8px; color:var(--text); font-family:inherit; font-size:13px; }
    .search input:focus { outline:none; border-color:var(--accent); }
    .btn-primary { background:var(--accent); border:1px solid var(--accent); color:white; font-weight:600; padding:8px 16px; border-radius:8px; cursor:pointer; font-family:inherit; font-size:13px; }
    .btn-primary:hover { opacity:0.9; }
    .net-grid { display:grid; grid-template-columns:repeat(auto-fill, minmax(340px, 1fr)); gap:12px; }
    .net-card { background:var(--bg); border:1px solid var(--border); border-radius:10px; padding:12px; }
    .net-card h4 { margin:0 0 8px 0; font-size:14px; display:flex; align-items:center; gap:8px; }
    .net-driver { font-size:11px; color:var(--muted); background:var(--surface); border:1px solid var(--border); border-radius:4px; padding:1px 6px; font-weight:400; }
    .net-meta { font-size:12px; color:var(--muted); margin-bottom:8px; }
    .net-meta span { margin-right:14px; }
    .net-containers { font-size:12px; }
    .net-containers table { font-size:12px; }
    .net-containers th { font-size:11px; }
    .ip-internal { color:var(--warning); font-weight:600; }
    .ip-external { color:var(--success); font-weight:600; }
    @media (max-width: 980px) {
      .wrap { padding:14px; }
      .kpis { grid-template-columns:1fr; }
      .net-grid { grid-template-columns:1fr; }
    }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="header">
      <div>
        <div class="title-row">
          <svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 8v4l3 3"/></svg>
          <div class="title">IPAM</div>
          <div class="cockpit-badge">IP &amp; Port Manager</div>
        </div>
        <div class="small">All host ports allocated by Docker containers</div>
      </div>
      <div class="badge"><img class="icon live" src="/static/icons/activity.svg" alt="live" /> {{.DockerHost}} | {{.Now}}</div>
    </div>

    <a class="back-link" href="/">
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 12H5"/><path d="M12 5l-7 7 7 7"/></svg>
      Back to Dashboard
		</a>

		<div class="search-row">
			<form class="search" method="get" action="/ipam">
				<input name="q" value="{{.Search}}" placeholder="Search ports, IPs, container name or state" style="min-width:220px;" />
				<button class="btn-primary" type="submit">Search</button>
				{{if .Search}}<a class="small" href="/ipam">clear</a>{{end}}
			</form>
			<form method="post" action="/ipam/analyze">
				<input type="hidden" name="q" value="{{.Search}}" />
				<button class="btn-ai" type="submit">✦ Analyze with AI</button>
			</form>
		</div>

		{{if .Success}}<div class="msg ok">{{.Success}}</div>{{end}}
		{{if .Error}}<div class="msg err">{{.Error}}</div>{{end}}

		{{if .AIAnalysis}}
		<div class="ai-panel">
			<div class="ai-head">
				<span class="ai-title">✦ AI Network Analysis</span>
				<span class="ai-model">Model: {{.AIModel}}</span>
			</div>
			<pre class="ai-body">{{.AIAnalysis}}</pre>
			<div class="ai-note">AI-generated review — verify before acting. Nothing was changed on the host.</div>
		</div>
		{{end}}

		<div class="kpis">
      <div class="kpi">
        <div class="label"><img class="icon" src="/static/icons/cpu.svg" alt="ports" /> Host Ports In Use</div>
        <div class="value">{{.TotalPorts}}</div>
      </div>
      <div class="kpi">
        <div class="label"><img class="icon" src="/static/icons/layers.svg" alt="ranges" /> Port Ranges</div>
        <div class="value">{{len .UsedRanges}}</div>
      </div>
      <div class="kpi">
        <div class="label"><img class="icon" src="/static/icons/shield.svg" alt="summary" /> Quick Summary</div>
        <div class="value small" style="margin-top:10px;">
          {{if .UsedRanges}}
            <div class="ranges-wrap">
            {{range .UsedRanges}}<span class="port-range-tag">{{.Display}}</span>{{end}}
            </div>
          {{else}}
            No ports allocated
          {{end}}
        </div>
      </div>
      <div class="kpi">
        <div class="label"><svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2z"/><path d="M2 12h20"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg> Docker Networks</div>
        <div class="value">{{len .Networks}}</div>
      </div>
    </div>

    <div class="panel">
      <h3>Port Allocations</h3>
      <table>
        <thead>
          <tr>
            <th>#</th>
            <th>Host Port</th>
            <th>Container Port</th>
            <th>Protocol</th>
            <th>Network</th>
            <th>Internal IP (Container)</th>
            <th>External Access (Host)</th>
            <th>Container</th>
            <th>State</th>
          </tr>
        </thead>
        <tbody>
          {{if .PortMappings}}
            {{range $i, $p := .PortMappings}}
            <tr>
              <td class="small">{{add $i 1}}</td>
              <td><strong>{{$p.HostPort}}</strong></td>
              <td>{{$p.ContainerPort}}</td>
              <td>{{$p.Protocol}}</td>
              <td>{{if $p.Network}}<span class="net-driver">{{$p.Network}}</span>{{else}}<span class="small">—</span>{{end}}</td>
              <td>{{if $p.InternalIP}}<span class="ip-internal">{{$p.InternalIP}}:{{$p.ContainerPort}}</span>{{else}}<span class="small">—</span>{{end}}</td>
              <td><span class="ip-external">{{$p.HostIP}}:{{$p.HostPort}}</span></td>
              <td><strong>{{$p.ContainerName}}</strong><div class="small">{{$p.ContainerID}}</div></td>
              <td>
                {{if eq $p.State "running"}}
                  <span class="state-running">running</span>
                {{else}}
                  <span class="state-other">{{$p.State}}</span>
                {{end}}
              </td>
            </tr>
            {{end}}
          {{else}}
            <tr><td colspan="9" class="small">No port mappings found. Containers may not have published ports.</td></tr>
          {{end}}
        </tbody>
      </table>
    </div>

    {{if .Networks}}
    <div class="panel">
      <h3>Docker Networks</h3>
      <div class="net-grid">
        {{range .Networks}}
        <div class="net-card">
          <h4>
            <svg class="icon" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2z"/><path d="M2 12h20"/></svg>
            {{.Name}}
            <span class="net-driver">{{.Driver}}</span>
          </h4>
          <div class="net-meta">
            {{if .Subnet}}<span>Subnet: <strong>{{.Subnet}}</strong></span>{{end}}
            {{if .Gateway}}<span>Gateway: <strong>{{.Gateway}}</strong></span>{{end}}
            <span>Scope: {{.Scope}}</span>
          </div>
          {{if .Containers}}
          <div class="net-containers">
            <table>
              <thead><tr><th>Container</th><th>Internal IP</th></tr></thead>
              <tbody>
                {{range .Containers}}
                <tr>
                  <td><strong>{{.Name}}</strong> <span class="small">{{.ID}}</span></td>
                  <td><span class="ip-internal">{{.InternalIP}}</span></td>
                </tr>
                {{end}}
              </tbody>
            </table>
          </div>
          {{else}}
          <div class="small">No containers connected</div>
          {{end}}
        </div>
        {{end}}
      </div>
    </div>
    {{end}}

  </div>
</body>
</html>
`

const runbooksHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Runbooks — DockPilot</title>
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600;700&display=swap" rel="stylesheet">
  <style>
    :root {
      --bg:#0a0e1a; --surface:#111827; --border:#1f2937; --accent:#3b82f6;
      --success:#10b981; --warning:#f59e0b; --danger:#ef4444;
      --muted:#6b7280; --text:#e5e7eb;
    }
    * { box-sizing:border-box; }
    body { margin:0; background: radial-gradient(1200px 600px at 90% -100px, #1d4ed833 0%, transparent 60%), var(--bg); color:var(--text); font-family:'JetBrains Mono', ui-monospace, monospace; }
    .wrap { width:100%; max-width:none; margin:0; padding:20px 24px; }
    .header { display:flex; justify-content:space-between; align-items:center; border:1px solid var(--border); background:var(--surface); border-radius:12px; padding:14px 16px; margin-bottom:14px; }
    .title { font-size:22px; font-weight:700; letter-spacing:.4px; line-height:1.1; }
    .title-row { display:flex; align-items:center; gap:10px; }
    .cockpit-badge { font-size:11px; color:var(--muted); background:var(--surface); border:1px solid var(--border); border-radius:6px; padding:2px 8px; line-height:1.2; }
    .badge { color:var(--muted); border:1px solid var(--border); border-radius:999px; padding:3px 10px; font-size:12px; }
    .icon { width:18px; height:18px; display:inline-block; color:var(--accent); }
    .icon.live { color:var(--success); }
    .small { font-size:12px; color:var(--muted); }
    .panel { background:var(--surface); border:1px solid var(--border); border-radius:12px; padding:14px; margin-bottom:14px; }
    .msg { padding:10px; border-radius:8px; margin:0 0 10px 0; font-size:13px; }
    .ok { background:#022c22; border:1px solid #065f46; }
    .err { background:#3f0d15; border:1px solid #7f1d1d; }
    .grid { display:grid; grid-template-columns: 360px 1fr; gap:14px; align-items:start; }
    .rb-list { display:flex; flex-direction:column; gap:8px; }
    .rb-card { background:#0f172a; border:1px solid var(--border); border-radius:10px; padding:12px; text-decoration:none; color:var(--text); display:block; transition: border-color .15s, transform .15s; }
    .rb-card:hover { border-color:var(--accent); }
    .rb-card.active { border-color:var(--accent); box-shadow: 0 0 0 1px var(--accent); }
    .rb-card-head { display:flex; align-items:center; gap:8px; margin-bottom:6px; }
    .rb-cat { font-size:10px; text-transform:uppercase; letter-spacing:.5px; color:var(--muted); }
    .rb-risk { font-size:10px; font-weight:700; text-transform:uppercase; padding:2px 8px; border-radius:999px; margin-left:auto; }
    .rb-risk-low    { background:#022c22; border:1px solid #065f46; color:var(--success); }
    .rb-risk-medium { background:#3a2c08; border:1px solid #78540c; color:var(--warning); }
    .rb-risk-high   { background:#3f0d15; border:1px solid #7f1d1d; color:var(--danger); }
    .rb-title { font-weight:700; font-size:14px; }
    .rb-desc { color:var(--muted); font-size:12px; margin-top:4px; line-height:1.45; }
    .rb-meta { color:var(--muted); font-size:11px; margin-top:6px; }
    .rb-detail-head { display:flex; align-items:center; gap:10px; margin-bottom:6px; }
    .rb-step { border:1px solid var(--border); background:#0f172a; border-radius:10px; padding:12px; margin-bottom:10px; }
    .rb-step.executed { border-color:#065f46; }
    .rb-step.failed { border-color:#7f1d1d; }
    .rb-step-row { display:flex; gap:10px; align-items:center; }
    .rb-step-num { width:26px; height:26px; border-radius:50%; background:var(--bg); border:1px solid var(--border); display:inline-flex; align-items:center; justify-content:center; font-weight:700; font-size:12px; color:var(--accent); }
    .rb-step-label { font-weight:600; font-size:13px; }
    .rb-step-cmd { color:var(--muted); font-size:11px; margin-top:4px; }
    .rb-step-output { margin-top:8px; background:#0b1324; border:1px solid var(--border); border-radius:8px; padding:10px; font-size:12px; white-space:pre-wrap; word-break:break-word; max-height:260px; overflow:auto; }
    .rb-step-output.err { border-color:#7f1d1d; background:#1a0710; }
    button, input { font-family:inherit; border-radius:8px; border:1px solid var(--border); background:#0f172a; color:var(--text); padding:8px 12px; cursor:pointer; font-size:12px; }
    .btn-primary { background:var(--accent); border-color:var(--accent); color:white; font-weight:700; }
    .btn-good { background:var(--success); border-color:var(--success); color:#07140f; font-weight:700; }
    .btn-danger { background:var(--danger); border-color:var(--danger); color:#fff; font-weight:700; }
    .btn-ai { background:linear-gradient(135deg,#7c3aed,#2563eb); border-color:#7c3aed; color:#fff; font-weight:700; }
    .btn-ai:hover { opacity:0.92; }
    .rb-ai { margin:0 0 18px; border:1px solid #4c1d95; border-radius:10px; background:linear-gradient(180deg,rgba(124,58,237,0.12),rgba(15,23,42,0.4)); padding:14px; }
    .rb-ai-head { display:flex; align-items:center; gap:10px; margin-bottom:10px; }
    .rb-ai-title { font-weight:700; color:#c4b5fd; font-size:13px; }
    .rb-ai-model { margin-left:auto; font-size:11px; color:var(--muted); }
    .rb-ai-body { background:#0b1324; border:1px solid var(--border); border-radius:8px; padding:12px; font-size:12.5px; line-height:1.55; white-space:pre-wrap; word-break:break-word; max-height:420px; overflow:auto; color:var(--text); }
    .rb-ai-note { margin-top:8px; font-size:11px; color:var(--muted); }
    .rb-prop-rationale { font-size:12.5px; color:var(--text); background:#0b1324; border:1px solid var(--border); border-radius:8px; padding:10px 12px; margin-bottom:12px; line-height:1.5; }
    .rb-diff { display:grid; grid-template-columns:1fr 1fr; gap:10px; }
    .rb-diff-col { background:#0b1324; border:1px solid var(--border); border-radius:8px; padding:10px; }
    .rb-diff-head { font-size:11px; font-weight:700; text-transform:uppercase; letter-spacing:.5px; margin-bottom:8px; padding-bottom:6px; border-bottom:1px solid var(--border); }
    .rb-diff-current { color:var(--muted); }
    .rb-diff-new { color:#86efac; }
    .rb-diff-step { display:flex; gap:8px; padding:6px 0; border-bottom:1px solid rgba(255,255,255,0.04); }
    .rb-diff-num { flex:0 0 auto; width:20px; height:20px; border-radius:50%; background:var(--surface); border:1px solid var(--border); font-size:11px; display:flex; align-items:center; justify-content:center; color:var(--muted); }
    .rb-diff-label { font-size:12px; color:var(--text); }
    .rb-diff-cmd { font-size:11px; color:var(--muted); margin-top:2px; word-break:break-word; }
    .rb-prop-actions { display:flex; gap:10px; margin-top:12px; align-items:center; }
    @media (max-width: 760px) { .rb-diff { grid-template-columns:1fr; } }
    .rb-actions { display:flex; gap:8px; margin-bottom:14px; flex-wrap:wrap; }
    .rb-empty { padding:40px; text-align:center; color:var(--muted); }
    .rb-empty-title { font-size:16px; color:var(--text); font-weight:700; margin-bottom:8px; }
    .back-link { color:var(--accent); text-decoration:none; font-size:12px; }
    .back-link:hover { text-decoration:underline; }
    @media (max-width: 980px) { .wrap { padding:14px; } .grid { grid-template-columns:1fr; } }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="header">
      <div>
        <div class="title-row">
          <img class="icon" src="/static/icons/layers.svg" alt="layers" />
          <div class="title">DockPilot Runbooks</div>
          <div class="cockpit-badge">Automated Workflows</div>
        </div>
        <div class="small"><a class="back-link" href="/">← Dashboard</a> &nbsp; | &nbsp; Pre-built Docker operations you can run step-by-step or end-to-end</div>
      </div>
      <div class="badge"><img class="icon live" src="/static/icons/activity.svg" alt="live" /> Socket: {{.DockerHost}} | {{.Now}}</div>
    </div>

    {{if .Success}}<div class="msg ok">{{.Success}}</div>{{end}}
    {{if .Error}}<div class="msg err">{{.Error}}</div>{{end}}

    <div class="grid">
      <div class="rb-list">
        {{range .Runbooks}}
        <a class="rb-card {{if .Selected}}active{{end}}" href="/runbooks?id={{.ID}}">
          <div class="rb-card-head">
            <span class="rb-cat">{{.Category}}</span>
            <span class="rb-risk rb-risk-{{.Risk}}">{{.Risk}} risk</span>
          </div>
          <div class="rb-title">{{.Name}}</div>
          <div class="rb-desc">{{.Description}}</div>
          <div class="rb-meta">{{len .Steps}} steps</div>
        </a>
        {{end}}
      </div>

      <div class="panel">
        {{if .Selected}}
        <div class="rb-detail-head">
          <span class="rb-cat">{{.Selected.Category}}</span>
          <div class="rb-title" style="font-size:16px;">{{.Selected.Name}}</div>
          <span class="rb-risk rb-risk-{{.Selected.Risk}}" style="margin-left:auto;">{{.Selected.Risk}} risk</span>
        </div>
        <div class="rb-desc" style="margin-bottom:14px;">{{.Selected.Description}}</div>

        <div class="rb-actions">
          <form method="post" action="/runbooks/execute" onsubmit="return confirm('Run ALL {{len .Selected.Steps}} steps of {{.Selected.Name}}?');">
            <input type="hidden" name="runbook_id" value="{{.Selected.ID}}" />
            <input type="hidden" name="mode" value="all" />
            <button class="btn-primary" type="submit">▶ Run All Steps</button>
          </form>
          <form method="post" action="/runbooks/analyze">
            <input type="hidden" name="runbook_id" value="{{.Selected.ID}}" />
            <button class="btn-ai" type="submit">✦ Analyze with AI</button>
          </form>
          <form method="post" action="/runbooks/propose">
            <input type="hidden" name="runbook_id" value="{{.Selected.ID}}" />
            <button class="btn-ai" type="submit">✦ Propose Improved Steps</button>
          </form>
          <a class="back-link" href="/runbooks?id={{.Selected.ID}}" style="align-self:center;">Reset</a>
        </div>

        {{if .AIAnalysis}}
        <div class="rb-ai">
          <div class="rb-ai-head">
            <span class="rb-ai-title">✦ AI Analysis</span>
            <span class="rb-ai-model">Model: {{.AIModel}}</span>
          </div>
          <pre class="rb-ai-body">{{.AIAnalysis}}</pre>
          <div class="rb-ai-note">AI-generated suggestions — review before applying. Runbook steps are not modified automatically.</div>
        </div>
        {{end}}

        {{if .Proposal}}
        <div class="rb-ai rb-proposal">
          <div class="rb-ai-head">
            <span class="rb-ai-title">✦ Proposed Steps</span>
            <span class="rb-ai-model">Model: {{.AIModel}}</span>
          </div>
          <div class="rb-prop-rationale">{{.Proposal.Rationale}}</div>
          <div class="rb-diff">
            <div class="rb-diff-col">
              <div class="rb-diff-head rb-diff-current">Current ({{len .Proposal.CurrentSteps}})</div>
              {{range $i, $s := .Proposal.CurrentSteps}}
              <div class="rb-diff-step"><span class="rb-diff-num">{{add $i 1}}</span><div><div class="rb-diff-label">{{$s.Label}}</div><div class="rb-diff-cmd">$ docker {{$s.Command}}</div></div></div>
              {{end}}
            </div>
            <div class="rb-diff-col">
              <div class="rb-diff-head rb-diff-new">Proposed ({{len .Proposal.NewSteps}})</div>
              {{range $i, $s := .Proposal.NewSteps}}
              <div class="rb-diff-step"><span class="rb-diff-num">{{add $i 1}}</span><div><div class="rb-diff-label">{{$s.Label}}</div><div class="rb-diff-cmd">$ docker {{$s.Command}}</div></div></div>
              {{end}}
            </div>
          </div>
          <div class="rb-prop-actions">
            <form method="post" action="/runbooks/apply" onsubmit="return confirm('Replace the steps of this runbook with the {{len .Proposal.NewSteps}} AI-proposed steps?');">
              <input type="hidden" name="runbook_id" value="{{.Proposal.RunbookID}}" />
              <input type="hidden" name="steps" value="{{.Proposal.EncodedSteps}}" />
              <button class="btn-good" type="submit">✓ Apply Proposed Steps</button>
            </form>
            <a class="back-link" href="/runbooks?id={{.Proposal.RunbookID}}" style="align-self:center;">Discard</a>
          </div>
          <div class="rb-ai-note">Applying replaces this runbook's steps for the current session. Review the commands above before applying.</div>
        </div>
        {{end}}

        {{range .Selected.Results}}
        <div class="rb-step {{if .Executed}}{{if .Err}}failed{{else}}executed{{end}}{{end}}">
          <div class="rb-step-row">
            <span class="rb-step-num">{{add .Index 1}}</span>
            <div style="flex:1;">
              <div class="rb-step-label">{{.Label}}</div>
              <div class="rb-step-cmd">$ docker {{.Command}}</div>
            </div>
            <form method="post" action="/runbooks/execute" style="margin:0;">
              <input type="hidden" name="runbook_id" value="{{$.Selected.ID}}" />
              <input type="hidden" name="mode" value="step" />
              <input type="hidden" name="step" value="{{.Index}}" />
              <button class="btn-good" type="submit">{{if .Executed}}Re-run{{else}}Execute{{end}}</button>
            </form>
          </div>
          {{if .Executed}}
            {{if .Err}}
              <div class="rb-step-output err">ERROR: {{.Err}}{{if .Output}}

{{.Output}}{{end}}</div>
            {{else}}
              <div class="rb-step-output">{{if .Output}}{{.Output}}{{else}}(no output){{end}}</div>
            {{end}}
          {{end}}
        </div>
        {{end}}
        {{else}}
        <div class="rb-empty">
          <div class="rb-empty-title">Select a Runbook</div>
          <div class="small">Choose a pre-built workflow from the list to inspect its steps and run them individually or all at once. Runbooks cover cleanup, diagnostics, and recovery.</div>
        </div>
        {{end}}
      </div>
    </div>
  </div>
</body>
</html>
`
