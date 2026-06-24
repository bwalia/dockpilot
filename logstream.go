package main

import (
	"bufio"
	"context"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// logstream.go implements live, follow-mode container logs. The dashboard's
// existing logs button is a one-shot `docker logs --tail 200` dump; this adds a
// dedicated page that streams `docker logs -f` in real time, with a client-side
// text filter, an adjustable tail window, pause/resume and a download button.
//
// The stream is delivered as Server-Sent Events. Rather than rely on
// http.Flusher (whose lifetime is bounded by the server's WriteTimeout, which
// would cut a long-running follow), we hijack the connection and write the SSE
// response by hand — the same trick the web terminal uses for WebSockets — so a
// tail can stay open indefinitely. The child `docker logs` process is killed as
// soon as the browser disconnects.

// handleLogStream streams a container's logs to the browser as SSE.
func (a *App) handleLogStream(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if !validDockerArg(id) {
		http.Error(w, "invalid container id", http.StatusBadRequest)
		return
	}
	tail := sanitizeTail(r.URL.Query().Get("tail"))

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	// Write the SSE response head ourselves (we own the socket now).
	io.WriteString(bufrw, "HTTP/1.1 200 OK\r\n"+
		"Content-Type: text/event-stream\r\n"+
		"Cache-Control: no-cache\r\n"+
		"Connection: close\r\n"+
		"X-Accel-Buffering: no\r\n\r\n")
	if err := bufrw.Flush(); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Detect browser disconnect: the client never writes to an SSE stream, so a
	// successful read means data we ignore, and an error/EOF means it's gone.
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := bufrw.Read(buf); err != nil {
				cancel()
				return
			}
		}
	}()

	// Both streams merge so stderr-logging containers show up too.
	pr, pw, err := os.Pipe()
	if err != nil {
		writeSSE(conn, bufrw, "error", "pipe error: "+err.Error())
		return
	}
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", "--tail", tail, id)
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		writeSSE(conn, bufrw, "error", "failed to follow logs: "+err.Error())
		return
	}
	pw.Close()
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
		pr.Close()
	}()

	writeSSE(conn, bufrw, "status", "following "+id+" (tail "+tail+")")

	// A periodic heartbeat keeps idle connections (and proxies) alive when a
	// container is quiet.
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	heartbeat := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case heartbeat <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	lines := make(chan string, 256)
	go func() {
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		close(lines)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat:
			conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := io.WriteString(bufrw, ": ping\n\n"); err != nil {
				return
			}
			if err := bufrw.Flush(); err != nil {
				return
			}
		case line, ok := <-lines:
			if !ok {
				writeSSE(conn, bufrw, "status", "log stream ended")
				return
			}
			conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := writeSSE(conn, bufrw, "log", line); err != nil {
				return
			}
		}
	}
}

// writeSSE emits one SSE event. The payload is split across data: lines so
// embedded newlines stay legal, per the SSE spec.
func writeSSE(conn io.Writer, bufrw *bufio.ReadWriter, event, data string) error {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	for _, ln := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := io.WriteString(bufrw, b.String()); err != nil {
		return err
	}
	return bufrw.Flush()
}

// sanitizeTail bounds the user-supplied tail count to a sane numeric range,
// also accepting "all". Anything invalid falls back to 200.
func sanitizeTail(s string) string {
	s = strings.TrimSpace(s)
	if s == "all" {
		return "all"
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return "200"
	}
	if n > 10000 {
		n = 10000
	}
	return strconv.Itoa(n)
}

// handleLogsPage renders the live-logs viewer for a container.
func (a *App) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/logs" {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id != "" && !validDockerArg(id) {
		http.Error(w, "invalid container id", http.StatusBadRequest)
		return
	}
	tail := sanitizeTail(r.URL.Query().Get("tail"))

	var name string
	if id != "" {
		containers, _ := listContainers()
		name = containerNameByID(containers, id)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		ID, Name, Tail, DockerHost, Now string
	}{
		ID:         id,
		Name:       name,
		Tail:       tail,
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		Now:        nowString(),
	}
	if err := logsTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var logsTmpl = template.Must(template.New("logs").Parse(logsHTML))

const logsHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Logs — {{.Name}} — DockPilot</title>
  <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600;700&display=swap" rel="stylesheet">
  <style>
    :root { --bg:#0a0e1a; --surface:#111827; --border:#1f2937; --accent:#3b82f6; --success:#10b981; --warning:#f59e0b; --danger:#ef4444; --muted:#6b7280; --text:#e5e7eb; }
    * { box-sizing:border-box; }
    body { margin:0; background:var(--bg); color:var(--text); font-family:'JetBrains Mono', ui-monospace, monospace; height:100vh; display:flex; flex-direction:column; }
    .bar { display:flex; align-items:center; gap:10px; padding:10px 16px; border-bottom:1px solid var(--border); background:var(--surface); flex-wrap:wrap; }
    .bar a { color:var(--muted); text-decoration:none; font-size:13px; font-weight:600; padding:6px 12px; border-radius:8px; }
    .bar a:hover { color:var(--text); background:rgba(255,255,255,0.06); }
    .title { font-size:15px; font-weight:700; }
    .small { font-size:12px; color:var(--muted); }
    .spacer { flex:1; }
    .dot { width:9px; height:9px; border-radius:50%; display:inline-block; background:var(--muted); }
    .dot.on { background:var(--success); } .dot.off { background:var(--danger); }
    input, select, button { font-family:inherit; font-size:13px; padding:6px 10px; border-radius:8px; border:1px solid var(--border); background:var(--bg); color:var(--text); }
    button { cursor:pointer; }
    button.accent { background:var(--accent); border-color:var(--accent); color:#fff; font-weight:600; }
    input[type=text] { min-width:200px; }
    input:focus, select:focus { outline:none; border-color:var(--accent); }
    #log { flex:1; overflow:auto; margin:0; padding:10px 14px; font-size:12.5px; line-height:1.5; white-space:pre-wrap; word-break:break-word; }
    .line.hidden { display:none; }
    .line .match { background:#f59e0b55; border-radius:3px; }
    .meta { color:var(--muted); font-style:italic; }
    .empty { margin:auto; text-align:center; color:var(--muted); }
  </style>
</head>
<body>
  <div class="bar">
    <a href="/">&larr; Dashboard</a>
    <span class="title">Logs</span>
    <span class="small">{{if .Name}}{{.Name}}{{else}}—{{end}} · {{.DockerHost}}</span>
    <span class="spacer"></span>
    {{if .ID}}
    <span class="small"><span id="status" class="dot off"></span> <span id="statustext">connecting…</span></span>
    <input type="text" id="filter" placeholder="filter (grep)…" />
    <label class="small">tail
      <select id="tail">
        <option value="100"{{if eq .Tail "100"}} selected{{end}}>100</option>
        <option value="200"{{if eq .Tail "200"}} selected{{end}}>200</option>
        <option value="500"{{if eq .Tail "500"}} selected{{end}}>500</option>
        <option value="1000"{{if eq .Tail "1000"}} selected{{end}}>1000</option>
        <option value="all"{{if eq .Tail "all"}} selected{{end}}>all</option>
      </select>
    </label>
    <button id="pause" type="button">Pause</button>
    <button id="clear" type="button">Clear</button>
    <button id="download" type="button">Download</button>
    <button id="reconnect" class="accent" type="button">Reconnect</button>
    {{end}}
  </div>
  {{if .ID}}
  <pre id="log"></pre>
  <script>
    var CID = "{{.ID}}";
    var logEl = document.getElementById("log");
    var filterEl = document.getElementById("filter");
    var statusDot = document.getElementById("status");
    var statusText = document.getElementById("statustext");
    var paused = false, es = null;
    var MAX_LINES = 5000;

    function setStatus(on, txt){ statusDot.className = "dot " + (on?"on":"off"); statusText.textContent = txt; }
    function escapeHtml(s){ return s.replace(/[&<>]/g, function(c){ return {"&":"&amp;","<":"&lt;",">":"&gt;"}[c]; }); }

    function matches(text){
      var f = filterEl.value.trim();
      if (!f) return true;
      try { return new RegExp(f, "i").test(text); } catch(e){ return text.toLowerCase().indexOf(f.toLowerCase()) !== -1; }
    }
    function append(text, cls){
      var atBottom = (logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight) < 40;
      var div = document.createElement("div");
      div.className = "line" + (cls ? " " + cls : "");
      div.dataset.raw = text;
      div.textContent = text;
      if (!matches(text)) div.classList.add("hidden");
      logEl.appendChild(div);
      while (logEl.childNodes.length > MAX_LINES) logEl.removeChild(logEl.firstChild);
      if (atBottom) logEl.scrollTop = logEl.scrollHeight;
    }
    function applyFilter(){
      var nodes = logEl.childNodes;
      for (var i=0;i<nodes.length;i++){
        var raw = nodes[i].dataset ? nodes[i].dataset.raw : "";
        if (raw === undefined) continue;
        if (matches(raw)) nodes[i].classList.remove("hidden"); else nodes[i].classList.add("hidden");
      }
    }
    filterEl.addEventListener("input", applyFilter);

    function connect(){
      if (es) es.close();
      logEl.innerHTML = "";
      var tail = document.getElementById("tail").value;
      setStatus(false, "connecting…");
      es = new EventSource("/containers/logs/stream?id=" + encodeURIComponent(CID) + "&tail=" + encodeURIComponent(tail));
      es.addEventListener("open", function(){ setStatus(true, "streaming"); });
      es.addEventListener("log", function(ev){ if (!paused) append(ev.data); });
      es.addEventListener("status", function(ev){ append("— " + ev.data + " —", "meta"); });
      es.addEventListener("error", function(ev){
        if (ev.data) append("— error: " + ev.data + " —", "meta");
        setStatus(false, "disconnected");
      });
      es.onerror = function(){ setStatus(false, "reconnecting…"); };
    }

    document.getElementById("pause").addEventListener("click", function(){
      paused = !paused; this.textContent = paused ? "Resume" : "Pause";
    });
    document.getElementById("clear").addEventListener("click", function(){ logEl.innerHTML = ""; });
    document.getElementById("reconnect").addEventListener("click", connect);
    document.getElementById("tail").addEventListener("change", connect);
    document.getElementById("download").addEventListener("click", function(){
      var text = "";
      var nodes = logEl.childNodes;
      for (var i=0;i<nodes.length;i++){ if (nodes[i].dataset && nodes[i].dataset.raw !== undefined) text += nodes[i].dataset.raw + "\n"; }
      var blob = new Blob([text], {type:"text/plain"});
      var a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = "{{if .Name}}{{.Name}}{{else}}container{{end}}-logs.txt";
      a.click();
      URL.revokeObjectURL(a.href);
    });
    connect();
  </script>
  {{else}}
  <div class="empty">No container selected. Open logs from the dashboard's container actions.</div>
  {{end}}
</body>
</html>
`
