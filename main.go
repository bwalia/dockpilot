package main

import (
	"bytes"
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
	"time"
)

type App struct {
	tmpl         *template.Template
	ipamTmpl     *template.Template
	runbooksTmpl *template.Template
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
}

type RunbookStepResult struct {
	Index     int
	Label     string
	Command   string
	Output    string
	Err       string
	Executed  bool
}

type RunbookView struct {
	Runbook
	Selected bool
	Results  []RunbookStepResult
}

type RunbooksData struct {
	Runbooks    []RunbookView
	Selected    *RunbookView
	Now         string
	DockerHost  string
	Success     string
	Error       string
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
	CPUPercent    float64
	CPUCores      int
	MemUsedBytes  int64
	MemTotalBytes int64
	MemPercent    float64
	DiskUsedBytes int64
	DiskTotalBytes int64
	DiskPercent   float64
	CPUUsedLabel  string
	CPUTotalLabel string
	MemUsedLabel  string
	MemTotalLabel string
	DiskUsedLabel string
	DiskTotalLabel string
	Error         string
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
}

func main() {
	funcMap := template.FuncMap{"add": func(a, b int) int { return a + b }}
	app := &App{
		tmpl:         template.Must(template.New("index").Parse(indexHTML)),
		ipamTmpl:     template.Must(template.New("ipam").Funcs(funcMap).Parse(ipamHTML)),
		runbooksTmpl: template.Must(template.New("runbooks").Funcs(funcMap).Parse(runbooksHTML)),
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
	mux.HandleFunc("/landing", app.handleLanding)
	mux.HandleFunc("/ipam", app.handleIPAM)
	mux.HandleFunc("/runbooks", app.handleRunbooks)
	mux.HandleFunc("/runbooks/execute", app.handleRunbookExecute)
	mux.HandleFunc("/", app.handleDashboard)
	mux.HandleFunc("/docker/exec", app.handleDockerCommand)
	mux.HandleFunc("/ai/interpret", app.handleAIInterpret)
	mux.HandleFunc("/containers/create", app.handleCreate)
	mux.HandleFunc("/containers/", app.handleContainerAction)

	addr := envOrDefault("ADDR", ":8090")
	log.Printf("dockpilot listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, app.withBasicAuth(withLogging(mux))))
}

func (a *App) withBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	search := strings.TrimSpace(r.URL.Query().Get("q"))
	d, err := a.buildDashboardData(search)
	if err != nil {
		a.render(w, PageData{Error: err.Error(), Usage: buildHostUsage(nil, nil)})
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

func findRunbook(id string) *Runbook {
	for i := range runbookCatalog {
		if runbookCatalog[i].ID == id {
			return &runbookCatalog[i]
		}
	}
	return nil
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
	}
	for _, rb := range runbookCatalog {
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

	rb := findRunbook(runbookID)
	if rb == nil {
		http.Error(w, "runbook not found: "+runbookID, http.StatusNotFound)
		return
	}

	data := RunbooksData{
		Now:        time.Now().Format("2006-01-02 15:04:05"),
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
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

	for _, cat := range runbookCatalog {
		view := RunbookView{Runbook: cat, Selected: cat.ID == rb.ID}
		if view.Selected {
			view.Results = results
			data.Selected = &view
		}
		data.Runbooks = append(data.Runbooks, view)
	}
	a.renderRunbooks(w, data)
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

	containers, err := listContainers()
	if err != nil {
		return PageData{}, err
	}

	stats := collectContainerStats()
	for i := range containers {
		if s, ok := stats[containers[i].ID]; ok {
			containers[i].CPUPerc = s.cpuPerc
			containers[i].MemUsage = s.memUsage
			containers[i].MemPerc = s.memPerc
		}
	}

	usage := buildHostUsage(containers, stats)

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
		Images:        countImages(),
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

func interpretDockerCommandWithOllama(userPrompt string) (AISuggestion, error) {
	baseURL := strings.TrimRight(envOrDefault("OLLAMA_BASE_URL", "http://localhost:11434/v1"), "/")
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

func buildHostUsage(containers []ContainerView, stats map[string]containerStat) HostUsage {
	var (
		cpuSum   float64
		memSum   int64
	)
	for _, c := range containers {
		if s, ok := stats[c.ID]; ok {
			cpuSum += s.cpuFloat
			memSum += s.memBytes
		}
	}

	cpus := dockerInfoCPUs()
	memTotal := dockerInfoTotalMem()
	diskUsed, diskTotal := dockerSystemDFSize()

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

func runDocker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
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

func (a *App) handleIPAM(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ipam" {
		http.NotFound(w, r)
		return
	}

	search := strings.TrimSpace(r.URL.Query().Get("q"))
	data := IPAMData{
		Now:        time.Now().Format("2006-01-02 15:04:05"),
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
	}

	// Fetch networks
	networks, netErr := listDockerNetworks()
	if netErr == nil {
		data.Networks = networks
	}

	mappings, err := listPortMappings()
	if err != nil {
		data.Error = fmt.Sprintf("failed to list port mappings: %v", err)
	} else {
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
	}

	type ipamDataWithSearch struct {
		IPAMData
		Search string
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.ipamTmpl.Execute(w, ipamDataWithSearch{data, search}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
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
	<title>DockPilot</title>
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
			font-size:11px;
			color:var(--muted);
			background:var(--surface);
			border:1px solid var(--border);
			border-radius:6px;
			padding:2px 8px;
			line-height:1.2;
		}
    .badge { color:var(--muted); border:1px solid var(--border); border-radius:999px; padding:3px 10px; font-size:12px; }
    .kpis { display:grid; grid-template-columns:repeat(4,minmax(0,1fr)); gap:10px; margin-bottom:14px; }
    .kpi { background:var(--surface); border:1px solid var(--border); border-radius:10px; padding:12px; }
	.kpi .label { color:var(--muted); font-size:12px; display:flex; align-items:center; gap:6px; }
    .kpi .value { font-size:22px; font-weight:700; margin-top:6px; }
    .usage-row { display:grid; grid-template-columns:repeat(3,minmax(0,1fr)); gap:10px; margin-bottom:14px; }
    .usage-card { background:var(--surface); border:1px solid var(--border); border-radius:10px; padding:14px; display:flex; gap:14px; align-items:center; }
    .usage-donut { position:relative; width:84px; height:84px; flex-shrink:0; }
    .usage-donut svg { transform:rotate(-90deg); width:100%; height:100%; }
    .usage-donut .track { fill:none; stroke:var(--border); stroke-width:10; }
    .usage-donut .arc { fill:none; stroke-width:10; stroke-linecap:round; transition: stroke-dashoffset .4s ease; }
    .usage-donut .arc-cpu { stroke:var(--accent); }
    .usage-donut .arc-mem { stroke:var(--success); }
    .usage-donut .arc-disk { stroke:var(--warning); }
    .usage-donut .pct { position:absolute; inset:0; display:flex; align-items:center; justify-content:center; font-size:14px; font-weight:700; color:var(--text); }
    .usage-meta { display:flex; flex-direction:column; gap:3px; min-width:0; }
    .usage-meta .title { font-size:12px; color:var(--muted); text-transform:uppercase; letter-spacing:.5px; }
    .usage-meta .used { font-size:18px; font-weight:700; }
    .usage-meta .total { font-size:11px; color:var(--muted); }
    .usage-bar { margin-top:6px; height:6px; background:var(--border); border-radius:999px; overflow:hidden; }
    .usage-bar > span { display:block; height:100%; border-radius:999px; }
    .usage-bar .fill-cpu { background:var(--accent); }
    .usage-bar .fill-mem { background:var(--success); }
    .usage-bar .fill-disk { background:var(--warning); }
    .metric-cell { font-variant-numeric:tabular-nums; }
    .metric-bar { margin-top:4px; height:4px; background:var(--border); border-radius:999px; overflow:hidden; width:72px; }
    .metric-bar > span { display:block; height:100%; }
	.icon { width:18px; height:18px; display:inline-block; color:var(--accent); }
	.icon.live { color:var(--success); }
	.search { display:flex; gap:8px; align-items:center; margin-bottom:14px; }
	.search input { flex:1; min-width:220px; }
	.cmd-run { background:var(--surface); border:1px solid var(--border); border-radius:12px; padding:14px; margin-bottom:14px; }
	.cmd-row { display:flex; gap:8px; align-items:center; }
	.cmd-prefix { border:1px solid var(--border); background:#0b1324; color:var(--muted); border-radius:8px; padding:10px 12px; font-size:13px; }
	.cmd-run input { flex:1; min-width:200px; }
	.cmd-output { margin-top:10px; background:#0b1324; border:1px solid var(--border); border-radius:8px; padding:10px; font-size:12px; white-space:pre-wrap; word-break:break-word; }
	.ai-run { background:var(--surface); border:1px solid var(--border); border-radius:12px; padding:14px; margin-bottom:14px; }
	.ai-run textarea { width:100%; min-height:72px; resize:vertical; }
	.ai-meta { margin-top:8px; font-size:12px; color:var(--muted); }
    .panel { background:var(--surface); border:1px solid var(--border); border-radius:12px; padding:14px; margin-bottom:14px; }
    .grid { display:grid; grid-template-columns:2fr 3fr; gap:14px; }
    .row { display:flex; gap:8px; flex-wrap:wrap; }
    input, button {
      font-family:inherit; border-radius:8px; border:1px solid var(--border);
      background:#0f172a; color:var(--text); padding:10px 11px;
    }
    input { min-width:140px; }
    button { cursor:pointer; }
    .btn-primary { background:var(--accent); border-color:var(--accent); color:white; font-weight:600; }
    .btn-good { background:var(--success); border-color:var(--success); color:#07140f; font-weight:700; }
    .btn-warn { background:var(--warning); border-color:var(--warning); color:#1a1200; font-weight:700; }
    .btn-danger { background:var(--danger); border-color:var(--danger); color:#fff; font-weight:700; }
    .msg { padding:10px; border-radius:8px; margin:0 0 10px 0; font-size:13px; }
    .ok { background:#022c22; border:1px solid #065f46; }
    .err { background:#3f0d15; border:1px solid #7f1d1d; }
    table { width:100%; border-collapse:collapse; font-size:13px; }
    th, td { padding:10px 8px; border-bottom:1px solid var(--border); text-align:left; vertical-align:top; }
    th { color:var(--muted); font-weight:600; }
    .state-running { color:var(--success); font-weight:700; }
    .state-other { color:var(--warning); font-weight:700; }
		.actions-col { width:255px; }
		.actions {
			display:grid;
			grid-template-columns:repeat(6, 34px);
			gap:6px;
			align-items:center;
			justify-content:start;
			min-width:232px;
		}
		.actions form { margin:0; }
		.btn-icon {
			width:34px;
			height:34px;
			padding:0;
			display:inline-flex;
			align-items:center;
			justify-content:center;
			position:relative;
		}
		.btn-icon svg {
			width:15px;
			height:15px;
			fill:none;
			stroke:currentColor;
			stroke-width:2;
			stroke-linecap:round;
			stroke-linejoin:round;
			pointer-events:none;
		}
		.tip:hover::after {
			content:attr(data-tip);
			position:absolute;
			bottom:calc(100% + 6px);
			left:50%;
			transform:translateX(-50%);
			white-space:nowrap;
			background:#0b1324;
			border:1px solid var(--border);
			color:var(--text);
			font-size:11px;
			padding:4px 7px;
			border-radius:6px;
			z-index:10;
		}
    .small { font-size:12px; color:var(--muted); }
    @media (max-width: 980px) {
			.wrap { padding:14px; }
      .kpis { grid-template-columns:repeat(2,minmax(0,1fr)); }
      .usage-row { grid-template-columns:1fr; }
      .grid { grid-template-columns:1fr; }
    }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="header">
      <div>
		<div class="title-row"><img class="icon" src="/static/icons/layers.svg" alt="layers" /><div class="title">DockPilot</div><div class="cockpit-badge">Docker Cockpit</div></div>
        <div class="small">KubePilot-style dashboard for host container operations</div>
      </div>
			<div class="badge"><img class="icon live" src="/static/icons/activity.svg" alt="live" /> Socket: {{.DockerHost}} | {{.Now}}</div>
    </div>

    {{if .Success}}<div class="msg ok">{{.Success}}</div>{{end}}
    {{if .Error}}<div class="msg err">{{.Error}}</div>{{end}}

		<form class="search" method="get" action="/">
			<input name="q" value="{{.Search}}" placeholder="Search by name, id, image, status or ports" />
			<button class="btn-primary" type="submit">Search</button>
			{{if .Search}}<a class="small" href="/">clear</a>{{end}}
		</form>

		<div class="ai-run">
			<h3>AI Command Assistant (Local Ollama)</h3>
			<form method="post" action="/ai/interpret">
				<input type="hidden" name="q" value="{{.Search}}" />
				<textarea name="ai_prompt" placeholder="Example: clean unused images and stopped containers safely">{{.AIPrompt}}</textarea>
				<div class="row" style="margin-top:8px; align-items:center;">
					<button class="btn-primary" type="submit">Suggest Docker Command</button>
					<div class="ai-meta">Model: {{.AIModel}} (set OLLAMA_BASE_URL env if needed)</div>
				</div>
			</form>
			{{if .AISuggestion}}<div class="cmd-output">Suggested: docker {{.AISuggestion}}{{if .AIExplanation}}\n\nWhy: {{.AIExplanation}}{{end}}</div>{{end}}
		</div>

		<div class="cmd-run">
			<h3>Run Docker Command</h3>
			<form method="post" action="/docker/exec">
				<input type="hidden" name="q" value="{{.Search}}" />
				<div class="cmd-row">
					<div class="cmd-prefix">docker</div>
					<input name="command" value="{{.CommandInput}}" placeholder="system prune -f" />
					<button class="btn-primary" type="submit">Run</button>
				</div>
			</form>
			{{if .CommandOutput}}<div class="cmd-output">{{.CommandOutput}}</div>{{end}}
		</div>

    <div class="usage-row">
      {{with .Usage}}
      <div class="usage-card">
        <div class="usage-donut">
          <svg viewBox="0 0 36 36">
            <circle class="track" cx="18" cy="18" r="15.9155"/>
            <circle class="arc arc-cpu" cx="18" cy="18" r="15.9155" stroke-dasharray="{{printf "%.2f" .CPUPercent}} 100" stroke-dashoffset="0"/>
          </svg>
          <div class="pct">{{printf "%.0f" .CPUPercent}}%</div>
        </div>
        <div class="usage-meta">
          <div class="title">CPU</div>
          <div class="used">{{.CPUUsedLabel}}</div>
          <div class="total">of {{.CPUTotalLabel}}</div>
          <div class="usage-bar"><span class="fill-cpu" style="width:{{printf "%.2f" .CPUPercent}}%"></span></div>
        </div>
      </div>

      <div class="usage-card">
        <div class="usage-donut">
          <svg viewBox="0 0 36 36">
            <circle class="track" cx="18" cy="18" r="15.9155"/>
            <circle class="arc arc-mem" cx="18" cy="18" r="15.9155" stroke-dasharray="{{printf "%.2f" .MemPercent}} 100" stroke-dashoffset="0"/>
          </svg>
          <div class="pct">{{printf "%.0f" .MemPercent}}%</div>
        </div>
        <div class="usage-meta">
          <div class="title">Memory</div>
          <div class="used">{{.MemUsedLabel}}</div>
          <div class="total">of {{.MemTotalLabel}}</div>
          <div class="usage-bar"><span class="fill-mem" style="width:{{printf "%.2f" .MemPercent}}%"></span></div>
        </div>
      </div>

      <div class="usage-card">
        <div class="usage-donut">
          <svg viewBox="0 0 36 36">
            <circle class="track" cx="18" cy="18" r="15.9155"/>
            <circle class="arc arc-disk" cx="18" cy="18" r="15.9155" stroke-dasharray="{{printf "%.2f" .DiskPercent}} 100" stroke-dashoffset="0"/>
          </svg>
          <div class="pct">{{printf "%.0f" .DiskPercent}}%</div>
        </div>
        <div class="usage-meta">
          <div class="title">Storage (Docker)</div>
          <div class="used">{{.DiskUsedLabel}}</div>
          <div class="total">images + containers + volumes</div>
          <div class="usage-bar"><span class="fill-disk" style="width:{{printf "%.2f" .DiskPercent}}%"></span></div>
        </div>
      </div>
      {{end}}
    </div>

    <div class="kpis">
			<div class="kpi"><div class="label"><img class="icon" src="/static/icons/cpu.svg" alt="cpu" /> Total Containers</div><div class="value">{{.Total}}</div></div>
			<div class="kpi"><div class="label"><img class="icon live" src="/static/icons/activity.svg" alt="running" /> Running</div><div class="value">{{.Running}}</div></div>
			<div class="kpi"><div class="label"><img class="icon" src="/static/icons/triangle-alert.svg" alt="stopped" /> Stopped</div><div class="value">{{.Stopped}}</div></div>
			<div class="kpi"><div class="label"><img class="icon" src="/static/icons/shield.svg" alt="images" /> Images</div><div class="value">{{.Images}}</div></div>
    </div>

    <div class="grid">
      <div class="panel">
        <h3>Launch Container</h3>
        <form method="post" action="/containers/create">
          <div class="row">
            <input name="name" placeholder="name (optional)" />
            <input name="image" placeholder="image:tag (required)" required />
          </div>
          <div class="row" style="margin-top:8px;">
            <input name="ports" placeholder="ports e.g. 8081:80,8443:443" style="min-width:320px;" />
          </div>
          <div class="row" style="margin-top:8px;">
            <input name="env" placeholder="env e.g. KEY=a,MODE=prod" style="min-width:320px;" />
          </div>
          <div class="row" style="margin-top:8px;">
            <input name="command" placeholder="command args e.g. sleep,3600" style="min-width:320px;" />
          </div>
          <div class="row" style="margin-top:10px; align-items:center;">
            <label class="small"><input type="checkbox" name="auto_start" checked /> auto-start</label>
            <button class="btn-primary" type="submit">Create</button>
          </div>
        </form>
      </div>

      <div class="panel">
        <h3>Quick Notes</h3>
        <ul class="small">
          <li>Use comma-separated values for ports/env/command.</li>
		  <li>Examples: ports 8080:80,8443:443 and env MODE=prod,DEBUG=false.</li>
          <li>Actions are executed with Docker CLI on this host.</li>
        </ul>
      </div>
    </div>

    <div style="margin-bottom:14px; display:flex; gap:10px; flex-wrap:wrap;">
      <a href="/ipam" style="display:inline-flex; align-items:center; gap:8px; background:var(--surface); border:1px solid var(--border); border-radius:10px; padding:10px 18px; color:var(--accent); text-decoration:none; font-weight:600; font-size:14px; transition:border-color .15s;">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 8v4l3 3"/></svg>
        IPAM — IP &amp; Port Manager
      </a>
      <a href="/runbooks" style="display:inline-flex; align-items:center; gap:8px; background:var(--surface); border:1px solid var(--border); border-radius:10px; padding:10px 18px; color:var(--accent); text-decoration:none; font-weight:600; font-size:14px; transition:border-color .15s;">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 11l3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/></svg>
        Runbooks — Automated Workflows
      </a>
    </div>

    <div class="panel">
      <h3>Containers</h3>
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Image</th>
            <th>State</th>
            <th>Ports</th>
            <th>CPU</th>
            <th>Memory</th>
            <th>Age</th>
			<th class="actions-col">Actions</th>
          </tr>
        </thead>
        <tbody>
          {{if .Containers}}
            {{range .Containers}}
            <tr>
              <td><strong>{{.Name}}</strong><div class="small">{{.ID}}</div></td>
              <td>{{.Image}}</td>
              <td>
                {{if eq .State "running"}}
                  <span class="state-running">running</span>
                {{else}}
                  <span class="state-other">{{.State}}</span>
                {{end}}
                <div class="small">{{.Status}}</div>
              </td>
              <td>{{.Ports}}</td>
              <td class="metric-cell">
                {{if .CPUPerc}}
                  {{.CPUPerc}}
                {{else}}
                  <span class="small">—</span>
                {{end}}
              </td>
              <td class="metric-cell">
                {{if .MemUsage}}
                  {{.MemUsage}}
                  <div class="small">{{.MemPerc}}</div>
                {{else}}
                  <span class="small">—</span>
                {{end}}
              </td>
              <td>{{.Created}}</td>
              <td class="actions">
								<form method="post" action="/containers/{{.ID}}/start">
									<input type="hidden" name="q" value="{{$.Search}}" />
									<button class="btn-good btn-icon tip" type="submit" data-tip="Start" title="Start" aria-label="Start {{.Name}}">
										<svg viewBox="0 0 24 24"><path d="M8 5v14l11-7z"/></svg>
									</button>
								</form>
								<form method="post" action="/containers/{{.ID}}/stop">
									<input type="hidden" name="q" value="{{$.Search}}" />
									<button class="btn-warn btn-icon tip" type="submit" data-tip="Stop" title="Stop" aria-label="Stop {{.Name}}">
										<svg viewBox="0 0 24 24"><rect x="7" y="7" width="10" height="10" rx="1"/></svg>
									</button>
								</form>
								<form method="post" action="/containers/{{.ID}}/restart">
									<input type="hidden" name="q" value="{{$.Search}}" />
									<button class="btn-primary btn-icon tip" type="submit" data-tip="Restart" title="Restart" aria-label="Restart {{.Name}}">
										<svg viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/></svg>
									</button>
								</form>
								<form method="post" action="/containers/{{.ID}}/inspect">
									<input type="hidden" name="q" value="{{$.Search}}" />
									<button class="btn-primary btn-icon tip" type="submit" data-tip="Inspect" title="Inspect" aria-label="Inspect {{.Name}}">
										<svg viewBox="0 0 24 24"><circle cx="11" cy="11" r="6"/><path d="M20 20l-4.35-4.35"/></svg>
									</button>
								</form>
								<form method="post" action="/containers/{{.ID}}/logs">
									<input type="hidden" name="q" value="{{$.Search}}" />
									<button class="btn-primary btn-icon tip" type="submit" data-tip="Logs" title="Logs" aria-label="Logs {{.Name}}">
										<svg viewBox="0 0 24 24"><path d="M5 4h14v16H5z"/><path d="M8 8h8"/><path d="M8 12h8"/><path d="M8 16h6"/></svg>
									</button>
								</form>
								<form method="post" action="/containers/{{.ID}}/remove" onsubmit="return confirm('Remove container {{.Name}}?')">
									<input type="hidden" name="q" value="{{$.Search}}" />
									<button class="btn-danger btn-icon tip" type="submit" data-tip="Remove" title="Remove" aria-label="Remove {{.Name}}">
										<svg viewBox="0 0 24 24"><path d="M3 6h18"/><path d="M8 6V4h8v2"/><path d="M7 6l1 14h8l1-14"/></svg>
									</button>
								</form>
              </td>
            </tr>
            {{end}}
          {{else}}
            <tr><td colspan="8" class="small">No containers found.</td></tr>
          {{end}}
        </tbody>
      </table>
    </div>
  </div>
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

		<form class="search" method="get" action="/ipam" style="margin-bottom:18px;">
			<input name="q" value="{{.Search}}" placeholder="Search ports, IPs, container name or state" style="min-width:220px;" />
			<button class="btn-primary" type="submit">Search</button>
			{{if .Search}}<a class="small" href="/ipam">clear</a>{{end}}
		</form>

		{{if .Error}}<div class="msg err">{{.Error}}</div>{{end}}

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
          <a class="back-link" href="/runbooks?id={{.Selected.ID}}" style="align-self:center;">Reset</a>
        </div>

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
