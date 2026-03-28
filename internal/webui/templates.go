package webui

// pageShell wraps a page body with the shared chrome (nav, CSS).
func pageShell(body string) string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>torSync — {{.Title}}</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;min-height:100vh}
a{color:#818cf8;text-decoration:none}a:hover{text-decoration:underline}
nav{background:#1e293b;border-bottom:1px solid #334155;padding:0 1.5rem;display:flex;align-items:center;gap:2rem;height:56px}
nav .brand{font-weight:700;color:#fff;font-size:1.1rem}
nav a{color:#94a3b8;font-size:.9rem;padding:.25rem .5rem;border-radius:.375rem}
nav a:hover,nav a.active{color:#e2e8f0;background:#334155;text-decoration:none}
main{max-width:1100px;margin:0 auto;padding:2rem 1.5rem}
h1{font-size:1.5rem;font-weight:700;color:#f1f5f9;margin-bottom:1.5rem}
h2{font-size:1.1rem;font-weight:600;color:#e2e8f0;margin-bottom:1rem}
.card{background:#1e293b;border:1px solid #334155;border-radius:.75rem;padding:1.25rem}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:1rem;margin-bottom:2rem}
.stat{background:#1e293b;border:1px solid #334155;border-radius:.75rem;padding:1rem}
.stat-label{font-size:.75rem;color:#64748b;text-transform:uppercase;letter-spacing:.05em;margin-bottom:.25rem}
.stat-value{font-size:1.5rem;font-weight:700;color:#f1f5f9}
.stat-sub{font-size:.8rem;color:#94a3b8;margin-top:.2rem}
table{width:100%;border-collapse:collapse;font-size:.9rem}
th{text-align:left;padding:.6rem .75rem;color:#64748b;font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;border-bottom:1px solid #334155}
td{padding:.65rem .75rem;border-bottom:1px solid #1e293b;color:#cbd5e1}
tr:last-child td{border-bottom:none}
tr:hover td{background:#1e293b}
.table-wrap{background:#1e293b;border:1px solid #334155;border-radius:.75rem;overflow:hidden}
.badge{display:inline-flex;align-items:center;border-radius:9999px;padding:.15rem .6rem;font-size:.75rem;font-weight:500}
.badge-green{background:#166534;color:#86efac}
.badge-red{background:#7f1d1d;color:#fca5a5}
.badge-yellow{background:#78350f;color:#fcd34d}
.badge-gray{background:#1e293b;color:#94a3b8;border:1px solid #334155}
.badge-indigo{background:#312e81;color:#a5b4fc;border:1px solid #4338ca}
.alert{padding:.75rem 1rem;border-radius:.5rem;font-size:.875rem;margin-bottom:1rem}
.alert-red{background:#7f1d1d22;border:1px solid #7f1d1d;color:#fca5a5}
.alert-green{background:#16653422;border:1px solid #166534;color:#86efac}
.alert-yellow{background:#78350f22;border:1px solid #78350f;color:#fcd34d}
form .field{margin-bottom:1rem}
form label{display:block;font-size:.85rem;color:#94a3b8;margin-bottom:.35rem}
form input[type=text],form input[type=number],form input[type=password],form input[type=url]{
  width:100%;background:#0f172a;border:1px solid #334155;border-radius:.5rem;
  padding:.5rem .75rem;color:#e2e8f0;font-size:.9rem;outline:none}
form input:focus{border-color:#6366f1;box-shadow:0 0 0 2px #6366f133}
.btn{display:inline-flex;align-items:center;border-radius:.5rem;padding:.45rem 1rem;font-size:.875rem;font-weight:500;cursor:pointer;border:none;transition:background .15s}
.btn-primary{background:#4f46e5;color:#fff}.btn-primary:hover{background:#4338ca}
.btn-secondary{background:#334155;color:#e2e8f0}.btn-secondary:hover{background:#475569}
.btn-danger{background:#7f1d1d;color:#fca5a5}.btn-danger:hover{background:#991b1b}
.btn-sm{padding:.3rem .6rem;font-size:.8rem}
.flex{display:flex}.gap-2{gap:.5rem}.gap-3{gap:.75rem}.items-center{align-items:center}
.justify-between{justify-content:space-between}.mt-4{margin-top:1rem}.mb-4{margin-bottom:1rem}
.text-sm{font-size:.875rem}.text-xs{font-size:.75rem}.text-gray{color:#64748b}
.mono{font-family:monospace;font-size:.85rem}
.free-banner{background:#312e8133;border:1px solid #4338ca;border-radius:.75rem;padding:1rem;margin-bottom:1.5rem}
.free-banner p{color:#a5b4fc;font-size:.9rem}
.dir-list{list-style:none;margin-top:.5rem}
.dir-item{display:flex;align-items:center;justify-content:space-between;padding:.5rem .75rem;background:#0f172a;border-radius:.5rem;margin-bottom:.5rem}
.dir-path{font-family:monospace;font-size:.85rem;color:#93c5fd}
</style>
</head>
<body>
<nav>
  <span class="brand">torSync</span>
  <a href="/">Dashboard</a>
  <a href="/files">Files</a>
  <a href="/peers">Peers</a>
  <a href="/settings">Settings</a>
</nav>
<main>
` + body + `
</main>
</body>
</html>`
}

const pageDashboard = `
<h1>Dashboard</h1>

{{if .IsFree}}
<div class="free-banner">
  <p><strong>Free tier</strong> — up to 2 nodes and 1 sync directory. <a href="/settings">Import a license key</a> to unlock more.</p>
</div>
{{end}}

<div class="cards">
  <div class="stat">
    <div class="stat-label">License</div>
    <div class="stat-value">
      {{if eq .LicStatus "active"}}<span class="badge badge-green">active</span>
      {{else if eq .LicStatus "grace"}}<span class="badge badge-yellow">grace</span>
      {{else if eq .LicStatus "free"}}<span class="badge badge-indigo">free</span>
      {{else}}<span class="badge badge-red">{{.LicStatus}}</span>{{end}}
    </div>
    {{if not .IsFree}}<div class="stat-sub">Expires {{.ExpiresAt}}</div>{{end}}
  </div>
  <div class="stat">
    <div class="stat-label">Peers</div>
    <div class="stat-value">{{.PeerCount}}</div>
    <div class="stat-sub">Max {{.MaxNodes}} nodes</div>
  </div>
  <div class="stat">
    <div class="stat-label">Node</div>
    <div class="stat-value" style="font-size:1rem">{{.NodeName}}</div>
    <div class="stat-sub mono">{{slice .NodeID 0 8}}…</div>
  </div>
  <div class="stat">
    <div class="stat-label">Uptime</div>
    <div class="stat-value" style="font-size:1.1rem">{{.Uptime}}</div>
  </div>
</div>

<h2>Sync Directories</h2>
<div class="table-wrap">
  <table>
    <thead><tr><th>Path</th></tr></thead>
    <tbody>
    {{range .SyncDirs}}
    <tr><td class="mono">{{.Path}}</td></tr>
    {{else}}
    <tr><td class="text-gray">No sync directories configured. <a href="/settings">Add one →</a></td></tr>
    {{end}}
    </tbody>
  </table>
</div>
`

const pageFiles = `
<h1>Files</h1>
<div class="table-wrap">
  <table>
    <thead><tr><th>Path</th><th>Size</th><th>Status</th><th>Modified</th></tr></thead>
    <tbody>
    {{range .Files}}
    <tr>
      <td class="mono">{{.Path}}</td>
      <td class="text-sm">{{.Size}}</td>
      <td>{{if .Deleted}}<span class="badge badge-gray">deleted</span>{{else}}<span class="badge badge-green">synced</span>{{end}}</td>
      <td class="text-sm text-gray">{{timeAgo .UpdatedAt}}</td>
    </tr>
    {{else}}
    <tr><td colspan="4" class="text-gray" style="padding:2rem;text-align:center">No files tracked yet.</td></tr>
    {{end}}
    </tbody>
  </table>
</div>
`

const pagePeers = `
<h1>Peers</h1>

{{if .Peers}}
<h2>Local gossip peers</h2>
<div class="table-wrap mb-4">
  <table>
    <thead><tr><th>Node ID</th><th>Address</th><th>Last Seen</th></tr></thead>
    <tbody>
    {{range .Peers}}
    <tr>
      <td class="mono">{{slice .NodeID 0 8}}…</td>
      <td class="mono">{{.PublicAddr}}</td>
      <td class="text-sm text-gray">{{timeAgo .LastSeen}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
</div>
{{end}}

{{if .SSCPeers}}
<h2>SSC-discovered peers</h2>
<div class="table-wrap">
  <table>
    <thead><tr><th>Instance ID</th><th>Address</th></tr></thead>
    <tbody>
    {{range .SSCPeers}}
    <tr>
      <td class="mono">{{slice .InstanceID 0 8}}…</td>
      <td class="mono">{{.PublicAddress}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
</div>
{{end}}

{{if and (not .Peers) (not .SSCPeers)}}
<div class="card text-gray" style="text-align:center;padding:3rem">No peers connected yet.</div>
{{end}}
`

const pageSettings = `
<h1>Settings</h1>

{{if .Error}}
<div class="alert alert-red">{{.Error}}</div>
{{end}}

{{with .Success}}
<div class="alert alert-green">Settings saved.</div>
{{end}}

<div style="display:grid;grid-template-columns:1fr 1fr;gap:1.5rem">

<!-- Sync directories -->
<div class="card">
  <h2>Sync Directories</h2>
  {{if .IsFree}}<p class="text-xs text-gray mb-4">Free tier: max 1 directory.</p>{{end}}
  <ul class="dir-list">
  {{range .Config.SyncDirs}}
  <li class="dir-item">
    <span class="dir-path">{{.Path}}</span>
    <form method="POST" action="/settings/sync-dir/remove" style="display:inline">
      <input type="hidden" name="path" value="{{.Path}}">
      <button type="submit" class="btn btn-danger btn-sm">Remove</button>
    </form>
  </li>
  {{else}}
  <li class="text-gray text-sm">No directories configured.</li>
  {{end}}
  </ul>
  {{if or (not .IsFree) (lt (len .Config.SyncDirs) 1)}}
  <form method="POST" action="/settings/sync-dir" class="flex gap-2 mt-4" style="align-items:flex-end">
    <div style="flex:1">
      <label>Add directory</label>
      <input type="text" name="path" placeholder="/data/sync" required>
    </div>
    <button type="submit" class="btn btn-primary">Add</button>
  </form>
  {{end}}
</div>

<!-- SSC connection -->
<div class="card">
  <h2>SSC Connection</h2>
  <form method="POST" action="/settings">
    <div class="field">
      <label>SSC URL</label>
      <input type="url" name="ssc_url" value="{{.Config.SscURL}}" placeholder="https://ssc.example.com">
    </div>
    <div class="field">
      <label>Web UI Port</label>
      <input type="number" name="web_port" value="{{.Config.WebPort}}" min="1" max="65535">
    </div>
    <button type="submit" class="btn btn-primary">Save</button>
  </form>
</div>

<!-- Change password -->
<div class="card">
  <h2>Change Password</h2>
  <form method="POST" action="/settings">
    <div class="field">
      <label>New Password</label>
      <input type="password" name="new_password" autocomplete="new-password">
    </div>
    <div class="field">
      <label>Confirm Password</label>
      <input type="password" name="confirm_password" autocomplete="new-password">
    </div>
    <button type="submit" class="btn btn-primary">Change Password</button>
  </form>
</div>

<!-- Node info -->
<div class="card">
  <h2>Node Info</h2>
  <table style="width:100%">
    <tr><td class="text-gray text-sm">Node ID</td><td class="mono text-xs">{{.Config.NodeID}}</td></tr>
    <tr><td class="text-gray text-sm">Node Name</td><td>{{.Config.NodeName}}</td></tr>
    <tr><td class="text-gray text-sm">Data Dir</td><td class="mono text-xs">{{.Config.DataDir}}</td></tr>
    <tr><td class="text-gray text-sm">License Key</td><td class="mono text-xs">{{.Config.LicenseKeyPath}}</td></tr>
  </table>
</div>

</div>
`
