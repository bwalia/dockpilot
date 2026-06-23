package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// exec.go implements the in-browser web terminal — the single feature devs reach
// for most. It opens a real interactive-ish shell into a running container by
// streaming keystrokes and output over a WebSocket to `docker exec -i <id> <sh>`.
//
// The whole project is stdlib-only (see go.mod), so rather than pull in a
// WebSocket dependency we hand-roll the small slice of RFC 6455 we need:
// the upgrade handshake plus masked-frame read / unmasked-frame write over a
// hijacked TCP connection. The browser side uses xterm.js from a CDN.
//
// Note: we deliberately use `exec -i` (no `-t`). Go's stdlib has no PTY, so a
// true TTY would need a cgo/pty dependency; a non-TTY pipe still runs commands
// fine for the day-to-day "shell in, poke around, run a command" workflow.

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket opcodes (the subset we handle).
const (
	wsOpText  = 0x1
	wsOpClose = 0x8
	wsOpPing  = 0x9
	wsOpPong  = 0xA
)

// wsConn is a minimal server-side WebSocket over a hijacked net.Conn.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex // serialises frame writes (output pump + ping/pong + close)
}

// wsUpgrade performs the RFC 6455 handshake and hijacks the connection. After
// it returns, the standard http.Server no longer manages the socket (so its
// WriteTimeout won't kill a long-lived terminal session) — we own conn.Close.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("not a websocket upgrade request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("response writer does not support hijacking")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	h := sha1.New()
	io.WriteString(h, key+wsGUID)
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, br: rw.Reader}, nil
}

// readMessage returns the next data/close frame, transparently answering pings
// and skipping pongs. Client→server frames are always masked.
func (c *wsConn) readMessage() (opcode byte, payload []byte, err error) {
	for {
		b0, err := c.br.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		b1, err := c.br.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		op := b0 & 0x0f
		masked := b1&0x80 != 0
		ln := int64(b1 & 0x7f)
		switch ln {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return 0, nil, err
			}
			ln = int64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return 0, nil, err
			}
			ln = int64(binary.BigEndian.Uint64(ext[:]))
		}
		if ln > 1<<20 {
			return 0, nil, fmt.Errorf("websocket frame too large (%d bytes)", ln)
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		data := make([]byte, ln)
		if _, err := io.ReadFull(c.br, data); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range data {
				data[i] ^= mask[i&3]
			}
		}
		switch op {
		case wsOpPing:
			c.writeFrame(wsOpPong, data)
			continue
		case wsOpPong:
			continue
		}
		return op, data, nil
	}
}

// writeFrame writes a single unmasked server frame (server frames are never
// masked per RFC 6455). All keystroke/output traffic is sent as one frame.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	n := len(payload)
	var header []byte
	b0 := byte(0x80) | opcode // FIN set; single, unfragmented frame
	switch {
	case n < 126:
		header = []byte{b0, byte(n)}
	case n < 1<<16:
		header = []byte{b0, 126, byte(n >> 8), byte(n)}
	default:
		header = make([]byte, 10)
		header[0] = b0
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:], uint64(n))
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (c *wsConn) writeText(s string) error { return c.writeFrame(wsOpText, []byte(s)) }
func (c *wsConn) writeClose()              { c.writeFrame(wsOpClose, nil) }

// validShell whitelists the shells the terminal will launch, so the query
// string can't be used to run an arbitrary program inside the container.
func validShell(s string) bool {
	switch s {
	case "sh", "bash", "ash", "zsh", "/bin/sh", "/bin/bash":
		return true
	}
	return false
}

// handleExecWS upgrades to a WebSocket and bridges it to `docker exec -i`.
func (a *App) handleExecWS(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	shell := strings.TrimSpace(r.URL.Query().Get("shell"))
	if shell == "" {
		shell = "sh"
	}
	if !validDockerArg(id) || !validShell(shell) {
		http.Error(w, "invalid container id or shell", http.StatusBadRequest)
		return
	}

	ws, err := wsUpgrade(w, r)
	if err != nil {
		return // handshake failed; nothing more we can say to the client
	}
	defer ws.conn.Close()

	// One pipe carries both stdout and stderr so the terminal shows everything
	// interleaved, the way a real shell does.
	pr, pw, err := os.Pipe()
	if err != nil {
		ws.writeText("dockpilot: pipe error: " + err.Error() + "\r\n")
		return
	}
	cmd := exec.Command("docker", "exec", "-i", id, shell)
	cmd.Stdout = pw
	cmd.Stderr = pw
	stdin, err := cmd.StdinPipe()
	if err != nil {
		pw.Close()
		pr.Close()
		ws.writeText("dockpilot: stdin error: " + err.Error() + "\r\n")
		return
	}
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		ws.writeText("dockpilot: failed to exec into container: " + err.Error() + "\r\n")
		ws.writeClose()
		return
	}
	pw.Close() // the child holds the write end; drop the parent's copy so reads EOF on exit

	ws.writeText("\x1b[32mConnected to " + id + " (" + shell + ")\x1b[0m\r\n")

	// container output -> browser
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := pr.Read(buf)
			if n > 0 {
				if werr := ws.writeFrame(wsOpText, buf[:n]); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		close(done)
	}()

	// browser keystrokes -> container stdin
	go func() {
		for {
			op, data, rerr := ws.readMessage()
			if rerr != nil || op == wsOpClose {
				break
			}
			if len(data) > 0 {
				if _, werr := stdin.Write(data); werr != nil {
					break
				}
			}
		}
		stdin.Close()
		if cmd.Process != nil {
			cmd.Process.Kill() // browser went away — don't leave the exec hanging
		}
	}()

	<-done
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
	cmd.Wait()
	pr.Close()
	ws.writeText("\r\n\x1b[33m[session closed]\x1b[0m\r\n")
	ws.writeClose()
}

// handleTerminalPage renders the xterm.js terminal shell for a container.
func (a *App) handleTerminalPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/terminal" {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	shell := strings.TrimSpace(r.URL.Query().Get("shell"))
	if shell == "" || !validShell(shell) {
		shell = "sh"
	}
	if id != "" && !validDockerArg(id) {
		http.Error(w, "invalid container id", http.StatusBadRequest)
		return
	}
	var name string
	if id != "" {
		containers, _ := listContainers()
		name = containerNameByID(containers, id)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		ID, Name, Shell, DockerHost, Now string
	}{
		ID:         id,
		Name:       name,
		Shell:      shell,
		DockerHost: envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		Now:        nowString(),
	}
	if err := terminalTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var terminalTmpl = template.Must(template.New("terminal").Parse(terminalHTML))

const terminalHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Terminal — {{.Name}} — DockPilot</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@5.3.0/css/xterm.min.css" />
  <script src="https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.min.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8.0/lib/xterm-addon-fit.min.js"></script>
  <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600;700&display=swap" rel="stylesheet">
  <style>
    :root { --bg:#0a0e1a; --surface:#111827; --border:#1f2937; --accent:#3b82f6; --success:#10b981; --danger:#ef4444; --muted:#6b7280; --text:#e5e7eb; }
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
    select, button { font-family:inherit; font-size:13px; padding:6px 10px; border-radius:8px; border:1px solid var(--border); background:var(--bg); color:var(--text); cursor:pointer; }
    button.accent { background:var(--accent); border-color:var(--accent); color:#fff; font-weight:600; }
    #term { flex:1; padding:8px; }
    .empty { margin:auto; text-align:center; color:var(--muted); }
  </style>
</head>
<body>
  <div class="bar">
    <a href="/">&larr; Dashboard</a>
    <span class="title">Terminal</span>
    <span class="small">{{if .Name}}{{.Name}}{{else}}—{{end}} · {{.DockerHost}}</span>
    <span class="spacer"></span>
    {{if .ID}}
    <span class="small"><span id="status" class="dot off"></span> <span id="statustext">connecting…</span></span>
    <label class="small">shell
      <select id="shell">
        <option value="sh"{{if eq .Shell "sh"}} selected{{end}}>sh</option>
        <option value="bash"{{if eq .Shell "bash"}} selected{{end}}>bash</option>
        <option value="ash"{{if eq .Shell "ash"}} selected{{end}}>ash</option>
        <option value="zsh"{{if eq .Shell "zsh"}} selected{{end}}>zsh</option>
      </select>
    </label>
    <button id="reconnect" class="accent" type="button">Reconnect</button>
    {{end}}
  </div>
  {{if .ID}}
  <div id="term"></div>
  <script>
    var CID = "{{.ID}}";
    var term = new Terminal({ cursorBlink:true, fontFamily:"JetBrains Mono, monospace", fontSize:13,
      theme:{ background:"#0a0e1a", foreground:"#e5e7eb" } });
    var fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(document.getElementById("term"));
    fit.fit();
    window.addEventListener("resize", function(){ try { fit.fit(); } catch(e){} });

    var ws = null, dataDisp = null;
    var statusDot = document.getElementById("status");
    var statusText = document.getElementById("statustext");
    function setStatus(on, txt){ statusDot.className = "dot " + (on?"on":"off"); statusText.textContent = txt; }

    function connect(){
      if (ws) { try { ws.close(); } catch(e){} }
      if (dataDisp) { dataDisp.dispose(); dataDisp = null; }
      term.reset();
      var shell = document.getElementById("shell").value;
      var proto = location.protocol === "https:" ? "wss:" : "ws:";
      var url = proto + "//" + location.host + "/containers/exec/ws?id=" + encodeURIComponent(CID) + "&shell=" + encodeURIComponent(shell);
      setStatus(false, "connecting…");
      ws = new WebSocket(url);
      ws.onopen = function(){ setStatus(true, "connected"); term.focus(); };
      ws.onmessage = function(ev){ term.write(typeof ev.data === "string" ? ev.data : new Uint8Array(ev.data)); };
      ws.onclose = function(){ setStatus(false, "disconnected"); };
      ws.onerror = function(){ setStatus(false, "error"); };
      dataDisp = term.onData(function(d){ if (ws && ws.readyState === 1) ws.send(d); });
    }
    document.getElementById("reconnect").addEventListener("click", connect);
    document.getElementById("shell").addEventListener("change", connect);
    connect();
  </script>
  {{else}}
  <div class="empty">No container selected. Open a terminal from the dashboard's container actions.</div>
  {{end}}
</body>
</html>
`
