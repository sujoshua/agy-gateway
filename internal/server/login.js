"use strict";
const $ = (id) => document.getElementById(id);

function show(id, text, cls) {
  const el = $(id);
  el.textContent = text;
  el.className = "msg " + (cls || "");
}

async function call(path, body) {
  const headers = { "Content-Type": "application/json" };
  const key = $("key").value.trim();
  if (key) headers.Authorization = "Bearer " + key;
  const resp = await fetch(path, { method: "POST", headers, body: JSON.stringify(body || {}) });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error((data.error && data.error.message) || "HTTP " + resp.status);
  return data;
}

async function status() {
  try {
    const data = await (await fetch("/readyz")).json();
    $("status").textContent = data.ready ? "The runtime is signed in and ready." : "Runtime status: " + data.status;
    $("status").className = data.ready ? "ok" : "";
  } catch {
    $("status").textContent = "Could not reach /readyz.";
  }
}

$("start").addEventListener("click", async () => {
  $("start").disabled = true;
  show("start-msg", "Asking the runtime for a sign-in link (this can take a few seconds)…");
  try {
    const { auth_url: url } = await call("/login/start");
    const a = document.createElement("a");
    a.href = url;
    a.target = "_blank";
    a.rel = "noopener noreferrer";
    a.textContent = "Open the Google sign-in page";
    $("start-msg").replaceChildren(a);
    $("start-msg").className = "msg";
  } catch (e) {
    show("start-msg", e.message, "bad");
  } finally {
    $("start").disabled = false;
  }
});

$("finish").addEventListener("click", async () => {
  $("finish").disabled = true;
  show("finish-msg", "Signing in and loading the models…");
  try {
    const { tier } = await call("/login/finish", { callback_url: $("cb").value.trim() });
    show("finish-msg", "Signed in" + (tier ? " (" + tier + ")" : "") + ".", "ok");
    $("cb").value = "";
    status();
  } catch (e) {
    show("finish-msg", e.message, "bad");
  } finally {
    $("finish").disabled = false;
  }
});

status();
