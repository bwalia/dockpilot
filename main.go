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
	"sort"
	"strings"
	"time"
)

type App struct {
	tmpl *template.Template
}

type PageData struct {
	Containers []ContainerView
	Total      int
	Running    int
	Stopped    int
	Images     int
	Search     string
	CommandInput  string
	CommandOutput string
	AIPrompt       string
	AISuggestion   string
	AIExplanation  string
	AIModel        string
	Error      string
	Success    string
	Now        string
	DockerHost string
}

type AISuggestion struct {
	Command     string `json:"command"`
	Explanation string `json:"explanation"`
}

type ContainerView struct {
	ID      string
	Name    string
	Image   string
	Status  string
	State   string
	Created string
	Ports   string
}

func main() {
	app := &App{tmpl: template.Must(template.New("index").Parse(indexHTML))}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
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
		a.render(w, PageData{Error: err.Error()})
		return
	}

	d.Success = r.URL.Query().Get("success")
	d.Error = r.URL.Query().Get("error")
	a.render(w, d)
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
		Containers: containers,
		Total:      len(containers),
		Running:    running,
		Stopped:    stopped,
		Images:     countImages(),
		Search:     search,
		CommandInput:  "",
		CommandOutput: "",
		AIPrompt:      "",
		AISuggestion:  "",
		AIExplanation: "",
		AIModel:       envOrDefault("OLLAMA_MODEL", "llama3"),
		Now:        time.Now().Format("2006-01-02 15:04:05"),
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
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
	
	http.Redirect(w, r, "/?success=container+"+action+"+ok", http.StatusSeeOther)
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
    .wrap { max-width:1200px; margin:0 auto; padding:20px; }
    .header {
      display:flex; justify-content:space-between; align-items:center;
      border:1px solid var(--border); background:var(--surface); border-radius:12px;
      padding:14px 16px; margin-bottom:14px;
    }
    .title { font-weight:700; letter-spacing:.3px; }
	.title-row { display:flex; align-items:center; gap:10px; }
    .badge { color:var(--muted); border:1px solid var(--border); border-radius:999px; padding:3px 10px; font-size:12px; }
    .kpis { display:grid; grid-template-columns:repeat(4,minmax(0,1fr)); gap:10px; margin-bottom:14px; }
    .kpi { background:var(--surface); border:1px solid var(--border); border-radius:10px; padding:12px; }
	.kpi .label { color:var(--muted); font-size:12px; display:flex; align-items:center; gap:6px; }
    .kpi .value { font-size:22px; font-weight:700; margin-top:6px; }
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
		.actions-col { width:170px; }
		.actions {
			display:grid;
			grid-template-columns:repeat(4, 34px);
			gap:6px;
			align-items:center;
			justify-content:start;
			min-width:152px;
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
      .kpis { grid-template-columns:repeat(2,minmax(0,1fr)); }
      .grid { grid-template-columns:1fr; }
    }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="header">
      <div>
		<div class="title-row"><img class="icon" src="/static/icons/layers.svg" alt="layers" /><div class="title">DockPilot Admin</div></div>
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

    <div class="panel">
      <h3>Containers</h3>
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Image</th>
            <th>State</th>
            <th>Ports</th>
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
              <td>{{.Created}}</td>
              <td class="actions">
								<form method="post" action="/containers/{{.ID}}/start">
									<button class="btn-good btn-icon tip" type="submit" data-tip="Start" title="Start" aria-label="Start {{.Name}}">
										<svg viewBox="0 0 24 24"><path d="M8 5v14l11-7z"/></svg>
									</button>
								</form>
								<form method="post" action="/containers/{{.ID}}/stop">
									<button class="btn-warn btn-icon tip" type="submit" data-tip="Stop" title="Stop" aria-label="Stop {{.Name}}">
										<svg viewBox="0 0 24 24"><rect x="7" y="7" width="10" height="10" rx="1"/></svg>
									</button>
								</form>
								<form method="post" action="/containers/{{.ID}}/restart">
									<button class="btn-primary btn-icon tip" type="submit" data-tip="Restart" title="Restart" aria-label="Restart {{.Name}}">
										<svg viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/></svg>
									</button>
								</form>
								<form method="post" action="/containers/{{.ID}}/remove" onsubmit="return confirm('Remove container {{.Name}}?')">
									<button class="btn-danger btn-icon tip" type="submit" data-tip="Remove" title="Remove" aria-label="Remove {{.Name}}">
										<svg viewBox="0 0 24 24"><path d="M3 6h18"/><path d="M8 6V4h8v2"/><path d="M7 6l1 14h8l1-14"/></svg>
									</button>
								</form>
              </td>
            </tr>
            {{end}}
          {{else}}
            <tr><td colspan="6" class="small">No containers found.</td></tr>
          {{end}}
        </tbody>
      </table>
    </div>
  </div>
</body>
</html>
`
