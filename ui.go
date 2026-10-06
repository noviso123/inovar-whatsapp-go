package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

const panelTokenMarker = "__INJECTED_TOKEN__"

// panelCanAutoToken reports whether the panel may embed the API token in the
// page: when the operator opted in via PANEL_AUTO_TOKEN (the Docker Compose
// deploy publishes the port on 127.0.0.1 only), or when the request itself
// comes from a loopback address on a loopback-only listener.
func (a *app) panelCanAutoToken(r *http.Request) bool {
	if a.cfg.panelAutoToken {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *app) ui(w http.ResponseWriter, r *http.Request) {
	token := ""
	if a.panelCanAutoToken(r) {
		token = a.cfg.token
	}
	lit, err := json.Marshal(token)
	if err != nil {
		lit = []byte(`""`)
	}
	page := strings.ReplaceAll(uiHTML, panelTokenMarker, string(lit))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(page))
}

// uiHTML is the static local panel: a WhatsApp-like operator view (chats,
// contacts, own product catalog) plus session management. The page holds no
// secrets by itself; every call it makes carries the token injected for local
// access or pasted by the operator.
const uiHTML = `<!doctype html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>WhatsApp Go API — Painel</title>
<style>
  * { box-sizing: border-box; }
  body { font: 14px/1.45 system-ui, sans-serif; margin: 0; background: #eae6df; color: #111b21; height: 100vh; display: flex; flex-direction: column; }
  header { background: #0b3d2e; color: #fff; padding: 8px 14px; display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
  header h1 { font-size: 16px; margin: 0; }
  header .spacer { flex: 1; }
  header .hint { color: #bfe3d2; font-size: 12px; }
  header input { padding: 6px 8px; border: 0; border-radius: 6px; }
  header button { padding: 6px 10px; border: 0; border-radius: 6px; background: #0f5c43; color: #fff; font: inherit; cursor: pointer; }
  select, input, textarea, button { font: inherit; }
  #sess-select { padding: 6px 8px; border-radius: 6px; border: 0; max-width: 200px; }
  .pill { padding: 2px 10px; border-radius: 999px; font-size: 12px; font-weight: 600; background: #e4e9e6; color: #354b40; }
  .pill.ok { background: #d9f2e5; color: #087c52; }
  .pill.warn { background: #fdf0d5; color: #92600a; }
  .pill.bad { background: #f8dcd8; color: #a83232; }
  main { flex: 1; min-height: 0; display: flex; flex-direction: column; max-width: 1200px; width: 100%; margin: 0 auto; padding: 0; }

  section.panel { background: #fff; border: 1px solid #d7ded9; border-radius: 10px; margin: 10px; padding: 12px 14px; }
  h2 { font-size: 14px; margin: 0 0 8px; }
  .row { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
  .hint { color: #54656f; font-size: 12px; }
  table { width: 100%; border-collapse: collapse; }
  th, td { text-align: left; padding: 5px 8px; border-bottom: 1px solid #e9edef; vertical-align: top; font-size: 13px; }
  th { font-size: 11px; text-transform: uppercase; letter-spacing: .04em; color: #54656f; }
  button.act { padding: 5px 10px; border: 0; border-radius: 6px; background: #087c52; color: #fff; cursor: pointer; font-size: 13px; }
  button.sec { background: #354b40; }
  button.danger { background: #a83232; }
  button:disabled { opacity: .5; cursor: wait; }
  #qr-panel { display: none; text-align: center; padding: 10px; }
  #qr-panel img { image-rendering: pixelated; border-radius: 8px; background: #fff; }

  #app { flex: 1; min-height: 0; display: none; margin: 10px; background: #fff; border-radius: 10px; border: 1px solid #d7ded9; overflow: hidden; }
  #app.on { display: grid; grid-template-columns: 340px 1fr; }
  aside { border-right: 1px solid #e9edef; display: flex; flex-direction: column; min-height: 0; background: #fff; }
  .tabs { display: flex; border-bottom: 1px solid #e9edef; }
  .tabs button { flex: 1; border: 0; background: none; padding: 10px 4px; cursor: pointer; font-weight: 600; color: #54656f; border-bottom: 3px solid transparent; }
  .tabs button.on { color: #087c52; border-bottom-color: #087c52; }
  .list { flex: 1; overflow-y: auto; min-height: 0; }
  .item { padding: 9px 12px; border-bottom: 1px solid #f0f2f5; cursor: pointer; display: flex; gap: 8px; align-items: center; }
  .item:hover { background: #f5f6f6; }
  .item.on { background: #e7f5ee; }
  .item .who { flex: 1; min-width: 0; }
  .item .name { font-weight: 600; font-size: 13px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .item .sub { color: #667781; font-size: 12px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .item .when { color: #667781; font-size: 11px; white-space: nowrap; }
  .item .x { color: #a83232; border: 0; background: none; cursor: pointer; font-size: 14px; padding: 2px 6px; }
  .pane-form { border-top: 1px solid #e9edef; padding: 10px 12px; display: none; }
  .pane-form.on { display: block; }
  .pane-form input, .pane-form textarea { width: 100%; margin: 3px 0; padding: 7px 9px; border: 1px solid #d1d7db; border-radius: 6px; }
  .pane-form textarea { min-height: 46px; }

  .chat { display: flex; flex-direction: column; min-height: 0; background: #eae6df; }
  .chat-head { background: #f0f2f5; border-bottom: 1px solid #d9dbdc; padding: 9px 14px; }
  .chat-head .n { font-weight: 700; font-size: 14px; }
  .chat-head .j { color: #667781; font-size: 12px; }
  #chat-msgs { flex: 1; overflow-y: auto; padding: 14px 18px; display: flex; flex-direction: column; gap: 3px; min-height: 0; }
  .bubble { max-width: 72%; padding: 6px 10px; border-radius: 8px; background: #fff; box-shadow: 0 1px 1px rgba(0,0,0,.12); align-self: flex-start; font-size: 13.5px; overflow-wrap: anywhere; }
  .bubble.me { background: #d9fdd3; align-self: flex-end; }
  .bubble .meta { display: block; color: #667781; font-size: 10.5px; text-align: right; margin-top: 2px; }
  .bubble .media-tag { color: #087c52; font-weight: 700; font-size: 12px; }
  .chat-foot { background: #f0f2f5; padding: 9px 12px; display: flex; gap: 8px; }
  .chat-foot input { flex: 1; padding: 10px 12px; border: 1px solid #d1d7db; border-radius: 999px; }
  .chat-foot button { padding: 10px 18px; border: 0; border-radius: 999px; background: #087c52; color: #fff; font-weight: 700; cursor: pointer; }
  .placeholder { flex: 1; display: flex; align-items: center; justify-content: center; color: #8696a0; font-size: 14px; background: #eae6df; padding: 20px; text-align: center; }

  #log { font: 12px/1.6 ui-monospace, monospace; white-space: pre-wrap; overflow-wrap: anywhere; max-height: 130px; overflow: auto; background: #0f1913; color: #b8e6cf; border-radius: 8px; padding: 8px 10px; }
</style>
</head>
<body>
<header>
  <h1>WhatsApp Go API</h1>
  <select id="sess-select"><option value="">— carregando —</option></select>
  <span id="sess-pill" class="pill">…</span>
  <span class="spacer"></span>
  <span class="hint" id="hdr-hint"></span>
  <input id="token" type="password" placeholder="API_TOKEN" style="width:220px" autocomplete="off">
  <button id="save-token">Salvar token</button>
  <button id="toggle-manage">Gerenciar sessões</button>
</header>
<main>
  <section id="manage" class="panel">
    <h2>Sessões</h2>
    <div class="row" style="margin-bottom:8px">
      <input id="new-id" placeholder="id da nova sessão (ex.: loja-a)" autocomplete="off">
      <button id="create" class="act">Criar e conectar (mostra QR)</button>
    </div>
    <table>
      <thead><tr><th>ID</th><th>Status</th><th>Conta (JID)</th><th>Ações</th></tr></thead>
      <tbody id="rows"></tbody>
    </table>
    <div class="row" style="margin-top:12px;border-top:1px solid #e3e9e5;padding-top:10px">
      <strong style="font-size:13px">Webhook — entrega de eventos (CRM ou outro consumidor)</strong>
      <span class="hint" id="wh-status"></span>
    </div>
    <div class="row" style="margin-top:6px">
      <input id="wh-url" placeholder="URL do webhook (https ou http interno)" style="flex:1;min-width:240px" autocomplete="off">
      <input id="wh-secret" type="password" placeholder="secret HMAC (vazio = manter)" style="width:190px" autocomplete="new-password">
      <input id="wh-token" type="password" placeholder="X-Webhook-Token (vazio = manter)" style="width:190px" autocomplete="new-password">
      <button id="wh-save" class="act">Salvar webhook</button>
      <button id="wh-clear" class="act sec">Limpar (usa o ambiente)</button>
    </div>
    <div id="qr-panel">
      <h2>QR de pareamento — <span id="qr-session"></span></h2>
      <img id="qr-img" width="232" height="232" alt="QR de pareamento">
      <p class="hint">Escaneie em WhatsApp &gt; Aparelhos conectados &gt; Conectar aparelho. O QR se renova sozinho.</p>
      <button id="qr-close" class="act sec">Fechar</button>
    </div>
  </section>

  <section id="app">
    <aside>
      <div class="tabs">
        <button id="tab-chats" class="on">Conversas</button>
        <button id="tab-contacts">Contatos</button>
        <button id="tab-products">Catálogo</button>
      </div>
      <div id="list-chats" class="list"></div>
      <div id="list-contacts" class="list" style="display:none"></div>
      <div id="list-products" class="list" style="display:none"></div>
      <div id="form-products" class="pane-form">
        <h2>Novo produto (catálogo local)</h2>
        <input id="p-name" placeholder="nome *">
        <input id="p-price" placeholder="preço (ex.: 79.90)">
        <input id="p-sku" placeholder="SKU/código">
        <input id="p-image" placeholder="URL da imagem (https)">
        <textarea id="p-desc" placeholder="descrição"></textarea>
        <button id="p-add" class="act">Cadastrar produto</button>
        <p class="hint">Catálogo próprio deste sistema, salvo no banco local. O catálogo do app WhatsApp Business é gerenciado pelo próprio app; o protocolo não permite cadastrá-lo remotamente.</p>
      </div>
    </aside>
    <div class="chat" id="chat-pane">
      <div id="chat-empty" class="placeholder">Selecione uma conversa ou contato à esquerda.<br>Digite o nome da sessão acima, crie-a em “Gerenciar sessões” e escaneie o QR.</div>
      <div id="chat-live" style="display:none; flex:1; min-height:0; flex-direction:column;">
        <div class="chat-head"><div class="n" id="chat-name">—</div><div class="j" id="chat-jid"></div></div>
        <div id="chat-msgs"></div>
        <div class="chat-foot">
          <input id="chat-input" placeholder="Digite uma mensagem e pressione Enter" autocomplete="off">
          <button id="send-btn">Enviar</button>
        </div>
      </div>
    </div>
  </section>

  <section class="panel">
    <h2>Registro</h2>
    <div id="log">Carregando…</div>
  </section>
</main>
<script>
const $ = (id) => document.getElementById(id);
const injectedToken = __INJECTED_TOKEN__;
let token = localStorage.getItem("api_token") || "";
if (injectedToken) { token = injectedToken; localStorage.setItem("api_token", token); }
$("token").value = token;

let sessions = [], sid = "", contacts = {}, currentChat = "", lastMsgsJSON = "", lastChatsJSON = "";
let prevStatus = {}, qrSession = "", lastQRUrl = "", activeTab = "chats";
let lastSessionsJSON = "", manageTouched = false;

function log(m) { const el = $("log"); el.textContent = "[" + new Date().toLocaleTimeString() + "] " + m + "\n" + el.textContent; }
function headers(json) { const h = { Authorization: "Bearer " + token }; if (json) h["Content-Type"] = "application/json"; return h; }
async function api(path, opts) {
  opts = opts || {};
  const res = await fetch(path, Object.assign({ cache: "no-store", headers: headers(opts.json) }, opts));
  const body = await res.json().catch(() => ({}));
  if (!res.ok) { if (res.status === 401) log("Token inválido (401)."); throw new Error((body.error && body.error.message) || ("HTTP " + res.status)); }
  return body;
}
function escapeHTML(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
function timeStr(ts) { const d = new Date(Number(ts) * 1000); return isNaN(d) ? "" : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }); }
function contactName(jid) { return contacts[jid] || jid.split("@")[0]; }
function pillClass(st) { return { connected: "ok", pairing: "warn", connecting: "warn", reconnecting: "warn" }[st] || (String(st).indexOf("passkey") === 0 ? "warn" : "bad"); }

async function loadSessions(keepQuiet) {
  try {
    sessions = await api("/v1/sessions");
    const j = JSON.stringify(sessions);
    const sel = $("sess-select");
    if (j !== lastSessionsJSON) {
      lastSessionsJSON = j;
      renderSessionsTable();
      sel.innerHTML = '<option value="">— escolha a sessão —</option>' + sessions.map((s) => '<option value="' + escapeHTML(s.id) + '">' + escapeHTML(s.id) + "</option>").join("");
      const connected = sessions.filter((s) => s.status === "connected");
      if (!sid && connected.length === 1) { sel.value = connected[0].id; await selectSession(connected[0].id); }
    }
    if (sid && sessions.some((s) => s.id === sid)) { if (sel.value !== sid) sel.value = sid; }
    else if (sid) { sid = ""; }
    const cur = sessions.find((s) => s.id === (sel.value || ""));
    const pill = $("sess-pill");
    if (cur) { pill.className = "pill " + pillClass(cur.status); pill.textContent = cur.status; } else { pill.className = "pill"; pill.textContent = "sem sessão"; }
    $("app").classList.toggle("on", !!cur && cur.status === "connected");
    $("hdr-hint").textContent = cur && cur.status === "connected" ? "operando como " + cur.account : "conecte uma sessão para operar";
    if (!manageTouched) $("manage").style.display = sessions.some((s) => s.status === "connected") ? "none" : "block";
    return cur;
  } catch (e) { if (!keepQuiet) log(e.message); return null; }
}
async function selectSession(id) {
  sid = id;
  currentChat = ""; lastMsgsJSON = ""; lastChatsJSON = "";
  $("chat-live").style.display = "none"; $("chat-empty").style.display = "flex";
  if (sid) { contacts = {}; await loadContacts(); await loadChats(); if (activeTab === "products") loadProducts(); }
}
function renderSessionsTable() {
  const tbody = $("rows");
  if (!sessions.length) { tbody.innerHTML = '<tr><td colspan="4" class="hint">Nenhuma sessão ainda.</td></tr>'; return; }
  tbody.innerHTML = sessions.map((s) => {
    return "<tr><td><strong>" + escapeHTML(s.id) + '</strong></td><td><span class="pill ' + pillClass(s.status) + '">' + escapeHTML(s.status) + "</span></td><td>" + escapeHTML(s.account || "—") + '</td><td><button data-act="connect" data-id="' + escapeHTML(s.id) + '" class="act">Conectar</button> <button data-act="qr" data-id="' + escapeHTML(s.id) + '" class="act sec">QR</button> <button data-act="pair" data-id="' + escapeHTML(s.id) + '" class="act sec">Código</button> <button data-act="disconnect" data-id="' + escapeHTML(s.id) + '" class="act sec">Desconectar</button> <button data-act="delete" data-id="' + escapeHTML(s.id) + '" class="act danger">Remover</button></td></tr>';
  }).join("");
}
async function showQR(id, attempts, silent) {
  attempts = attempts || 1;
  for (let i = 0; i < attempts; i++) {
    const res = await fetch("/v1/sessions/" + encodeURIComponent(id) + "/pairing?format=png", { cache: "no-store", headers: headers(false) });
    if (res.ok) {
      const blob = await res.blob();
      if (lastQRUrl) URL.revokeObjectURL(lastQRUrl);
      lastQRUrl = URL.createObjectURL(blob);
      $("qr-img").src = lastQRUrl; $("qr-session").textContent = id;
      $("qr-panel").style.display = "block"; $("manage").style.display = "block"; qrSession = id;
      return true;
    }
    if (res.status === 404 && i < attempts - 1) { await new Promise((r) => setTimeout(r, 2000)); continue; }
    if (!silent) log(id + ": sem QR agora (HTTP " + res.status + "). Clique em Conectar e aguarde o status pairing.");
    return false;
  }
  return false;
}
async function connectAndShowQR(id) {
  try {
    const r = await api("/v1/sessions/" + encodeURIComponent(id) + "/connect", { method: "POST" });
    log(id + ": connect → " + r.status);
    await loadSessions(true);
    if (r.status === "pairing") { await showQR(id, 1, false); return; }
    if (r.status === "connected") return;
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 2000));
      const s = await api("/v1/sessions/" + encodeURIComponent(id));
      await loadSessions(true);
      if (s.status === "pairing") { await showQR(id, 3, false); return; }
      if (s.status === "connected") return;
      if (["logged_out", "temporarily_banned", "client_outdated", "stream_replaced", "connection_failed", "pairing_failed"].indexOf(s.status) >= 0) return;
    }
    await showQR(id, 1, false);
  } catch (e) { log("Erro ao conectar " + id + ": " + e.message); }
}

async function loadContacts() {
  if (!sid) return;
  try {
    const list = await api("/v1/sessions/" + encodeURIComponent(sid) + "/contacts");
    contacts = {};
    list.forEach((c) => { contacts[c.jid] = c.push_name || c.full_name || c.first_name || c.jid.split("@")[0]; });
    renderContacts();
  } catch (e) { log("Contatos: " + e.message); }
}
function renderContacts() {
  const el = $("list-contacts");
  const jids = Object.keys(contacts).sort((a, b) => contacts[a].localeCompare(contacts[b]));
  el.innerHTML = jids.length ? jids.map((j) => '<div class="item" data-jid="' + escapeHTML(j) + '"><div class="who"><div class="name">' + escapeHTML(contacts[j]) + '</div><div class="sub">' + escapeHTML(j) + "</div></div></div>").join("") : '<div class="item hint">Nenhum contato no cache local ainda.</div>';
}
async function loadChats() {
  if (!sid) return;
  try {
    const list = await api("/v1/sessions/" + encodeURIComponent(sid) + "/chats");
    const j = JSON.stringify(list);
    if (j === lastChatsJSON) return;
    lastChatsJSON = j;
    const el = $("list-chats");
    el.innerHTML = list.length ? list.map((c) => {
      const label = c.chat.indexOf("@g.us") >= 0 ? c.chat.split("@")[0] + " (grupo)" : contactName(c.chat);
      const sub = (c.last_media_type ? "[" + c.last_media_type + "] " : "") + c.last_text;
      return '<div class="item' + (currentChat === c.chat ? " on" : "") + '" data-jid="' + escapeHTML(c.chat) + '"><div class="who"><div class="name">' + escapeHTML(label) + '</div><div class="sub">' + escapeHTML(sub) + '</div></div><div class="when">' + timeStr(c.last_timestamp) + "</div></div>";
    }).join("") : '<div class="item hint">Sem conversas registradas ainda. Elas aparecem aqui quando mensagens chegarem ou forem enviadas.</div>';
  } catch (e) { log("Conversas: " + e.message); }
}
async function openChat(jid) {
  currentChat = jid;
  lastMsgsJSON = "";
  $("chat-empty").style.display = "none";
  $("chat-live").style.display = "flex";
  $("chat-name").textContent = jid.indexOf("@g.us") >= 0 ? jid : contactName(jid);
  $("chat-jid").textContent = jid;
  await loadMessages();
  loadChats();
}
async function loadMessages() {
  if (!sid || !currentChat) return;
  try {
    const list = await api("/v1/sessions/" + encodeURIComponent(sid) + "/chats/" + encodeURIComponent(currentChat));
    const j = JSON.stringify(list);
    if (j === lastMsgsJSON) return;
    lastMsgsJSON = j;
    const el = $("chat-msgs");
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
    el.innerHTML = list.map((m) => {
      const tag = m.media_type ? '<span class="media-tag">[' + escapeHTML(m.media_type) + "]</span> " : "";
      return '<div class="bubble ' + (m.from_me ? "me" : "") + '">' + tag + escapeHTML(m.text) + '<span class="meta">' + (m.from_me ? "você · " : "") + timeStr(m.timestamp) + "</span></div>";
    }).join("") || '<div class="placeholder">Nenhuma mensagem nesta conversa ainda.</div>';
    if (nearBottom) el.scrollTop = el.scrollHeight;
  } catch (e) { log("Mensagens: " + e.message); }
}
async function sendMessage() {
  const input = $("chat-input");
  const text = input.value.trim();
  if (!sid || !currentChat || !text) return;
  try {
    await api("/v1/sessions/" + encodeURIComponent(sid) + "/messages", { method: "POST", json: true, body: JSON.stringify({ to: currentChat, text: text }) });
    input.value = "";
    lastMsgsJSON = "";
    await loadMessages();
    lastChatsJSON = "";
    await loadChats();
  } catch (e) { log("Enviar: " + e.message); }
}
async function loadProducts() {
  if (!sid) return;
  try {
    const list = await api("/v1/sessions/" + encodeURIComponent(sid) + "/products");
    $("list-products").innerHTML = list.length ? list.map((p) => '<div class="item"><div class="who"><div class="name">' + escapeHTML(p.name) + (p.sku ? " · " + escapeHTML(p.sku) : "") + '</div><div class="sub">' + (p.price ? escapeHTML(p.price) + " " + escapeHTML(p.currency) : "") + escapeHTML(p.description ? " — " + p.description : "") + "</div></div>" + (p.image_url ? '<img src="' + escapeHTML(p.image_url) + '" width="34" height="34" style="border-radius:6px;object-fit:cover">' : "") + '<button class="x" data-pid="' + escapeHTML(p.id) + '" title="excluir">✕</button></div>').join("") : '<div class="item hint">Catálogo vazio. Cadastre o primeiro produto abaixo.</div>';
  } catch (e) { log("Catálogo: " + e.message); }
}
function setTab(t) {
  activeTab = t;
  ["chats", "contacts", "products"].forEach((k) => {
    $("tab-" + k).classList.toggle("on", k === t);
    $("list-" + k).style.display = k === t ? "block" : "none";
    $("form-products").classList.toggle("on", t === "products");
  });
  if (t === "contacts") loadContacts();
  if (t === "products") loadProducts();
}

$("tab-chats").addEventListener("click", () => setTab("chats"));
$("tab-contacts").addEventListener("click", () => setTab("contacts"));
$("tab-products").addEventListener("click", () => setTab("products"));
$("list-contacts").addEventListener("click", (ev) => { const it = ev.target.closest(".item[data-jid]"); if (it) openChat(it.dataset.jid); });
$("list-chats").addEventListener("click", (ev) => { const it = ev.target.closest(".item[data-jid]"); if (it) openChat(it.dataset.jid); });
$("list-products").addEventListener("click", async (ev) => {
  const x = ev.target.closest("button[data-pid]");
  if (!x) return;
  try { await api("/v1/sessions/" + encodeURIComponent(sid) + "/products/" + encodeURIComponent(x.dataset.pid), { method: "DELETE" }); loadProducts(); log("Produto excluído."); }
  catch (e) { log("Excluir produto: " + e.message); }
});
$("p-add").addEventListener("click", async () => {
  const body = { name: $("p-name").value.trim(), price: $("p-price").value.trim(), sku: $("p-sku").value.trim(), image_url: $("p-image").value.trim(), description: $("p-desc").value.trim() };
  if (!body.name) { log("Informe o nome do produto."); return; }
  try {
    await api("/v1/sessions/" + encodeURIComponent(sid) + "/products", { method: "POST", json: true, body: JSON.stringify(body) });
    $("p-name").value = ""; $("p-price").value = ""; $("p-sku").value = ""; $("p-image").value = ""; $("p-desc").value = "";
    log("Produto cadastrado no catálogo local.");
    loadProducts();
  } catch (e) { log("Cadastrar produto: " + e.message); }
});
$("send-btn").addEventListener("click", sendMessage);
$("chat-input").addEventListener("keydown", (ev) => { if (ev.key === "Enter") { ev.preventDefault(); sendMessage(); } });
$("sess-select").addEventListener("change", () => selectSession($("sess-select").value));
$("toggle-manage").addEventListener("click", () => { manageTouched = true; const m = $("manage"); m.style.display = m.style.display === "none" ? "block" : "none"; });
$("save-token").addEventListener("click", () => { token = $("token").value.trim(); localStorage.setItem("api_token", token); log("Token salvo neste navegador."); loadSessions(); });
$("create").addEventListener("click", async () => {
  const id = $("new-id").value.trim();
  if (!id) { log("Informe um id para a nova sessão."); return; }
  try {
    await api("/v1/sessions", { method: "POST", json: true, body: JSON.stringify({ id: id }) });
    log("Sessão criada: " + id + ". Conectando…");
    $("new-id").value = "";
    await loadSessions(true);
    await connectAndShowQR(id);
  } catch (e) { log("Erro ao criar: " + e.message); }
});
$("rows").addEventListener("click", async (ev) => {
  const btn = ev.target.closest("button[data-act]");
  if (!btn) return;
  const id = btn.dataset.id, act = btn.dataset.act;
  btn.disabled = true;
  try {
    if (act === "connect") await connectAndShowQR(id);
    else if (act === "qr") await showQR(id, 1, false);
    else if (act === "pair") {
      const phone = prompt("Telefone internacional sem zero inicial (ex.: 5511999999999):");
      if (phone) {
        const r = await api("/v1/sessions/" + encodeURIComponent(id) + "/pair", { method: "POST", json: true, body: JSON.stringify({ phone: phone }) });
        log(id + ": código " + r.code + " — digite no WhatsApp do titular.");
      }
    } else if (act === "disconnect") { await api("/v1/sessions/" + encodeURIComponent(id) + "/disconnect", { method: "POST" }); log(id + ": desconectada."); await loadSessions(); }
    else if (act === "delete") {
      if (confirm("Remover a sessão " + id + "? Faz logout do dispositivo e apaga credenciais locais.")) { await api("/v1/sessions/" + encodeURIComponent(id), { method: "DELETE" }); log(id + ": removida."); if (sid === id) { sid = ""; } await loadSessions(); }
    }
  } catch (e) { log("Erro (" + act + " " + id + "): " + e.message); }
  btn.disabled = false;
});
$("qr-close").addEventListener("click", () => { $("qr-panel").style.display = "none"; qrSession = ""; });
async function loadWebhookSettings() {
  try {
    const s = await api("/v1/settings/webhook");
    $("wh-url").value = s.webhook_url || "";
    $("wh-status").textContent = (s.webhook_secret_set ? "secret ✓ · " : "secret — · ") + (s.webhook_token_set ? "token ✓ · " : "token — · ") + (s.stored_in_db ? "salvo no banco" : "usando ambiente");
  } catch (e) { $("wh-status").textContent = "erro: " + e.message; }
}
$("wh-save").addEventListener("click", async () => {
  const body = { webhook_url: $("wh-url").value.trim() };
  if ($("wh-secret").value) body.webhook_secret = $("wh-secret").value;
  if ($("wh-token").value) body.webhook_token = $("wh-token").value;
  try {
    const s = await api("/v1/settings/webhook", { method: "POST", json: true, body: JSON.stringify(body) });
    $("wh-secret").value = ""; $("wh-token").value = "";
    $("wh-status").textContent = "Webhook salvo no banco. Entrega ativa para: " + (s.webhook_url || "—");
    log("Configuração de webhook salva.");
  } catch (e) { log("Salvar webhook: " + e.message); }
});
$("wh-clear").addEventListener("click", async () => {
  try {
    await api("/v1/settings/webhook", { method: "POST", json: true, body: JSON.stringify({ webhook_url: "", webhook_secret: "", webhook_token: "" }) });
    log("Configurações de webhook limpas; usando valores do ambiente.");
    await loadWebhookSettings();
  } catch (e) { log("Limpar webhook: " + e.message); }
});

setInterval(async () => {
  const cur = await loadSessions(true);
  if (cur && cur.status === "connected" && sid) { await loadChats(); if (currentChat) await loadMessages(); }
}, 3000);
(async () => {
  if (!injectedToken && !token) log("Painel carregado. Cole o API_TOKEN e clique em Salvar token.");
  else log("Painel pronto. Escolha a sessão no topo; gerencie pareamento em “Gerenciar sessões”.");
  await loadSessions();
  await loadWebhookSettings();
  setTab("chats");
})();
</script>
</body>
</html>`
