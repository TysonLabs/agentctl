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

  // Writes carry the version they were based on; a 409 means the file changed
  // underneath us, so reload and let the person redo the edit.
  async function write(path, body) {
    try {
      const next = await api("POST", path, Object.assign({ version: state ? state.version : "" }, body));
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
  }

  function groups() {
    const by = new Map();
    for (const s of state.services) {
      if (!by.has(s.name)) by.set(s.name, { name: s.name, meta: s.meta || {}, envs: [] });
      by.get(s.name).envs.push(s);
    }
    return [...by.values()];
  }

  function render() {
    $("path").textContent = state.path;
    $("appver").textContent = state.app_version ? "v" + state.app_version : "";
    $("add").disabled = !!state.error;

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
        h("p", {}, h("strong", { text: plural(state.plaintext, "token is", "tokens are") + " stored in plain text " }),
          "in services.toml. Move them into the Keychain so the file holds no secrets."),
        btn));
    }
    for (const w of state.warnings || []) {
      alerts.push(h("div", { class: "alert info" }, h("p", { text: w })));
    }
    $("alerts").replaceChildren(...alerts);

    const gs = groups();
    const wired = state.services.filter((s) => s.token.wired).length;
    $("summary").textContent = state.services.length
      ? `${plural(gs.length, "service", "services")} · ${plural(state.services.length, "environment", "environments")} · ${wired} wired`
      : "";

    if (!state.services.length && !state.error) {
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
    return h("section", { class: "card" },
      h("div", { class: "card-head" },
        h("div", {}, h("div", { class: "name", text: g.name }), h("div", { class: "meta" }, meta)),
        h("div", {},
          h("button", { class: "btn ghost", type: "button", text: "Add env", onclick: () => openAdd(g.name) }),
          h("button", { class: "btn ghost", type: "button", text: "Details", onclick: () => openMeta(g) }))),
      g.envs.map(renderEnv));
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
    btn.disabled = true;
    try { await fn(); } catch (e) { toast(e.message, true); } finally { btn.disabled = false; }
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
  function openDialog(title, hint, fields, okText, submit) {
    $("dlg-title").textContent = title;
    $("dlg-hint").textContent = hint || "";
    $("dlg-fields").replaceChildren(...fields);
    $("dlg-error").textContent = "";
    $("dlg-ok").textContent = okText;
    $("dlg-ok").disabled = false;
    onSubmit = submit;
    $("dlg").showModal();
    const first = $("dlg-fields").querySelector("input:not([readonly])");
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
  $("dlg").addEventListener("close", () => { $("dlg-fields").replaceChildren(); onSubmit = null; });

  $("add").addEventListener("click", () => openAdd());
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
