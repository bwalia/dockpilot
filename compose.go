package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// compose.go implements Docker Compose / stack management. For most dev teams a
// stack — a group of services brought up from one compose file — is the unit of
// work, not an individual container. This page lists the compose projects known
// to the daemon, shows each project's services grouped together, and exposes the
// lifecycle verbs (up / pull / restart / stop / down) plus a deploy form that
// brings a stack up from a compose file on the host.
//
// Listing uses `docker compose ls` (Compose v2). Per-service rows are recovered
// from the standard `com.docker.compose.*` container labels so the grouping
// works even for projects that are currently stopped. Mutations run via
// runCompose, which — unlike runDocker's 25s cap — allows several minutes since
// `up`/`pull` legitimately take a while.

// composeCmdTimeout bounds a compose mutation. Pulls and image builds are slow,
// so this is far more generous than dockerCmdTimeout.
const composeCmdTimeout = 8 * time.Minute

// composeStack is one project as shown on the page.
type composeStack struct {
	Name       string
	Status     string
	ConfigFile string // first config file; the handle we drive actions through
	Running    int
	Total      int
	Services   []composeService
}

type composeService struct {
	Service string
	Name    string // container name
	State   string
	Status  string
	Image   string
}

// StacksPage is the view-model for the stacks template.
type StacksPage struct {
	Stacks         []composeStack
	Count          int
	ComposeOK      bool   // is `docker compose` available at all?
	ComposeVersion string
	Error          string
	Success        string
	Now            string
	DockerHost     string
}

func (a *App) handleStacks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/stacks" {
		http.NotFound(w, r)
		return
	}
	page := a.buildStacksPage()
	if v := strings.TrimSpace(r.URL.Query().Get("success")); v != "" {
		page.Success = v
	}
	if v := strings.TrimSpace(r.URL.Query().Get("error")); v != "" {
		page.Error = v
	}
	page.Now = nowString()
	page.DockerHost = envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := stacksTmpl.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) buildStacksPage() StacksPage {
	page := StacksPage{}
	ver, ok := composeVersion()
	page.ComposeOK = ok
	page.ComposeVersion = ver
	if !ok {
		page.Error = "Docker Compose v2 is not available (`docker compose version` failed). Install the compose plugin to manage stacks."
		return page
	}

	stacks, err := listStacks()
	if err != nil {
		page.Error = fmt.Sprintf("failed to list stacks: %v", err)
		return page
	}
	page.Stacks = stacks
	page.Count = len(stacks)
	return page
}

// composeVersion reports whether `docker compose` is usable and its version.
func composeVersion() (string, bool) {
	out, err := runDocker("compose", "version", "--short")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// listStacks merges `docker compose ls` (project status + config file) with the
// per-container compose labels (the service breakdown).
func listStacks() ([]composeStack, error) {
	out, err := runDocker("compose", "ls", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}

	type lsEntry struct {
		Name        string `json:"Name"`
		Status      string `json:"Status"`
		ConfigFiles string `json:"ConfigFiles"`
	}
	var entries []lsEntry
	if s := strings.TrimSpace(out); s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &entries); err != nil {
			return nil, fmt.Errorf("parsing compose ls output: %v", err)
		}
	}

	byProject := composeServicesByProject()

	stacks := make([]composeStack, 0, len(entries))
	for _, e := range entries {
		svcs := byProject[e.Name]
		running := 0
		for _, s := range svcs {
			if s.State == "running" {
				running++
			}
		}
		stacks = append(stacks, composeStack{
			Name:       e.Name,
			Status:     e.Status,
			ConfigFile: firstConfigFile(e.ConfigFiles),
			Running:    running,
			Total:      len(svcs),
			Services:   svcs,
		})
	}
	sort.Slice(stacks, func(i, j int) bool { return stacks[i].Name < stacks[j].Name })
	return stacks, nil
}

// composeServicesByProject groups every container that carries compose labels by
// its project name.
func composeServicesByProject() map[string][]composeService {
	const fmtStr = `{{.Label "com.docker.compose.project"}}` + "\t" +
		`{{.Label "com.docker.compose.service"}}` + "\t" +
		`{{.Names}}` + "\t" + `{{.State}}` + "\t" + `{{.Status}}` + "\t" + `{{.Image}}`
	out, err := runDocker("ps", "-a", "--format", fmtStr)
	if err != nil {
		return nil
	}
	byProject := map[string][]composeService{}
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 6 || strings.TrimSpace(f[0]) == "" {
			continue // not a compose-managed container
		}
		byProject[f[0]] = append(byProject[f[0]], composeService{
			Service: f[1],
			Name:    f[2],
			State:   f[3],
			Status:  f[4],
			Image:   f[5],
		})
	}
	for p := range byProject {
		svcs := byProject[p]
		sort.Slice(svcs, func(i, j int) bool { return svcs[i].Service < svcs[j].Service })
		byProject[p] = svcs
	}
	return byProject
}

func firstConfigFile(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// compose ls joins multiple files with a comma.
	return strings.TrimSpace(strings.Split(s, ",")[0])
}

// ---- action handler ------------------------------------------------------

// handleStacksAction is the POST handler for stack lifecycle verbs and deploys.
func (a *App) handleStacksAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		stacksRedirect(w, r, "invalid form", true)
		return
	}
	action := strings.TrimSpace(r.FormValue("action"))
	file := strings.TrimSpace(r.FormValue("file"))
	project := strings.TrimSpace(r.FormValue("project"))

	args, err := composeCommandArgs(action, file, project)
	if err != nil {
		stacksRedirect(w, r, err.Error(), true)
		return
	}

	if out, err := runCompose(args...); err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		stacksRedirect(w, r, fmt.Sprintf("%s failed: %s", action, firstLine(msg)), true)
		return
	}
	label := project
	if label == "" {
		label = file
	}
	stacksRedirect(w, r, fmt.Sprintf("%s ok: %s", action, label), false)
}

// composeCommandArgs maps an action to compose CLI args, validating the file
// path so it can't be smuggled in as a flag and confirming it exists on disk.
func composeCommandArgs(action, file, project string) ([]string, error) {
	if !validComposeFile(file) {
		return nil, fmt.Errorf("invalid compose file path")
	}
	if _, err := os.Stat(file); err != nil {
		return nil, fmt.Errorf("compose file not found: %s", file)
	}
	base := []string{"compose", "-f", file}
	if project != "" {
		if !validDockerArg(project) {
			return nil, fmt.Errorf("invalid project name")
		}
		base = append(base, "-p", project)
	}

	switch action {
	case "up", "deploy":
		return append(base, "up", "-d", "--remove-orphans"), nil
	case "pull":
		return append(base, "pull"), nil
	case "restart":
		return append(base, "restart"), nil
	case "stop":
		return append(base, "stop"), nil
	case "down":
		return append(base, "down"), nil
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

// validComposeFile permits an absolute or relative path made of the characters
// real compose files use, and rejects anything that looks like a flag.
func validComposeFile(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if r == '/' || r == '.' || r == '_' || r == '-' || r == ' ' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// runCompose runs a compose mutation with a generous timeout and returns the
// combined output, which carries compose's human-readable progress/errors.
func runCompose(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), composeCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("compose command timed out after %s", composeCmdTimeout)
	}
	return string(out), err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func stacksRedirect(w http.ResponseWriter, r *http.Request, msg string, isErr bool) {
	key := "success"
	if isErr {
		key = "error"
	}
	http.Redirect(w, r, "/stacks?"+key+"="+urlQueryEscape(msg), http.StatusSeeOther)
}

var stacksTmpl = template.Must(template.New("stacks").Funcs(template.FuncMap{
	"add": func(a, b int) int { return a + b },
}).Parse(stacksHTML))

const stacksHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Stacks — DockPilot</title>
  <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600;700&display=swap" rel="stylesheet">
  <style>
    :root { --bg:#0a0e1a; --surface:#111827; --border:#1f2937; --accent:#3b82f6; --success:#10b981; --warning:#f59e0b; --danger:#ef4444; --muted:#6b7280; --text:#e5e7eb; }
    * { box-sizing:border-box; }
    body { margin:0; background: radial-gradient(1200px 600px at 90% -100px, #1d4ed833 0%, transparent 60%), var(--bg); color:var(--text); font-family:'JetBrains Mono', ui-monospace, monospace; }
    .wrap { width:100%; padding:20px 24px; }
    .navbar { display:flex; align-items:center; gap:8px; margin-bottom:16px; flex-wrap:wrap; }
    .navbar a { color:var(--muted); text-decoration:none; font-size:13px; font-weight:600; padding:6px 12px; border-radius:8px; border:1px solid transparent; }
    .navbar a:hover { color:var(--text); background:rgba(255,255,255,0.06); }
    .navbar a.active { color:var(--text); background:var(--accent); border-color:var(--accent); }
    .header { display:flex; justify-content:space-between; align-items:center; border:1px solid var(--border); background:var(--surface); border-radius:12px; padding:14px 16px; margin-bottom:14px; }
    .title { font-size:22px; font-weight:700; letter-spacing:.4px; }
    .small { font-size:12px; color:var(--muted); }
    .badge { color:var(--muted); border:1px solid var(--border); border-radius:999px; padding:3px 10px; font-size:12px; }
    .msg { padding:10px; border-radius:8px; margin:0 0 12px 0; font-size:13px; }
    .err { background:#3f0d15; border:1px solid #7f1d1d; }
    .ok { background:#022c22; border:1px solid #065f46; }
    .panel { background:var(--surface); border:1px solid var(--border); border-radius:12px; padding:14px; margin-bottom:14px; }
    .panel h3 { margin:0 0 10px 0; font-size:14px; }
    input[type=text] { padding:8px 12px; background:var(--bg); border:1px solid var(--border); border-radius:8px; color:var(--text); font-family:inherit; font-size:13px; min-width:240px; }
    input[type=text]:focus { outline:none; border-color:var(--accent); }
    .btn { border:1px solid var(--accent); background:var(--accent); color:#fff; font-weight:600; padding:8px 14px; border-radius:8px; cursor:pointer; font-family:inherit; font-size:13px; }
    .btn:hover { opacity:0.9; }
    .btn.ghost { background:transparent; color:var(--muted); border-color:var(--border); }
    .btn.ghost:hover { color:var(--text); }
    .btn.danger { background:transparent; border-color:#7f1d1d; color:#fca5a5; }
    .btn.danger:hover { background:#3f0d15; }
    .btn.sm { padding:5px 10px; font-size:12px; }
    .deploy { display:flex; gap:8px; align-items:center; flex-wrap:wrap; }
    .stack { border:1px solid var(--border); border-radius:12px; margin-bottom:14px; overflow:hidden; }
    .stack-head { display:flex; justify-content:space-between; align-items:center; gap:10px; padding:12px 14px; background:var(--surface); flex-wrap:wrap; }
    .stack-name { font-size:16px; font-weight:700; }
    .stack-actions { display:flex; gap:6px; flex-wrap:wrap; }
    .stack-actions form { margin:0; display:inline; }
    table { width:100%; border-collapse:collapse; font-size:13px; }
    th, td { padding:9px 14px; border-top:1px solid var(--border); text-align:left; }
    th { color:var(--muted); font-weight:600; }
    .pill { font-size:11px; padding:2px 8px; border-radius:999px; }
    .pill.run { background:#022c22; border:1px solid #065f46; color:#6ee7b7; }
    .pill.off { background:#1f2937; border:1px solid var(--border); color:var(--muted); }
    code { color:#93c5fd; }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="navbar">
      <a href="/">Dashboard</a>
      <a href="/stacks" class="active">Stacks</a>
      <a href="/images">Images</a>
      <a href="/volumes">Volumes</a>
      <a href="/networks">Networks</a>
      <a href="/ipam">IPAM</a>
      <a href="/runbooks">Runbooks</a>
    </div>

    <div class="header">
      <div>
        <div class="title">Compose Stacks</div>
        <div class="small">Deploy and manage multi-service stacks{{if .ComposeVersion}} · compose {{.ComposeVersion}}{{end}}</div>
      </div>
      <div class="badge">{{.DockerHost}} | {{.Now}}</div>
    </div>

    {{if .Success}}<div class="msg ok">{{.Success}}</div>{{end}}
    {{if .Error}}<div class="msg err">{{.Error}}</div>{{end}}

    {{if .ComposeOK}}
    <div class="panel">
      <h3>Deploy a stack</h3>
      <form method="post" action="/stacks/action" class="deploy">
        <input type="hidden" name="action" value="deploy" />
        <input type="text" name="file" placeholder="Path to compose file, e.g. /srv/app/docker-compose.yml" required />
        <input type="text" name="project" placeholder="Project name (optional)" style="min-width:180px" />
        <button class="btn" type="submit">Up -d</button>
        <span class="small">runs <code>docker compose -f &lt;file&gt; up -d --remove-orphans</code></span>
      </form>
    </div>

    {{if .Stacks}}
      {{range .Stacks}}
      <div class="stack">
        <div class="stack-head">
          <div>
            <div class="stack-name">{{.Name}}</div>
            <div class="small">{{.Status}} · {{.Running}}/{{.Total}} running{{if .ConfigFile}} · <code>{{.ConfigFile}}</code>{{end}}</div>
          </div>
          <div class="stack-actions">
            {{if .ConfigFile}}
            {{$f := .ConfigFile}}{{$p := .Name}}
            <form method="post" action="/stacks/action"><input type="hidden" name="action" value="up"><input type="hidden" name="file" value="{{$f}}"><input type="hidden" name="project" value="{{$p}}"><button class="btn sm" type="submit">Up</button></form>
            <form method="post" action="/stacks/action"><input type="hidden" name="action" value="pull"><input type="hidden" name="file" value="{{$f}}"><input type="hidden" name="project" value="{{$p}}"><button class="btn sm ghost" type="submit">Pull</button></form>
            <form method="post" action="/stacks/action"><input type="hidden" name="action" value="restart"><input type="hidden" name="file" value="{{$f}}"><input type="hidden" name="project" value="{{$p}}"><button class="btn sm ghost" type="submit">Restart</button></form>
            <form method="post" action="/stacks/action"><input type="hidden" name="action" value="stop"><input type="hidden" name="file" value="{{$f}}"><input type="hidden" name="project" value="{{$p}}"><button class="btn sm ghost" type="submit">Stop</button></form>
            <form method="post" action="/stacks/action" onsubmit="return confirm('docker compose down for {{$p}}? This removes the stack containers and networks.')"><input type="hidden" name="action" value="down"><input type="hidden" name="file" value="{{$f}}"><input type="hidden" name="project" value="{{$p}}"><button class="btn sm danger" type="submit">Down</button></form>
            {{else}}
            <span class="small">no config file on record — actions unavailable</span>
            {{end}}
          </div>
        </div>
        {{if .Services}}
        <table>
          <thead><tr><th>Service</th><th>Container</th><th>State</th><th>Status</th><th>Image</th></tr></thead>
          <tbody>
            {{range .Services}}
            <tr>
              <td>{{.Service}}</td>
              <td class="small">{{.Name}}</td>
              <td>{{if eq .State "running"}}<span class="pill run">running</span>{{else}}<span class="pill off">{{.State}}</span>{{end}}</td>
              <td class="small">{{.Status}}</td>
              <td class="small">{{.Image}}</td>
            </tr>
            {{end}}
          </tbody>
        </table>
        {{end}}
      </div>
      {{end}}
    {{else}}
      <div class="panel"><span class="small">No compose stacks found. Deploy one above to get started.</span></div>
    {{end}}
    {{end}}
  </div>
</body>
</html>
`
