// agentcfg settings page. Every value from the server goes into the DOM via
// textContent; nothing is built from HTML strings.
"use strict";

(function () {
  const KEY_STORE = "agentcfg.key";
  let key = "";
  let state = null;
  const tests = {}; // "name.env" -> {ok, text}

  // The session key arrives in the URL fragment. Keep it for reloads of this
  // tab only, then take it out of the address bar.
  const m = /(?:^|[#&])k=([0-9a-f]{64})/.exec(location.hash);
  if (m) {
    key = m[1];
    try { sessionStorage.setItem(KEY_STORE, key); } catch (_) { /* private mode */ }
    history.replaceState(null, "", location.pathname);
  } else {
    try { key = sessionStorage.getItem(KEY_STORE) || ""; } catch (_) { key = ""; }
  }

  const $ = (id) => document.getElementById(id);

  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") el.className = v;
      else if (k === "text") el.textContent = v;
      else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? "" : String(v));
    }
    for (const kid of kids.flat()) {
      if (kid === null || kid === undefined || kid === false) continue;
      el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
    }
    return el;
  }

  async function api(method, path, body) {
    const opts = { method, headers: { "X-Agentcfg-Key": key } };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetch(path, opts);
    } catch (_) {
      throw new Error("agentcfg is not running. Start it again with: agentcfg ui");
    }
    let data = {};
    try { data = await res.json(); } catch (_) { /* empty body */ }
    if (!res.ok) {
      const err = new Error(data.error || "HTTP " + res.status);
      err.status = res.status;
      throw err;
    }
    return data;
  }

  function toast(text, bad) {
    const t = h("div", { class: "toast" + (bad ? " bad" : ""), role: bad ? "alert" : "status", text });
    $("toasts").append(t);
    setTimeout(() => t.remove(), bad ? 7000 : 4000);
  }

  // Writes carry the version they were based on (the current one unless the
  // caller passes the version its form was built from); a 409 means the file
  // changed underneath us, so reload and let the person redo the edit.
  async function write(path, body, version) {
    try {
      const v = version !== undefined ? version : (state ? state.version : "");
      const next = await api("POST", path, Object.assign({ version: v }, body));
      apply(next);
      (next.notes || []).forEach((n) => toast(n));
      return true;
    } catch (e) {
      if (e.status === 409) {
        await load();
        throw new Error("services.toml changed on disk, so the page reloaded. Check it and try again.");
      }
      throw e;
    }
  }

  // Focus keys. render() rebuilds the cards, so the buttons a person was on
  // are new nodes afterwards. Controls that must keep focus across a render
  // carry data-focus; pendingFocus names where focus goes after the next
  // render (first key that exists and is enabled wins).
  let pendingFocus = null;
  function findFocus(k) {
    if (!k) return null;
    return [...document.querySelectorAll("[data-focus]")].find((el) => el.dataset.focus === k) || null;
  }
  function focusKeys(keys) {
    for (const k of keys || []) {
      const el = findFocus(k);
      if (el && !el.disabled) { el.focus(); return true; }
    }
    return false;
  }
  // write, then put focus on the first of keys that the new page has.
  async function writeFocus(keys, path, body) {
    pendingFocus = keys;
    try { return await write(path, body); } finally { pendingFocus = null; }
  }

  async function load() {
    if (!key) {
      renderFatal("This page needs the link agentcfg printed. Run agentcfg ui again.");
      return;
    }
    try {
      apply(await api("GET", "/api/state"));
    } catch (e) {
      renderFatal(e.message);
    }
  }

  function apply(next) {
    state = next;
    render();
  }

  function renderFatal(msg) {
    $("path").textContent = "";
    $("alerts").replaceChildren(h("div", { class: "alert bad" }, h("p", { text: msg })));
    $("services").replaceChildren();
    $("summary").textContent = "";
    $("add").disabled = true;
    $("add-gate").disabled = true;
  }

  function groups() {
    const by = new Map();
    for (const s of state.services) {
      if (!by.has(s.name)) by.set(s.name, { name: s.name, meta: s.meta || {}, envs: [] });
      by.get(s.name).envs.push(s);
    }
    for (const an of state.announces || []) {
      if (!by.has(an.name)) by.set(an.name, { name: an.name, meta: an.meta || {}, envs: [] });
    }
    // A gate-only project has meta and gate, and no env.
    for (const gt of state.gates || []) {
      if (!by.has(gt.name)) by.set(gt.name, { name: gt.name, meta: {}, envs: [] });
      const group = by.get(gt.name);
      group.meta = gt.meta || {};
    }
    return [...by.values()];
  }

  function render() {
    const active = document.activeElement;
    const keys = pendingFocus || (active && active.dataset && active.dataset.focus ? [active.dataset.focus] : null);
    pendingFocus = null;
    renderPage();
    if (keys) focusKeys(keys);
  }

  function renderPage() {
    $("path").textContent = state.path;
    $("appver").textContent = state.app_version ? "v" + state.app_version : "";
    $("add").disabled = !!state.error;
    $("add-gate").disabled = !!state.error;

    const alerts = [];
    if (state.error) {
      alerts.push(h("div", { class: "alert bad" },
        h("p", {}, h("strong", { text: "agentctl cannot read this file. " }), state.error, " Fix it by hand, then reload.")));
    }
    if (state.plaintext > 0) {
      const btn = h("button", { class: "btn primary", type: "button", text: "Move to Keychain" });
      btn.addEventListener("click", () => busy(btn, async () => {
        await write("/api/migrate", {});
      }));
      alerts.push(h("div", { class: "alert warn" },
        h("p", {}, h("strong", { text: plural(state.plaintext, "secret is", "secrets are") + " stored in plain text " }),
          "in services.toml (tokens or Slack webhooks). Move them into the Keychain so the file holds no secrets."),
        btn));
    }
    for (const w of state.warnings || []) {
      alerts.push(h("div", { class: "alert info" }, h("p", { text: w })));
    }
    $("alerts").replaceChildren(...alerts);

    const gs = groups();
    const wired = state.services.filter((s) => s.token.wired).length;
    $("summary").textContent = gs.length
      ? `${plural(gs.length, "service", "services")} · ${plural(state.services.length, "environment", "environments")} · ${wired} wired`
      : "";

    if (!gs.length && !state.error) {
      $("services").replaceChildren(h("div", { class: "card empty" },
        h("h2", { text: "No services yet" }),
        h("p", { class: "muted", text: "Add the first service agentctl should talk to." }),
        h("button", { class: "btn primary", type: "button", text: "Add service", onclick: openAdd })));
      return;
    }
    $("services").replaceChildren(...gs.map(renderGroup));
  }

  function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }

  function renderGroup(g) {
    const meta = [];
    if (g.meta.repo) meta.push(h("span", {}, "repo ", h("b", { class: "mono", text: g.meta.repo })));
    if (g.meta.unit) meta.push(h("span", {}, "unit ", h("b", { class: "mono", text: g.meta.unit })));
    if (!meta.length) meta.push(h("span", { text: "no repo or unit set" }));
    const an = (state.announces || []).find((a) => a.name === g.name);
    meta.push(h("span", {}, "Slack ", slackChip(an)));
    return h("section", { class: "card" },
      h("div", { class: "card-head" },
        h("div", {}, h("div", { class: "name", text: g.name }), h("div", { class: "meta" }, meta)),
        h("div", {},
          h("button", { class: "btn ghost", type: "button", text: "Add env", onclick: () => openAdd(g.name) }),
          h("button", { class: "btn ghost", type: "button", text: "Slack", onclick: () => openSlack(g.name, an) }),
          h("button", { class: "btn ghost", type: "button", text: "Gate", "aria-label": "Go to the gate of " + g.name, onclick: () => focusGate(g.name) }),
          h("button", { class: "btn ghost", type: "button", text: "Details", onclick: () => openMeta(g) }))),
      g.envs.map(renderEnv),
      renderGate(g));
  }

  // agentflow's [name.gate]: the ordered steps `agentflow gate` runs. Every
  // project card shows this block, with an empty state when there is no gate.
  const DEFAULT_TIMEOUT = "30m";
  const LOCK_HINT = "agentflow gate holds this named lock for the whole run, so two gates that share it never run at once. Empty means no lock.";
  const STOP_HINT = "Stop on fail: if this step fails, the later steps are skipped.";
  const lockEdit = new Map(); // project name -> {draft, error} while its lock is being edited

  function gateOf(name) {
    return (state.gates || []).find((x) => x.name === name) || null;
  }

  // The ⛔ marker. Screen readers hear "stop on fail", not the emoji's name.
  function stopMark(withText) {
    return h("span", { class: "stop-mark", title: STOP_HINT },
      h("span", { "aria-hidden": "true", text: "⛔" }),
      withText ? h("span", { class: "stop-text", text: "stop on fail" }) : h("span", { class: "vh", text: "stop-on-fail" }));
  }

  // The card's "Gate" action: bring the block into view and focus its heading.
  function focusGate(name) {
    const head = findFocus("gate-h:" + name);
    if (!head) return;
    head.scrollIntoView({ block: "center" });
    head.focus({ preventScroll: true });
    const block = head.closest(".gate");
    block.classList.remove("flash");
    void block.offsetWidth; // restart the highlight
    block.classList.add("flash");
  }

  function renderGate(g) {
    const name = g.name;
    const gt = gateOf(name);
    const hid = "gate-h-" + name;
    const title = h("h3", { class: "gate-title", id: hid, tabindex: "-1", "data-focus": "gate-h:" + name, text: "Gate" });
    if (!gt) {
      return h("section", { class: "gate", "aria-labelledby": hid },
        h("div", { class: "gate-head" },
          h("div", { class: "gate-title-row" }, title, h("span", { class: "muted small", text: "not set up" }))),
        h("div", { class: "gate-empty" },
          h("p", { class: "muted", text: "Commands that must pass before a PR, run in order. agentflow gate runs them and records a receipt per step." }),
          h("button", { class: "btn primary", type: "button", text: "Add first step", "data-focus": "gate-add:" + name,
            onclick: () => openGateStep(name, null) })));
    }
    const n = gt.steps.length;
    const rmGate = armedButton("Remove gate", "Confirm: remove all steps", async () => {
      await writeFocus(["gate-add:" + name, "gate-h:" + name], "/api/gate/remove", { name });
      toast("Removed the gate of " + name);
    });
    const [lockInline, lockExtra] = renderLock(name, gt);
    const head = h("div", { class: "gate-head" },
      h("div", { class: "gate-title-row" }, title, h("span", { class: "muted small", text: plural(n, "step", "steps") })),
      lockInline,
      h("div", { class: "gate-head-actions" }, rmGate),
      lockExtra);
    const legend = h("p", { class: "gate-legend muted small" },
      "Runs top to bottom; every step runs even if an earlier one fails. A ", stopMark(false), " step skips the rest when it fails.");
    const list = n
      ? h("ol", { class: "gate-steps", role: "list", "aria-label": "Gate steps of " + name + ", in run order" },
        gt.steps.map((st, i) => renderGateStep(name, gt.steps, i)))
      : h("p", { class: "gate-none muted small", text: "No steps yet." });
    const foot = h("p", { class: "gate-foot muted small" },
      "Run: ", h("code", { class: "mono", text: "agentflow gate" }),
      " · Proof: ", h("code", { class: "mono", text: "agentflow gate --check --rev HEAD" }),
      g.meta.repo ? null : h("span", { class: "gate-norepo", text: " · Set the repo in Details so agentflow gate can find this project." }));
    return h("section", { class: "gate", "aria-labelledby": hid },
      head, legend,
      gt.error ? h("p", { class: "gate-error", text: "agentflow refuses this gate: " + gt.error }) : null,
      list,
      h("div", { class: "gate-add" },
        h("button", { class: "btn", type: "button", text: "+ Add step", "data-focus": "gate-add:" + name,
          onclick: () => openGateStep(name, null) })),
      foot);
  }

  function renderGateStep(name, steps, i) {
    const st = steps[i];
    const n = steps.length;
    const key = name + ":" + st.name;
    const up = h("button", { class: "btn icon", type: "button", text: "↑", title: "Move up", "aria-label": "Move " + st.name + " up",
      "data-focus": "gate-up:" + key, disabled: i === 0 || undefined });
    up.addEventListener("click", () => busy(up, () =>
      writeFocus(["gate-up:" + key, "gate-down:" + key], "/api/gate/move", { name, step: st.name, to: i })));
    const down = h("button", { class: "btn icon", type: "button", text: "↓", title: "Move down", "aria-label": "Move " + st.name + " down",
      "data-focus": "gate-down:" + key, disabled: i === n - 1 || undefined });
    down.addEventListener("click", () => busy(down, () =>
      writeFocus(["gate-down:" + key, "gate-up:" + key], "/api/gate/move", { name, step: st.name, to: i + 2 })));
    const edit = h("button", { class: "btn", type: "button", text: "Edit", "aria-label": "Edit step " + st.name,
      "data-focus": "gate-edit:" + key, onclick: () => openGateStep(name, st) });
    // After a removal, focus moves to the next step (or the previous one).
    const neighbour = steps[i + 1] || steps[i - 1];
    const rm = armedButton("Remove", "Confirm", async () => {
      await writeFocus([neighbour ? "gate-edit:" + name + ":" + neighbour.name : "", "gate-add:" + name],
        "/api/gate/step/remove", { name, step: st.name });
      toast("Removed step " + st.name);
    }, ["Remove step " + st.name, "Confirm: remove step " + st.name]);
    const timeout = st.timeout
      ? h("span", { class: "gate-timeout mono", title: "Timeout" }, h("span", { class: "vh", text: "timeout " }), st.timeout)
      : h("span", { class: "gate-timeout mono muted", title: "Timeout (the default)" }, h("span", { class: "vh", text: "default timeout " }), DEFAULT_TIMEOUT);
    return h("li", { class: "gate-row" + (st.stop_on_fail ? " stops" : "") },
      h("span", { class: "gate-idx", text: String(i + 1) }),
      h("span", { class: "gate-name mono", text: st.name, title: st.name }),
      h("span", { class: "gate-run mono", text: st.run, title: st.run }),
      timeout,
      h("span", { class: "gate-stop" }, st.stop_on_fail ? stopMark(true) : null),
      h("div", { class: "actions" }, up, down, edit, rm));
  }

  // The lock, edited in place in the block header. Returns the inline part
  // and, while editing, a full-width row with the hint and any error.
  function renderLock(name, gt) {
    const e = lockEdit.get(name);
    if (!e) {
      return [h("div", { class: "gate-lock" },
        h("span", { class: "muted small", text: "Lock:" }),
        gt.lock ? h("span", { class: "chip ok mono", text: gt.lock, title: LOCK_HINT }) : h("span", { class: "muted small", text: "none", title: LOCK_HINT }),
        h("button", { class: "btn ghost", type: "button", text: "Edit", "aria-label": "Edit gate lock", "data-focus": "gate-lock:" + name,
          onclick: () => {
            lockEdit.set(name, { draft: gt.lock || "", error: "" });
            pendingFocus = ["gate-lock-in:" + name];
            render();
          } })), null];
    }
    const id = "gate-lock-in-" + name;
    const input = h("input", { id, type: "text", class: "mono", value: e.draft, placeholder: name + "-build", autocomplete: "off",
      spellcheck: "false", "data-focus": "gate-lock-in:" + name, "aria-describedby": id + "-hint", "aria-invalid": e.error ? "true" : undefined });
    input.addEventListener("input", () => { e.draft = input.value; });
    const save = h("button", { class: "btn primary", type: "button", text: "Save" });
    const cancel = h("button", { class: "btn", type: "button", text: "Cancel" });
    const close = () => {
      lockEdit.delete(name);
      pendingFocus = ["gate-lock:" + name];
      render();
    };
    const doSave = () => busy(save, async () => {
      try {
        await write("/api/gate/lock", { name, lock: input.value.trim() });
      } catch (err) {
        if (lockEdit.has(name)) {
          e.error = err.message;
          pendingFocus = ["gate-lock-in:" + name];
          render();
        }
        return;
      }
      close();
      toast("Saved the gate lock of " + name);
    });
    save.addEventListener("click", doSave);
    cancel.addEventListener("click", close);
    input.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter") { ev.preventDefault(); doSave(); }
      if (ev.key === "Escape") { ev.preventDefault(); close(); }
    });
    return [
      h("div", { class: "gate-lock editing" },
        h("label", { for: id, class: "muted small", text: "Lock:" }), input, save, cancel),
      h("div", { class: "gate-lock-extra" },
        h("p", { class: "hint", id: id + "-hint", text: LOCK_HINT }),
        e.error ? h("p", { class: "form-error", role: "alert", text: e.error }) : null),
    ];
  }

  // A destructive button that needs a second click within 4 s. aria, when
  // given, is [label, armed label] for buttons whose text alone is ambiguous.
  function armedButton(text, armedText, fn, aria) {
    const b = h("button", { class: "btn danger", type: "button", text, "aria-label": aria ? aria[0] : undefined });
    let armed = null;
    const reset = () => {
      armed = null;
      b.classList.remove("armed");
      b.textContent = text;
      if (aria) b.setAttribute("aria-label", aria[0]);
    };
    b.addEventListener("click", () => {
      if (!armed) {
        b.classList.add("armed");
        b.textContent = armedText;
        if (aria) b.setAttribute("aria-label", aria[1]);
        armed = setTimeout(reset, 4000);
        return;
      }
      clearTimeout(armed);
      armed = null;
      // A refused write leaves the button in place; disarm it.
      busy(b, fn).then(() => { if (b.isConnected) reset(); });
    });
    return b;
  }
  // agentflow's [name.announce]: where `agentflow ship announce` posts.
  function slackChip(an) {
    if (!an) return h("span", { class: "muted", text: "off" });
    const label = (an.channel || "?") + " · " + an.envs.join(", ");
    if (an.webhook.wired && an.webhook.source === "keychain") {
      return h("span", { class: "chip ok", title: "Webhook in the Keychain (" + an.webhook.fingerprint + ")" }, label);
    }
    if (an.webhook.wired) {
      return h("span", { class: "chip warn", title: "Webhook stored in plain text in services.toml" }, label + " · in file");
    }
    return h("span", { class: "chip bad", title: an.webhook.reason || "" }, label + " · " + (an.webhook.reason || "not ready"));
  }

  function tokenChip(t) {
    if (t.source === "keychain" && t.wired) {
      return h("span", { class: "chip ok", title: "Stored in the Keychain" }, "Keychain ", h("span", { class: "fp", text: t.fingerprint }));
    }
    if (t.source === "file" && t.wired) {
      return h("span", { class: "chip warn", title: "Stored in plain text in services.toml" }, "In file ", h("span", { class: "fp", text: t.fingerprint }));
    }
    return h("span", { class: "chip bad", title: t.reason || "" }, t.source === "keychain" ? "Keychain: missing" : "No token");
  }

  function renderEnv(s) {
    const full = s.name + "." + s.env;
    const testBtn = h("button", { class: "btn", type: "button", text: "Test", disabled: !s.token.wired || undefined });
    testBtn.addEventListener("click", () => busy(testBtn, async () => {
      const r = await api("POST", "/api/test", { service: full });
      tests[full] = r.ok
        ? { ok: true, text: `OK · version ${r.version || "?"} · ${r.ms} ms` }
        : { ok: false, text: r.error || "failed" };
      render();
    }));
    const rm = h("button", { class: "btn danger", type: "button", text: "Remove" });
    let armed = null;
    rm.addEventListener("click", () => {
      if (!armed) {
        rm.classList.add("armed");
        rm.textContent = "Confirm";
        armed = setTimeout(() => { armed = null; rm.classList.remove("armed"); rm.textContent = "Remove"; }, 4000);
        return;
      }
      clearTimeout(armed);
      busy(rm, async () => {
        delete tests[full];
        await write("/api/remove", { service: full });
        toast("Removed " + full);
      });
    });
    const tr = tests[full];
    return h("div", { class: "env-row" },
      h("span", { class: "pill", text: s.env }),
      h("div", { class: "url" },
        h("span", { class: "mono", text: s.base_url, title: s.base_url }),
        !s.token.wired && s.token.reason ? h("span", { class: "reason", text: s.token.reason, title: s.token.reason }) : null),
      h("div", { class: "token" }, tokenChip(s.token)),
      h("div", { class: "actions" },
        testBtn,
        h("button", { class: "btn", type: "button", text: s.token.source === "none" || !s.token.wired ? "Set token" : "Replace token", onclick: () => openToken(s) }),
        h("button", { class: "btn", type: "button", text: "Edit", onclick: () => openURL(s) }),
        rm),
      tr ? h("div", { class: "result" }, h("span", { class: "result-line " + (tr.ok ? "ok" : "bad"), text: tr.text })) : null);
  }

  async function busy(btn, fn) {
    const had = document.activeElement === btn;
    btn.disabled = true;
    try { await fn(); } catch (e) { toast(e.message, true); } finally {
      btn.disabled = false;
      // Disabling the focused button drops focus to the page; when no render
      // moved it elsewhere, give it back.
      if (had && btn.isConnected && (!document.activeElement || document.activeElement === document.body)) btn.focus();
    }
  }

  // ---- dialogs ----

  function field(id, label, opts) {
    opts = opts || {};
    const input = h("input", {
      id, name: id, type: opts.type || "text", value: opts.value || "", placeholder: opts.placeholder || "",
      class: opts.mono ? "mono" : "", autocomplete: "off", spellcheck: "false",
      required: opts.required || undefined, readonly: opts.readonly || undefined,
    });
    return h("div", { class: "field" },
      h("label", { for: id, text: label }), input,
      opts.hint ? h("div", { class: "hint", text: opts.hint }) : null);
  }

  let onSubmit = null;
  // Where focus goes when the dialog closes: by default the control that
  // opened it, found again by its focus key if a render replaced it. A
  // submit handler may point it elsewhere (a renamed step's Edit button).
  let dialogReturn = null;
  function openDialog(title, hint, fields, okText, submit) {
    const opener = document.activeElement;
    dialogReturn = opener && opener.dataset && opener.dataset.focus ? [opener.dataset.focus] : null;
    $("dlg-title").textContent = title;
    $("dlg-hint").textContent = hint || "";
    $("dlg-fields").replaceChildren(...fields);
    $("dlg-error").textContent = "";
    $("dlg-ok").textContent = okText;
    $("dlg-ok").disabled = false;
    onSubmit = submit;
    $("dlg").showModal();
    const first = $("dlg-fields").querySelector("input:not([readonly]), select");
    if (first) first.focus();
  }
  const val = (id) => ($(id) ? $(id).value.trim() : "");

  function openAdd(name) {
    const fixed = typeof name === "string" ? name : "";
    openDialog(fixed ? "Add an env to " + fixed : "Add service",
      "The token goes straight into the Keychain; it is never written to services.toml.",
      [
        h("div", { class: "field-row" },
          field("f-name", "Service", { value: fixed, readonly: !!fixed, placeholder: "recursivecx", mono: true, required: true }),
          field("f-env", "Environment", { placeholder: "prod", mono: true, required: true })),
        field("f-url", "Base URL", { placeholder: "https://example.com", mono: true, required: true }),
        field("f-token", "Token", { type: "password", mono: true, hint: "Optional now; you can set it later." }),
        fixed ? null : h("div", { class: "field-row" },
          field("f-repo", "Repo (optional)", { placeholder: "~/src/github.com/org/repo", mono: true }),
          field("f-unit", "Systemd unit (optional)", { placeholder: "service.service", mono: true })),
      ].filter(Boolean),
      "Add",
      async () => {
        // Snapshot the form before the first request. The add operation needs
        // several writes, and rendering/canceling/editing must not change
        // which values later writes use.
        const form = {
          name: val("f-name"), env: val("f-env"), baseURL: val("f-url"),
          token: $("f-token").value,
          repo: val("f-repo"), unit: val("f-unit"),
        };
        const full = form.name + "." + form.env;
        if (state.services.some((s) => s.name + "." + s.env === full)) {
          throw new Error(full + " already exists. Use Edit to change it.");
        }
        await write("/api/env", { service: full, base_url: form.baseURL });
        try {
          // Do not trim secrets. cfg.CheckToken deliberately rejects any
          // whitespace as a paste error rather than guessing a different key.
          if (form.token) {
            await write("/api/token", { service: full, token: form.token });
          }
          if (!fixed && (form.repo || form.unit)) {
            await write("/api/meta", { name: form.name, meta: { repo: form.repo, unit: form.unit } });
          }
        } catch (e) {
          // The env write already committed. Resolve this submit so its common
          // path closes the dialog; retrying Add would only collide with that
          // env. Its ordinary token/details controls can finish the setup.
          toast("Added " + full + ", but setup is incomplete: " + e.message, true);
          return;
        }
        toast("Added " + full);
      });
  }

  function openURL(s) {
    const full = s.name + "." + s.env;
    openDialog("Edit " + full, "", [field("f-url", "Base URL", { value: s.base_url, mono: true, required: true })], "Save",
      async () => {
        await write("/api/env", { service: full, base_url: val("f-url") });
        delete tests[full];
        toast("Saved " + full);
      });
  }

  function openToken(s) {
    const full = s.name + "." + s.env;
    openDialog((s.token.wired ? "Replace token for " : "Set token for ") + full,
      "Stored in your login Keychain as agentctl / " + full + ". " +
        (s.token.source === "file" ? "The plaintext copy in services.toml is removed." : ""),
      [field("f-token", "Token", { type: "password", mono: true, required: true })], "Store in Keychain",
      async () => {
        await write("/api/token", { service: full, token: $("f-token").value });
        delete tests[full];
        toast("Stored the " + full + " token in the Keychain");
      });
  }

  function openSlack(name, an) {
    const hasHook = !!an && an.webhook.source !== "none";
    const fields = [
      field("f-channel", "Channel label", { value: an ? an.channel : "", placeholder: "#releases", required: true,
        hint: "Shown in agentflow's output; the webhook decides where Slack posts." }),
      field("f-envs", "Envs that announce", { value: an ? an.envs.join(", ") : "", placeholder: "prod", mono: true,
        hint: "Comma-separated. Empty means prod." }),
      field("f-hook", "Incoming webhook", { type: "password", mono: true, required: !hasHook,
        placeholder: hasHook ? "leave empty to keep the stored webhook" : "https://hooks.slack.com/services/…",
        hint: "Stored in your login Keychain (service agentflow), never in services.toml." }),
    ];
    if (an) {
      const rm = h("button", { class: "btn danger", type: "button", text: "Turn off Slack announce" });
      let armed = null;
      rm.addEventListener("click", () => {
        if (!armed) {
          rm.classList.add("armed");
          rm.textContent = "Confirm: remove settings and webhook";
          armed = setTimeout(() => { armed = null; rm.classList.remove("armed"); rm.textContent = "Turn off Slack announce"; }, 4000);
          return;
        }
        clearTimeout(armed);
        busy(rm, async () => {
          await write("/api/announce/remove", { name });
          $("dlg").close();
          toast("Slack announce is off for " + name);
        });
      });
      fields.push(h("div", { class: "field" }, rm));
    }
    openDialog("Slack announce for " + name,
      "agentflow ship announce posts a verified deploy here.", fields, "Save",
      async () => {
        const form = { channel: val("f-channel"), envs: val("f-envs"), hook: $("f-hook").value };
        const envs = form.envs ? form.envs.split(",").map((e) => e.trim()).filter(Boolean) : ["prod"];
        await write("/api/announce", { name, channel: form.channel, envs, webhook: form.hook });
        toast("Saved Slack announce for " + name);
      });
  }

  function checkbox(id, label, checked, hint) {
    return h("div", { class: "field check" },
      h("label", { for: id },
        h("input", { id, name: id, type: "checkbox", checked: checked || undefined }), " ", label),
      hint ? h("div", { class: "hint", text: hint }) : null);
  }

  // Add (st null) or edit a gate step. The dialog shows the gate's steps with
  // this one highlighted where it will land; Position picks that place. With
  // no name, it starts a new gate-only project and can set its repo.
  function openGateStep(name, st) {
    const fresh = !name;
    // The dialog is built from this snapshot. Every write it makes carries the
    // snapshot's version (or the version of the write before it), so a file
    // that changed meanwhile is refused (409) instead of placing the step
    // against a list the person did not see.
    const base = state.version;
    const steps = fresh ? [] : ((gateOf(name) || { steps: [] }).steps).slice();
    const others = st ? steps.filter((x) => x.name !== st.name) : steps;
    const fields = [];
    if (fresh) {
      fields.push(
        field("f-proj", "Project", { placeholder: "myproj", mono: true, required: true,
          hint: "A new project name. To gate an existing service, use the Gate block on its card." }),
        field("f-repo", "Repo (optional)", { placeholder: "~/src/github.com/org/repo", mono: true,
          hint: "The project's checkout. agentflow gate picks the project whose repo is the checkout (or a worktree of it) it runs in; without a repo it needs --project." }));
    }
    fields.push(
      field("f-step", "Step name", { value: st ? st.name : "", placeholder: "test", mono: true, required: true }),
      field("f-run", "Command", { value: st ? st.run : "", placeholder: "make test", mono: true, required: true,
        hint: "Runs with /bin/sh -c from the repo root, stdin closed." }),
      field("f-timeout", "Timeout", { value: st ? st.timeout : "", placeholder: DEFAULT_TIMEOUT, mono: true,
        hint: "Empty means " + DEFAULT_TIMEOUT + " (max 24h). When it runs out, the step's whole process group is killed." }),
      checkbox("f-stop", "Stop on fail", st && st.stop_on_fail,
        "On: if this step fails, the later steps are skipped. Off: the later steps still run, so one run reports every failure. The gate fails either way."));

    // Position: 0 = first, k = after others[k-1]. A new step defaults to the
    // end; an edited one to where it is now.
    let sel = null;
    let preview = null;
    if (others.length) {
      const initial = st ? steps.findIndex((x) => x.name === st.name) : others.length;
      sel = h("select", { id: "f-pos", name: "f-pos", "aria-describedby": "f-preview-label" },
        h("option", { value: "0", text: "First" }),
        others.map((x, k) => h("option", { value: String(k + 1), text: "After " + x.name + (k === others.length - 1 ? " (last)" : "") })));
      sel.value = String(initial);
      preview = h("ol", { class: "gate-preview", role: "list", "aria-labelledby": "f-preview-label" });
      fields.push(h("div", { class: "field" },
        h("label", { for: "f-pos", text: "Position" }), sel,
        h("div", { class: "hint", id: "f-preview-label", text: "Order after saving:" }),
        preview));
    }
    const draw = () => {
      if (!preview) return;
      const k = Number(sel.value);
      const label = val("f-step") || (st ? st.name : "new step");
      const rows = others.map((x) => h("li", {},
        h("span", { class: "mono", text: x.name }), h("span", { class: "mono muted pv-run", text: x.run })));
      rows.splice(k, 0, h("li", { class: "this" },
        h("span", { class: "mono", text: label }),
        h("span", { class: "pv-tag", text: st ? "this step" : "new" })));
      preview.replaceChildren(...rows);
    };

    openDialog(st ? "Edit gate step " + st.name : (fresh ? "Add a gate project" : "Add a gate step to " + name),
      "agentflow gate runs the steps top to bottom and records a receipt for each step that passes.", fields, st ? "Save" : "Add",
      async () => {
        const form = {
          proj: fresh ? val("f-proj") : name, repo: fresh ? val("f-repo") : "",
          step: { name: val("f-step"), run: val("f-run"), timeout: val("f-timeout"), stop_on_fail: $("f-stop").checked },
          pos: sel ? Number(sel.value) : others.length,
        };
        const anchor = form.pos > 0 ? others[form.pos - 1].name : null; // the step it goes after
        if (fresh) {
          if (groups().some((g) => g.name === form.proj)) {
            throw new Error(form.proj + " already exists. Use the Gate block on its card.");
          }
          // The repo goes first: a gate without meta.repo cannot run, while a
          // refused step leaves only the repo, and resubmitting adds the step.
          if (form.repo) {
            await write("/api/meta", { name: form.proj, meta: { repo: form.repo } }, base);
          }
          await write("/api/gate/step", { name: form.proj, original: "", step: form.step }, form.repo ? undefined : base);
          toast("Added step " + form.step.name + " to " + form.proj);
          return;
        }
        if (!st) {
          dialogReturn = ["gate-add:" + name];
          const at = anchor === null ? 1 : steps.findIndex((x) => x.name === anchor) + 2;
          await write("/api/gate/step", { name, original: "", step: form.step, at }, base);
          toast("Added step " + form.step.name);
          return;
        }
        // Focus returns to this step's Edit button, under its new name.
        dialogReturn = ["gate-edit:" + name + ":" + form.step.name, "gate-add:" + name];
        const changed = form.step.name !== st.name || form.step.run !== st.run ||
          form.step.timeout !== (st.timeout || "") || form.step.stop_on_fail !== !!st.stop_on_fail;
        if (changed) {
          await write("/api/gate/step", { name, original: st.name, step: form.step }, base);
        }
        // Then the position, against the list as it is now.
        const cur = (gateOf(name) || { steps: [] }).steps;
        const rest = cur.filter((x) => x.name !== form.step.name);
        const now = cur.findIndex((x) => x.name === form.step.name) + 1;
        const to = anchor === null ? 1 : rest.findIndex((x) => x.name === anchor) + 2;
        if (to !== now) {
          try {
            // Without an edit, the snapshot's version guards the move too.
            await write("/api/gate/move", { name, step: form.step.name, to }, changed ? undefined : base);
          } catch (e) {
            if (!changed) throw e;
            // The edit already committed; close and say what is left to do.
            toast("Saved step " + form.step.name + ", but did not move it: " + e.message, true);
            return;
          }
        }
        toast(changed || to !== now ? "Saved step " + form.step.name : "No changes to save");
      });
    if (preview) {
      draw();
      sel.addEventListener("change", draw);
      $("f-step").addEventListener("input", draw);
    }
  }
  function openMeta(g) {
    openDialog("Details for " + g.name, "Used by agentflow and the fleet view. Leave a field empty to clear it.",
      [
        field("f-repo", "Repo", { value: g.meta.repo, placeholder: "~/src/github.com/org/repo", mono: true }),
        field("f-unit", "Systemd unit", { value: g.meta.unit, placeholder: "service.service", mono: true }),
      ], "Save",
      async () => {
        await write("/api/meta", { name: g.name, meta: { repo: val("f-repo"), unit: val("f-unit") } });
        toast("Saved " + g.name);
      });
  }

  $("dlg-form").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    for (const input of $("dlg-fields").querySelectorAll("input[required]")) {
      if (!input.value.trim()) {
        $("dlg-error").textContent = "Fill in " + input.labels[0].textContent + ".";
        input.focus();
        return;
      }
    }
    const controls = [...$("dlg-form").elements];
    controls.forEach((control) => { control.disabled = true; });
    try {
      await onSubmit();
      $("dlg").close();
    } catch (e) {
      $("dlg-error").textContent = e.message;
    } finally {
      controls.forEach((control) => { control.disabled = false; });
    }
  });
  $("dlg-cancel").addEventListener("click", () => $("dlg").close());
  $("dlg").addEventListener("cancel", (ev) => {
    if ($("dlg-ok").disabled) ev.preventDefault();
  });
  $("dlg").addEventListener("close", () => {
    $("dlg-fields").replaceChildren();
    onSubmit = null;
    const keys = dialogReturn;
    dialogReturn = null;
    if (keys) focusKeys(keys);
  });

  $("add").addEventListener("click", () => openAdd());
  $("add-gate").addEventListener("click", () => openGateStep("", null));
  $("done").addEventListener("click", async () => {
    try {
      await api("POST", "/api/quit", {});
    } catch (e) {
      // A network failure means the idle timer probably stopped the server.
      // An HTTP rejection means it is still running and must not be reported
      // as stopped.
      if (e.status) {
        toast(e.message, true);
        return;
      }
    }
    try { sessionStorage.removeItem(KEY_STORE); } catch (_) { /* ignore */ }
    $("stopped").hidden = false;
  });

  load();
})();
