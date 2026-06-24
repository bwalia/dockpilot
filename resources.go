package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// resources.go implements the Images, Volumes and Networks management pages.
//
// All three are list-of-resources views with per-row actions (remove), optional
// create/pull forms, and global prune buttons. Rather than triplicate the IPAM
// page, they share one generic template (resourceHTML) and one action handler,
// parameterised by "kind" (images|volumes|networks). Every mutation shells out
// to the docker CLI via runDocker, mirroring the rest of the app, and redirects
// back to the page with a success/error banner.

// resourceAction is a button rendered against a row or in the global toolbar.
type resourceAction struct {
	Label   string // visible button text
	Action  string // verb sent to the action handler
	Arg     string // resource id/name the verb operates on (row actions)
	Danger  bool   // render with the danger style
	Confirm string // optional confirm() prompt; empty means no confirmation
}

// resourceForm is a single-input form (pull an image, create a volume/network).
type resourceForm struct {
	Action      string // verb sent to the action handler
	Field       string // not used server-side; the input is always named "arg"
	Placeholder string
	Button      string
}

type resourceKPI struct {
	Label string
	Value string
}

type resourceRow struct {
	Cells   []string
	Actions []resourceAction
}

// ResourcePage is the view-model for the shared resource template.
type ResourcePage struct {
	Kind       string // url segment + form target: images|volumes|networks
	Title      string
	Subtitle   string
	Columns    []string
	Rows       []resourceRow
	KPIs       []resourceKPI
	Forms      []resourceForm
	Prune      []resourceAction
	Count      int
	Search     string
	Error      string
	Success    string
	Now        string
	DockerHost string
}

// ---- page handlers -------------------------------------------------------

func (a *App) handleImages(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/images" {
		http.NotFound(w, r)
		return
	}
	a.renderResource(w, a.buildImagesPage(strings.TrimSpace(r.URL.Query().Get("q"))))
}

func (a *App) handleVolumes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/volumes" {
		http.NotFound(w, r)
		return
	}
	a.renderResource(w, a.buildVolumesPage(strings.TrimSpace(r.URL.Query().Get("q"))))
}

func (a *App) handleNetworks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/networks" {
		http.NotFound(w, r)
		return
	}
	a.renderResource(w, a.buildNetworksPage(strings.TrimSpace(r.URL.Query().Get("q"))))
}

func (a *App) renderResource(w http.ResponseWriter, page ResourcePage) {
	page.Now = nowString()
	page.DockerHost = envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.resourceTmpl.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---- page builders -------------------------------------------------------

func (a *App) buildImagesPage(search string) ResourcePage {
	page := ResourcePage{
		Kind:     "images",
		Title:    "Images",
		Subtitle: "Local Docker images · pull, tag cleanup, prune",
		Columns:  []string{"#", "Repository", "Tag", "Image ID", "Size", "Created", ""},
		Search:   search,
		Forms: []resourceForm{
			{Action: "pull", Placeholder: "Pull image, e.g. nginx:latest", Button: "Pull"},
		},
		Prune: []resourceAction{
			{Label: "Prune dangling", Action: "prune", Danger: true, Confirm: "Remove all dangling (untagged) images?"},
			{Label: "Prune all unused", Action: "prune-all", Danger: true, Confirm: "Remove ALL images not used by a container? This can free a lot of space."},
		},
	}

	images, err := listImages()
	if err != nil {
		page.Error = fmt.Sprintf("failed to list images: %v", err)
		return page
	}

	dangling := 0
	q := strings.ToLower(search)
	for _, img := range images {
		if img.Tag == "<none>" || img.Repo == "<none>" {
			dangling++
		}
		if q != "" && !strings.Contains(strings.ToLower(img.Repo+" "+img.Tag+" "+img.ID), q) {
			continue
		}
		ref := img.ID
		if img.Repo != "<none>" && img.Tag != "<none>" {
			ref = img.Repo + ":" + img.Tag
		}
		page.Rows = append(page.Rows, resourceRow{
			Cells: []string{img.Repo, img.Tag, img.ID, img.Size, img.Created},
			Actions: []resourceAction{
				{Label: "Remove", Action: "remove", Arg: ref, Danger: true, Confirm: "Remove image " + ref + "?"},
				{Label: "Force", Action: "remove-force", Arg: ref, Danger: true, Confirm: "Force-remove image " + ref + " (even if tagged/used)?"},
			},
		})
	}
	page.Count = len(images)
	page.KPIs = []resourceKPI{
		{Label: "Total Images", Value: itoa(len(images))},
		{Label: "Dangling", Value: itoa(dangling)},
		{Label: "Shown", Value: itoa(len(page.Rows))},
	}
	return page
}

func (a *App) buildVolumesPage(search string) ResourcePage {
	page := ResourcePage{
		Kind:     "volumes",
		Title:    "Volumes",
		Subtitle: "Docker volumes · create, inspect mountpoints, prune",
		Columns:  []string{"#", "Name", "Driver", "Mountpoint", "Scope", ""},
		Search:   search,
		Forms: []resourceForm{
			{Action: "create", Placeholder: "New volume name", Button: "Create"},
		},
		Prune: []resourceAction{
			{Label: "Prune unused", Action: "prune", Danger: true, Confirm: "Remove all volumes not used by at least one container?"},
		},
	}

	vols, err := listVolumes()
	if err != nil {
		page.Error = fmt.Sprintf("failed to list volumes: %v", err)
		return page
	}

	q := strings.ToLower(search)
	for _, v := range vols {
		if q != "" && !strings.Contains(strings.ToLower(v.Name+" "+v.Driver+" "+v.Mountpoint), q) {
			continue
		}
		page.Rows = append(page.Rows, resourceRow{
			Cells: []string{v.Name, v.Driver, v.Mountpoint, v.Scope},
			Actions: []resourceAction{
				{Label: "Remove", Action: "remove", Arg: v.Name, Danger: true, Confirm: "Remove volume " + v.Name + "? Data in it will be lost."},
			},
		})
	}
	page.Count = len(vols)
	page.KPIs = []resourceKPI{
		{Label: "Total Volumes", Value: itoa(len(vols))},
		{Label: "Shown", Value: itoa(len(page.Rows))},
	}
	return page
}

func (a *App) buildNetworksPage(search string) ResourcePage {
	page := ResourcePage{
		Kind:     "networks",
		Title:    "Networks",
		Subtitle: "Docker networks · create, inspect, prune",
		Columns:  []string{"#", "Name", "Driver", "Scope", "Subnet", "Gateway", "Containers", ""},
		Search:   search,
		Forms: []resourceForm{
			{Action: "create", Placeholder: "New bridge network name", Button: "Create"},
		},
		Prune: []resourceAction{
			{Label: "Prune unused", Action: "prune", Danger: true, Confirm: "Remove all networks not used by at least one container?"},
		},
	}

	nets, err := listDockerNetworks()
	if err != nil {
		page.Error = fmt.Sprintf("failed to list networks: %v", err)
		return page
	}

	q := strings.ToLower(search)
	for _, n := range nets {
		if q != "" && !strings.Contains(strings.ToLower(n.Name+" "+n.Driver+" "+n.Subnet), q) {
			continue
		}
		row := resourceRow{
			Cells: []string{n.Name, n.Driver, n.Scope, emptyDash(n.Subnet), emptyDash(n.Gateway), itoa(len(n.Containers))},
		}
		// The built-in networks can't be removed; don't offer a dead button.
		if !isBuiltinNetwork(n.Name) {
			row.Actions = []resourceAction{
				{Label: "Remove", Action: "remove", Arg: n.Name, Danger: true, Confirm: "Remove network " + n.Name + "?"},
			}
		}
		page.Rows = append(page.Rows, row)
	}
	page.Count = len(nets)
	page.KPIs = []resourceKPI{
		{Label: "Total Networks", Value: itoa(len(nets))},
		{Label: "Shown", Value: itoa(len(page.Rows))},
	}
	return page
}

// ---- action handler ------------------------------------------------------

// handleResourceAction is the shared POST handler for all three pages. It is
// registered three times via resourceActionHandler(kind).
func (a *App) handleResourceAction(kind string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		resourceRedirect(w, r, kind, "invalid form", true)
		return
	}
	action := strings.TrimSpace(r.FormValue("action"))
	arg := strings.TrimSpace(r.FormValue("arg"))

	args, err := resourceCommandArgs(kind, action, arg)
	if err != nil {
		resourceRedirect(w, r, kind, err.Error(), true)
		return
	}

	if _, err := runDocker(args...); err != nil {
		resourceRedirect(w, r, kind, fmt.Sprintf("%s failed: %v", action, err), true)
		return
	}
	msg := fmt.Sprintf("%s ok", action)
	if arg != "" {
		msg = fmt.Sprintf("%s %s ok", action, arg)
	}
	resourceRedirect(w, r, kind, msg, false)
}

// resourceCommandArgs maps a (kind, action, arg) triple to docker CLI args,
// validating any user-supplied name/ref so it can't be smuggled in as a flag.
func resourceCommandArgs(kind, action, arg string) ([]string, error) {
	needsArg := func() error {
		if !validDockerArg(arg) {
			return fmt.Errorf("invalid or missing name")
		}
		return nil
	}

	switch kind {
	case "images":
		switch action {
		case "remove":
			if err := needsArg(); err != nil {
				return nil, err
			}
			return []string{"rmi", arg}, nil
		case "remove-force":
			if err := needsArg(); err != nil {
				return nil, err
			}
			return []string{"rmi", "-f", arg}, nil
		case "pull":
			if err := needsArg(); err != nil {
				return nil, err
			}
			return []string{"pull", arg}, nil
		case "prune":
			return []string{"image", "prune", "-f"}, nil
		case "prune-all":
			return []string{"image", "prune", "-a", "-f"}, nil
		}
	case "volumes":
		switch action {
		case "remove":
			if err := needsArg(); err != nil {
				return nil, err
			}
			return []string{"volume", "rm", arg}, nil
		case "create":
			if err := needsArg(); err != nil {
				return nil, err
			}
			return []string{"volume", "create", arg}, nil
		case "prune":
			return []string{"volume", "prune", "-f"}, nil
		}
	case "networks":
		switch action {
		case "remove":
			if err := needsArg(); err != nil {
				return nil, err
			}
			return []string{"network", "rm", arg}, nil
		case "create":
			if err := needsArg(); err != nil {
				return nil, err
			}
			return []string{"network", "create", arg}, nil
		case "prune":
			return []string{"network", "prune", "-f"}, nil
		}
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

func resourceRedirect(w http.ResponseWriter, r *http.Request, kind, msg string, isErr bool) {
	key := "success"
	if isErr {
		key = "error"
	}
	http.Redirect(w, r, "/"+kind+"?"+key+"="+urlQueryEscape(msg), http.StatusSeeOther)
}

// ---- docker listing helpers ----------------------------------------------

type imageView struct {
	Repo    string
	Tag     string
	ID      string
	Size    string
	Created string
}

func listImages() ([]imageView, error) {
	out, err := runDocker("images", "--format", "{{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.Size}}\t{{.CreatedSince}}")
	if err != nil {
		return nil, err
	}
	var images []imageView
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		images = append(images, imageView{Repo: f[0], Tag: f[1], ID: f[2], Size: f[3], Created: f[4]})
	}
	return images, nil
}

type volumeView struct {
	Name       string
	Driver     string
	Mountpoint string
	Scope      string
}

func listVolumes() ([]volumeView, error) {
	out, err := runDocker("volume", "ls", "--format", "{{.Name}}\t{{.Driver}}\t{{.Mountpoint}}\t{{.Scope}}")
	if err != nil {
		return nil, err
	}
	var vols []volumeView
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		v := volumeView{Name: f[0], Driver: f[1]}
		if len(f) > 2 {
			v.Mountpoint = f[2]
		}
		if len(f) > 3 {
			v.Scope = f[3]
		}
		vols = append(vols, v)
	}
	return vols, nil
}

// ---- small helpers -------------------------------------------------------

func isBuiltinNetwork(name string) bool {
	switch name {
	case "bridge", "host", "none":
		return true
	}
	return false
}

// validDockerArg rejects empty input and anything that could be interpreted as
// a CLI flag, while permitting image refs and resource names (letters, digits
// and the punctuation Docker allows: _ . - : / @).
func validDockerArg(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.-:/@", r) {
			continue
		}
		return false
	}
	return true
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func nowString() string { return time.Now().Format("2006-01-02 15:04:05") }

const resourceHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>{{.Title}} — DockPilot</title>
  <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600;700&display=swap" rel="stylesheet">
  <style>
    :root { --bg:#0a0e1a; --surface:#111827; --border:#1f2937; --accent:#3b82f6; --success:#10b981; --warning:#f59e0b; --danger:#ef4444; --muted:#6b7280; --text:#e5e7eb; }
    * { box-sizing:border-box; }
    body { margin:0; background: radial-gradient(1200px 600px at 90% -100px, #1d4ed833 0%, transparent 60%), var(--bg); color:var(--text); font-family:'JetBrains Mono', ui-monospace, monospace; }
    .wrap { width:100%; max-width:none; margin:0; padding:20px 24px; }
    .navbar { display:flex; align-items:center; gap:8px; margin-bottom:16px; flex-wrap:wrap; }
    .navbar a { color:var(--muted); text-decoration:none; font-size:13px; font-weight:600; padding:6px 12px; border-radius:8px; border:1px solid transparent; }
    .navbar a:hover { color:var(--text); background:rgba(255,255,255,0.06); }
    .navbar a.active { color:var(--text); background:var(--accent); border-color:var(--accent); }
    .header { display:flex; justify-content:space-between; align-items:center; border:1px solid var(--border); background:var(--surface); border-radius:12px; padding:14px 16px; margin-bottom:14px; }
    .title { font-size:22px; font-weight:700; letter-spacing:.4px; line-height:1.1; }
    .small { font-size:12px; color:var(--muted); }
    .badge { color:var(--muted); border:1px solid var(--border); border-radius:999px; padding:3px 10px; font-size:12px; }
    .msg { padding:10px; border-radius:8px; margin:0 0 12px 0; font-size:13px; }
    .err { background:#3f0d15; border:1px solid #7f1d1d; }
    .ok { background:#022c22; border:1px solid #065f46; }
    .kpis { display:grid; grid-template-columns:repeat(3,minmax(0,1fr)); gap:10px; margin-bottom:14px; }
    .kpi { background:var(--surface); border:1px solid var(--border); border-radius:10px; padding:12px; }
    .kpi .label { color:var(--muted); font-size:12px; }
    .kpi .value { font-size:22px; font-weight:700; margin-top:6px; }
    .panel { background:var(--surface); border:1px solid var(--border); border-radius:12px; padding:14px; margin-bottom:14px; }
    .toolbar { display:flex; gap:8px; align-items:center; flex-wrap:wrap; margin-bottom:14px; }
    .toolbar form { display:flex; gap:6px; align-items:center; margin:0; }
    input[type=text] { padding:8px 12px; background:var(--bg); border:1px solid var(--border); border-radius:8px; color:var(--text); font-family:inherit; font-size:13px; min-width:220px; }
    input[type=text]:focus { outline:none; border-color:var(--accent); }
    .btn { border:1px solid var(--accent); background:var(--accent); color:#fff; font-weight:600; padding:8px 14px; border-radius:8px; cursor:pointer; font-family:inherit; font-size:13px; }
    .btn:hover { opacity:0.9; }
    .btn.ghost { background:transparent; color:var(--muted); border-color:var(--border); }
    .btn.ghost:hover { color:var(--text); }
    .btn.danger { background:transparent; border-color:#7f1d1d; color:#fca5a5; }
    .btn.danger:hover { background:#3f0d15; }
    .btn.sm { padding:5px 10px; font-size:12px; }
    .spacer { flex:1; }
    table { width:100%; border-collapse:collapse; font-size:13px; }
    th, td { padding:10px 8px; border-bottom:1px solid var(--border); text-align:left; vertical-align:middle; }
    th { color:var(--muted); font-weight:600; }
    td.actions { white-space:nowrap; text-align:right; }
    td.actions form { display:inline; }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="navbar">
      <a href="/">Dashboard</a>
      <a href="/images"{{if eq .Kind "images"}} class="active"{{end}}>Images</a>
      <a href="/volumes"{{if eq .Kind "volumes"}} class="active"{{end}}>Volumes</a>
      <a href="/networks"{{if eq .Kind "networks"}} class="active"{{end}}>Networks</a>
      <a href="/ipam">IPAM</a>
      <a href="/runbooks">Runbooks</a>
    </div>

    <div class="header">
      <div>
        <div class="title">{{.Title}}</div>
        <div class="small">{{.Subtitle}}</div>
      </div>
      <div class="badge">{{.DockerHost}} | {{.Now}}</div>
    </div>

    {{if .Success}}<div class="msg ok">{{.Success}}</div>{{end}}
    {{if .Error}}<div class="msg err">{{.Error}}</div>{{end}}

    <div class="kpis">
      {{range .KPIs}}
      <div class="kpi"><div class="label">{{.Label}}</div><div class="value">{{.Value}}</div></div>
      {{end}}
    </div>

    <div class="toolbar">
      <form method="get" action="/{{.Kind}}">
        <input type="text" name="q" value="{{.Search}}" placeholder="Search {{.Kind}}" />
        <button class="btn ghost" type="submit">Search</button>
        {{if .Search}}<a class="small" href="/{{.Kind}}">clear</a>{{end}}
      </form>
      {{range .Forms}}
      <form method="post" action="/{{$.Kind}}/action">
        <input type="hidden" name="action" value="{{.Action}}" />
        <input type="text" name="arg" placeholder="{{.Placeholder}}" required />
        <button class="btn" type="submit">{{.Button}}</button>
      </form>
      {{end}}
      <div class="spacer"></div>
      {{range .Prune}}
      <form method="post" action="/{{$.Kind}}/action" onsubmit="return confirm('{{.Confirm}}')">
        <input type="hidden" name="action" value="{{.Action}}" />
        <button class="btn danger" type="submit">{{.Label}}</button>
      </form>
      {{end}}
    </div>

    <div class="panel">
      <table>
        <thead>
          <tr>{{range .Columns}}<th>{{.}}</th>{{end}}</tr>
        </thead>
        <tbody>
          {{if .Rows}}
            {{range $i, $row := .Rows}}
            <tr>
              <td class="small">{{add $i 1}}</td>
              {{range $row.Cells}}<td>{{if .}}{{.}}{{else}}<span class="small">—</span>{{end}}</td>{{end}}
              <td class="actions">
                {{range $row.Actions}}
                <form method="post" action="/{{$.Kind}}/action"{{if .Confirm}} onsubmit="return confirm('{{.Confirm}}')"{{end}}>
                  <input type="hidden" name="action" value="{{.Action}}" />
                  <input type="hidden" name="arg" value="{{.Arg}}" />
                  <button class="btn sm {{if .Danger}}danger{{else}}ghost{{end}}" type="submit">{{.Label}}</button>
                </form>
                {{end}}
              </td>
            </tr>
            {{end}}
          {{else}}
            <tr><td colspan="{{len .Columns}}" class="small">No {{.Kind}} found.</td></tr>
          {{end}}
        </tbody>
      </table>
    </div>
  </div>
</body>
</html>
`
