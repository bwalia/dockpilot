package main

import (
	"bytes"
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
	Error      string
	Success    string
	Now        string
	DockerHost string
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

	containers, err := listContainers()
	if err != nil {
		a.render(w, PageData{Error: err.Error()})
		return
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

	d := PageData{
		Containers: containers,
		Total:      len(containers),
		Running:    running,
		Stopped:    stopped,
		Images:     countImages(),
		Now:        time.Now().Format("2006-01-02 15:04:05"),
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		Success:    r.URL.Query().Get("success"),
		Error:      r.URL.Query().Get("error"),
	}
	a.render(w, d)
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
    .actions form { display:inline-block; margin:2px; }
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
            <th>Actions</th>
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
                <form method="post" action="/containers/{{.ID}}/start"><button class="btn-good">Start</button></form>
                <form method="post" action="/containers/{{.ID}}/stop"><button class="btn-warn">Stop</button></form>
                <form method="post" action="/containers/{{.ID}}/restart"><button class="btn-primary">Restart</button></form>
                <form method="post" action="/containers/{{.ID}}/remove" onsubmit="return confirm('Remove container {{.Name}}?')"><button class="btn-danger">Remove</button></form>
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
