package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/NakliTechie/continuum/api"
)

// BrowserRead is deliberately a separate read-only doorway on the existing
// loopback listener. It never accepts the root operator token, and it never
// forwards a browser Origin to the private /v1 API.
func (m *Modern) BrowserRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	origin := r.Header.Get("Origin")
	if origin != "" {
		u, err := url.Parse(origin)
		same := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
		if !same && !m.s.cfg.OriginAllowed(origin) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	viewer := token != "" && m.observer != "" && subtle.ConstantTimeCompare([]byte(token), []byte(m.observer)) == 1
	if !viewer {
		g, err := m.findGrant(token)
		if err != nil || g.Class != "observer" {
			http.Error(w, "observer credential required", http.StatusForbidden)
			return
		}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (128<<10)+1))
	if err != nil || len(data) > 128<<10 {
		http.Error(w, "bounded JSON required", http.StatusBadRequest)
		return
	}
	var q api.Request
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&q) != nil || dec.Decode(new(any)) != io.EOF {
		http.Error(w, "one JSON request required", http.StatusBadRequest)
		return
	}
	switch q.Operation {
	case "version", "status", "events", "screen", "observe":
	default:
		http.Error(w, "read-only browser endpoint", http.StatusForbidden)
		return
	}
	forward := r.Clone(r.Context())
	forward.Header = r.Header.Clone()
	forward.Header.Del("Origin")
	forward.Body = io.NopCloser(bytes.NewReader(data))
	m.ServeHTTP(w, forward)
}

func (m *Modern) BrowserObserver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	_, _ = io.WriteString(w, observerHTML)
}

const observerHTML = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Continuum observer</title>
<style>body{font:15px system-ui;background:#10151b;color:#e8eef5;max-width:900px;margin:3rem auto;padding:0 1rem}input,button{font:inherit;padding:.55rem;background:#1c2834;color:inherit;border:1px solid #526172;border-radius:5px}input{width:min(32rem,70vw)}button{cursor:pointer}pre{white-space:pre-wrap;overflow-wrap:anywhere;background:#1c2834;padding:1rem;border-radius:6px}p{color:#afbdcb}</style>
<h1>Continuum observer</h1><p>This page accepts only a read-only observer token or a scoped observer grant. It cannot start, stop, approve or control work. The token stays in this tab's memory.</p>
<label>Observer token <input id="token" type="password" autocomplete="off"></label> <button id="refresh">Refresh</button>
<p id="state" role="status">Enter an observer token.</p><pre id="blocks"></pre>
<script>
const token=document.getElementById('token'),state=document.getElementById('state'),blocks=document.getElementById('blocks');
async function refresh(){if(!token.value){state.textContent='Enter an observer token.';return}try{const r=await fetch('/v1/observe',{method:'POST',headers:{'Content-Type':'application/json','Authorization':'Bearer '+token.value},body:JSON.stringify({operation:'status'})});if(!r.ok)throw Error('Observer access refused ('+r.status+').');const v=await r.json();if(v.class!=='ok')throw Error(v.code||v.class);const data=v.result;state.textContent='Read-only view · '+data.total+' visible block(s) · '+data.observed_at;blocks.textContent=JSON.stringify(data.blocks,null,2)}catch(e){state.textContent=e.message;blocks.textContent=''}}
document.getElementById('refresh').addEventListener('click',refresh);setInterval(()=>{if(token.value)refresh()},5000);
</script>`
