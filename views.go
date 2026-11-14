package main

import (
	"html/template"
	"log"
	"net/http"
	"time"
)

type fileView struct {
	Name string
	Size string
	When string
}

type loginData struct {
	Users []User
	Error string
}

type mainData struct {
	User          User
	IsCloud       bool
	CanOCR        bool
	Remote        string
	Files         []fileView
	RetentionDays int
	Busy          bool
	BusyMine      bool
	BusyWho       string
	Job           Job
	LastFile      string
	Error         string
	Info          string
}

var (
	loginTmpl = template.Must(template.New("login").Parse(loginHTML))
	mainTmpl  = template.Must(template.New("main").Parse(mainHTML))
)

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := loginTmpl.Execute(w, loginData{Users: s.cfg.Users, Error: errMsg}); err != nil {
		log.Printf("Vorlage login: %v", err)
	}
}

func (s *Server) renderMain(w http.ResponseWriter, r *http.Request, u User, errMsg, infoMsg string) {
	files, err := s.st.listFiles(u.ID)
	if err != nil {
		log.Printf("Dateiliste fuer %s: %v", u.ID, err)
	}

	views := make([]fileView, 0, len(files))
	for _, f := range files {
		views = append(views, fileView{
			Name: f.Name,
			Size: formatSize(f.Size),
			When: f.ModTime.Local().Format("02.01.2006, 15:04"),
		})
	}

	job := s.st.Snapshot()
	busy := s.st.Busy()

	data := mainData{
		User:          u,
		IsCloud:       u.Target == "ocis",
		CanOCR:        ocrAvailable() && u.Target == "download",
		Remote:        u.Remote,
		Files:         views,
		RetentionDays: s.cfg.RetentionDays,
		Busy:          busy,
		BusyMine:      busy && job.UserID == u.ID,
		BusyWho:       job.UserName,
		Error:         errMsg,
		Info:          infoMsg,
	}
	// Das Ergebnis des letzten Auftrags zeigen, aber nur der eigenen Person.
	if job.UserID == u.ID && !busy && !job.Finished.IsZero() && time.Since(job.Finished) < 10*time.Minute {
		data.Job = job
		data.LastFile = job.File
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := mainTmpl.Execute(w, data); err != nil {
		log.Printf("Vorlage main: %v", err)
	}
}

const styleCSS = `
:root { color-scheme: light dark; --fg:#1b1b1b; --bg:#f7f7f6; --card:#fff;
        --line:#dcdcd8; --muted:#666; --accent:#2f6f4f; --accent-fg:#fff;
        --warn:#8a4b00; --err:#9b1c1c; }
@media (prefers-color-scheme: dark) {
  :root { --fg:#e8e8e6; --bg:#171716; --card:#222221; --line:#393936;
          --muted:#a0a09c; --accent:#4e9b74; --accent-fg:#0d0d0c;
          --warn:#e0a060; --err:#f08a8a; }
}
* { box-sizing: border-box; }
body { margin:0; padding:1rem; font:16px/1.5 system-ui, sans-serif;
       color:var(--fg); background:var(--bg); }
main { max-width:34rem; margin:0 auto; }
h1 { font-size:1.4rem; margin:.2rem 0 1rem; }
h2 { font-size:1.05rem; margin:0 0 .6rem; }
.card { background:var(--card); border:1px solid var(--line); border-radius:10px;
        padding:1rem; margin-bottom:1rem; }
label { display:block; font-weight:600; margin:.8rem 0 .3rem; font-size:.9rem; }
select, input[type=text], input[type=password] {
  width:100%; padding:.6rem; font-size:1rem; border:1px solid var(--line);
  border-radius:7px; background:var(--bg); color:var(--fg); }
button { font-size:1rem; padding:.7rem 1.1rem; border-radius:7px; border:0;
         background:var(--accent); color:var(--accent-fg); font-weight:600;
         cursor:pointer; }
button.secondary { background:transparent; color:var(--fg);
                   border:1px solid var(--line); font-weight:500; }
button.link { background:none; border:0; color:var(--muted); font-weight:500;
              padding:.2rem .4rem; text-decoration:underline; cursor:pointer;
              font-size:.85rem; }
button[disabled] { opacity:.5; cursor:not-allowed; }
.wide { width:100%; margin-top:1.1rem; }
.row { display:flex; justify-content:space-between; align-items:center; gap:.6rem; }
.muted { color:var(--muted); font-size:.85rem; }
.err { color:var(--err); }
.warn { color:var(--warn); }
.msg { border-left:3px solid var(--accent); padding:.5rem .8rem; margin-bottom:1rem;
       background:var(--card); border-radius:0 7px 7px 0; font-size:.92rem; }
.msg.bad { border-left-color:var(--err); }
ul.files { list-style:none; padding:0; margin:0; }
ul.files li { border-top:1px solid var(--line); padding:.6rem 0; }
ul.files li:first-child { border-top:0; }
a.dl { font-weight:600; color:var(--accent); text-decoration:none;
       word-break:break-all; }
progress { width:100%; height:.7rem; }
.thumbs { display:grid; grid-template-columns:repeat(auto-fill, minmax(6.5rem, 1fr));
          gap:.6rem; margin:.8rem 0; }
.thumbs:empty { display:none; }
.thumb { display:block; text-decoration:none; color:var(--muted); font-size:.8rem;
         text-align:center; }
.thumb img, .thumb .pending { display:block; width:100%; aspect-ratio:210/297;
         object-fit:contain; background:var(--bg); border:1px solid var(--line);
         border-radius:5px; }
.thumb .pending { display:flex; align-items:center; justify-content:center;
                  font-size:.85rem; }
label.check { display:flex; gap:.5rem; align-items:center; font-weight:500; }
label.check input { width:1.1rem; height:1.1rem; }
.flip { border:2px solid var(--accent); border-radius:8px; padding:.8rem; margin:.8rem 0; }
.flip .row { flex-wrap:wrap; justify-content:flex-start; }
.grid2 { display:grid; grid-template-columns:1fr 1fr; gap:.6rem; }
`

const loginHTML = `<!DOCTYPE html>
<html lang="de">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Scanner</title>
<style>` + styleCSS + `</style>
</head>
<body>
<main>
  <h1>Scanner</h1>
  {{if .Error}}<div class="msg bad">{{.Error}}</div>{{end}}
  <form class="card" method="post" action="/login">
    <label for="user">Wer bist du?</label>
    <select id="user" name="user">
      {{range .Users}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
    </select>
    <label for="pin">PIN</label>
    <input id="pin" name="pin" type="password" inputmode="numeric"
           autocomplete="current-password" autofocus>
    <button class="wide" type="submit">Anmelden</button>
  </form>
  <p class="muted">Jede Person sieht nur ihre eigenen Scans.</p>
</main>
</body>
</html>
`

const mainHTML = `<!DOCTYPE html>
<html lang="de">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Scanner</title>
<style>` + styleCSS + `</style>
</head>
<body>
<main>
  <div class="row">
    <h1>Scanner</h1>
    <form method="post" action="/logout">
      <button class="link" type="submit">{{.User.Name}} abmelden</button>
    </form>
  </div>

  {{if .Error}}<div class="msg bad">{{.Error}}</div>{{end}}
  {{if .Info}}<div class="msg">{{.Info}}</div>{{end}}

  {{if and .Job.State (eq .Job.State "done")}}
    <div class="msg">
      Fertig: {{.Job.Pages}} Seite(n).
      {{if .Job.Note}}{{.Job.Note}}{{end}}
    </div>
  {{end}}
  {{if and .Job.State (eq .Job.State "error")}}
    <div class="msg bad">{{.Job.Note}}</div>
  {{end}}

  {{if and .Busy (not .BusyMine)}}
    <div class="msg">
      <strong>{{.BusyWho}}</strong> scannt gerade. Das Gerät kann nur einen
      Auftrag auf einmal, bitte kurz warten.
    </div>
  {{end}}

  <div class="card" id="statusCard" {{if not .BusyMine}}hidden{{end}}>
    <h2>Scan läuft</h2>
    <p id="statusStep" class="muted">Wird gestartet …</p>
    <progress id="statusBar" max="100" value="0"></progress>
    <p class="muted" id="statusMeta"></p>
    <div class="flip" id="flipBox" hidden>
      <p><strong>Vorderseiten fertig (<span id="flipCount">0</span>).</strong>
        Nimm den Stapel aus der Ablage, dreh ihn als Ganzes um, ohne die
        Reihenfolge zu ändern, und leg ihn wieder in den Einzug.</p>
      <div class="row">
        <form method="post" action="/duplex">
          <input type="hidden" name="action" value="continue">
          <button type="submit">Rückseiten scannen</button>
        </form>
        <form method="post" action="/duplex">
          <input type="hidden" name="action" value="skip">
          <button class="secondary" type="submit">Ohne Rückseiten fertig</button>
        </form>
      </div>
    </div>
    <div class="thumbs" id="thumbs"></div>
    <form method="post" action="/cancel">
      <button class="secondary" type="submit">Abbrechen</button>
    </form>
  </div>

  <form class="card" method="post" action="/scan" id="scanForm"
        {{if .Busy}}hidden{{end}}>
    <h2>Neuer Scan</h2>

    <label for="source">Vorlage</label>
    <select id="source" name="source">
      <option value="adf">Einzug, einseitig</option>
      <option value="adf_duplex">Einzug, doppelseitig (Stapel wird umgedreht)</option>
      <option value="flatbed">Glas (eine Seite)</option>
    </select>

    <div class="grid2">
      <div>
        <label for="mode">Farbe</label>
        <select id="mode" name="mode">
          <option value="gray">Graustufen</option>
          <option value="color">Farbe</option>
          <option value="bw">Schwarzweiß</option>
        </select>
      </div>
      <div>
        <label for="resolution">Auflösung</label>
        <select id="resolution" name="resolution">
          <option value="150">150 dpi (schnell)</option>
          <option value="200" selected>200 dpi</option>
          <option value="300">300 dpi (langsam)</option>
          <option value="600">600 dpi (sehr langsam)</option>
        </select>
      </div>
    </div>

    <label for="output">Ausgabe</label>
    <select id="output" name="output">
      <option value="pdf" selected>Eine PDF-Datei (alle Seiten)</option>
      <option value="png">Einzelne Bilder (PNG, eine Datei pro Seite)</option>
    </select>

    <label class="check"><input type="checkbox" name="drop_blank" value="1" checked>
      Leere Seiten entfernen</label>
    {{if .CanOCR}}
    <label class="check" id="ocrRow"><input type="checkbox" name="ocr" value="1" checked>
      Texterkennung (durchsuchbar, Seiten aufrichten und gerade ziehen)</label>
    {{end}}

    <label for="prefix">Name</label>
    <input id="prefix" name="prefix" type="text" value="scan" maxlength="32">

    <button class="wide" type="submit">Scan starten</button>
    <p class="muted">
      Das Gerät braucht pro Seite mehrere Minuten. Du kannst die Seite
      währenddessen offen lassen.
      {{if .IsCloud}}Deine Scans landen in der Cloud unter
      <code>{{.Remote}}</code>.{{end}}
    </p>
  </form>

  {{if .Files}}
  <div class="card">
    <h2>Deine Scans</h2>
    <ul class="files">
      {{range .Files}}
      <li>
        <div class="row">
          <a class="dl" href="/download/{{.Name}}">{{.Name}}</a>
          <form method="post" action="/delete"
                onsubmit="return confirm('Diese Datei löschen?')">
            <input type="hidden" name="name" value="{{.Name}}">
            <button class="link" type="submit">löschen</button>
          </form>
        </div>
        <span class="muted">{{.When}} · {{.Size}}</span>
      </li>
      {{end}}
    </ul>
    {{if gt .RetentionDays 0}}
    <p class="muted">Dateien werden nach {{.RetentionDays}} Tagen automatisch gelöscht.</p>
    {{end}}
  </div>
  {{else if not .IsCloud}}
  <p class="muted">Noch keine Scans vorhanden.</p>
  {{end}}
</main>

<script>
var wasRunning = {{.BusyMine}};
var card = document.getElementById('statusCard');
var stepEl = document.getElementById('statusStep');
var barEl = document.getElementById('statusBar');
var metaEl = document.getElementById('statusMeta');
var thumbsEl = document.getElementById('thumbs');
var flipBox = document.getElementById('flipBox');
var shownJob = '', shownPages = 0, pendingEl = null;

// Fertige Seiten als Vorschau anhaengen, die gerade laufende als Platzhalter.
function updateThumbs(s) {
  if (s.job !== shownJob) {
    thumbsEl.textContent = '';
    shownJob = s.job; shownPages = 0; pendingEl = null;
  }
  if (pendingEl) { pendingEl.remove(); pendingEl = null; }
  while (shownPages < s.done) {
    shownPages++;
    var url = '/preview/' + shownPages + '?j=' + encodeURIComponent(s.job);
    var a = document.createElement('a');
    a.className = 'thumb'; a.href = url; a.target = '_blank';
    var img = document.createElement('img');
    img.src = url; img.alt = 'Seite ' + shownPages; img.loading = 'lazy';
    a.appendChild(img);
    a.appendChild(document.createTextNode('Seite ' + shownPages));
    thumbsEl.appendChild(a);
  }
  if (/scannen/.test(s.step || '')) {
    pendingEl = document.createElement('div');
    pendingEl.className = 'thumb';
    var box = document.createElement('div');
    box.className = 'pending';
    box.textContent = (s.progress || 0) + ' %';
    pendingEl.appendChild(box);
    pendingEl.appendChild(document.createTextNode('Seite ' + (shownPages + 1)));
    thumbsEl.appendChild(pendingEl);
  }
}

function mmss(sec) {
  var m = Math.floor(sec / 60), s = sec % 60;
  return m + ':' + (s < 10 ? '0' : '') + s;
}

async function poll() {
  var res;
  try {
    res = await fetch('/api/status', { cache: 'no-store' });
  } catch (e) {
    setTimeout(poll, 4000);
    return;
  }
  if (!res.ok) { return; }
  var s = await res.json();

  if (s.state === 'running' && s.mine) {
    wasRunning = true;
    card.hidden = false;
    stepEl.textContent = s.step || 'Wird gestartet …';
    barEl.value = s.progress || 0;
    var parts = [];
    if (s.done > 0) { parts.push(s.done + ' Seite(n) fertig'); }
    if (s.elapsed > 0) { parts.push('Laufzeit ' + mmss(s.elapsed)); }
    metaEl.textContent = parts.join(' · ');
    updateThumbs(s);
    flipBox.hidden = !s.waiting;
    document.getElementById('flipCount').textContent = s.fronts;
    setTimeout(poll, 2000);
    return;
  }

  if (wasRunning) {
    // Der eigene Auftrag ist fertig: Seite neu laden, damit Ergebnis und
    // Dateiliste erscheinen.
    location.reload();
    return;
  }
  if (s.state === 'running') {
    // Jemand anderes scannt. Warten, bis das Gerät frei ist.
    setTimeout(poll, 5000);
  }
}

if ({{.Busy}}) { poll(); }

// Texterkennung gibt es nur fuer PDF.
var outSel = document.getElementById('output'), ocrRow = document.getElementById('ocrRow');
if (outSel && ocrRow) {
  var syncOCR = function () { ocrRow.hidden = outSel.value !== 'pdf'; };
  outSel.addEventListener('change', syncOCR); syncOCR();
}

document.getElementById('scanForm')?.addEventListener('submit', function (e) {
  var b = e.target.querySelector('button[type=submit]');
  if (b) { b.disabled = true; b.textContent = 'Startet …'; }
});
</script>
</body>
</html>
`
