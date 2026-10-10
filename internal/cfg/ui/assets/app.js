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
    $("add-gate").disabled = true;
  }

  function groups() {
    const by = new Map();
    for (const s of state.services) {
      if (!by.has(s.name)) by.set(s.name, { name: s.name, meta: s.meta || {}, envs: [] });
      by.get(s.name).envs.push(s);
    }
    for (const an of state.announces || []) {
      if (!by.has(an.name)) by.set(an.name, { name: an.name, meta: {}, envs: [] });
    }
    // A gate-only project has meta and gate, and no env.
    for (const gt of state.gates || []) {
      if (!by.has(gt.name)) by.set(gt.name, { name: gt.name, meta: gt.meta || {}, envs: [] });
    }
    return [...by.values()];
  }

  function render() {
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
          h("button", { class: "btn ghost", type: "button", text: "Gate step", onclick: () => openGateStep(g.name, null) }),
          h("button", { class: "btn ghost", type: "button", text: "Details", onclick: () => openMeta(g) }))),
      g.envs.map(renderEnv),
      renderGate(g.name));
  }

  // agentflow's [name.gate]: the ordered steps `agentflow gate` runs.
  function renderGate(name) {
    const gt = (state.gates || []).find((x) => x.name === name);
    if (!gt) return null;
    const n = gt.steps.length;
    const lockBtn = h("button", { class: "btn", type: "button", text: gt.lock ? "Change lock" : "Set lock", onclick: () => openGateLock(name, gt) });
    const rmGate = armedButton("Remove gate", "Confirm: remove all steps", async () => {
      await write("/api/gate/remove", { name });
      toast("Removed the gate of " + name);
    });
    const head = h("div", { class: "gate-head" },
      h("span", { class: "pill", text: "gate" }),
      h("div", { class: "gate-sum" },
        h("span", { class: "muted small", text: plural(n, "step", "steps") + " · lock " }),
        gt.lock ? h("span", { class: "chip ok mono", text: gt.lock }) : h("span", { class: "muted small", text: "none" }),
        gt.error ? h("span", { class: "reason", text: gt.error, title: gt.error }) : null),
      h("div", { class: "actions" }, lockBtn, rmGate));
    const rows = gt.steps.map((st, i) => {
      const up = h("button", { class: "btn", type: "button", text: "↑", title: "Move up", "aria-label": "Move " + st.name + " up", disabled: i === 0 || undefined });
      up.addEventListener("click", () => busy(up, () => write("/api/gate/move", { name, step: st.name, to: i })));
      const down = h("button", { class: "btn", type: "button", text: "↓", title: "Move down", "aria-label": "Move " + st.name + " down", disabled: i === n - 1 || undefined });
      down.addEventListener("click", () => busy(down, () => write("/api/gate/move", { name, step: st.name, to: i + 2 })));
      const rm = armedButton("Delete", "Confirm", async () => {
        await write("/api/gate/step/remove", { name, step: st.name });
        toast("Deleted step " + st.name);
      });
      return h("div", { class: "gate-row" },
        h("span", { class: "gate-idx muted", text: String(i + 1) }),
        h("div", { class: "gate-step" },
          h("span", { class: "gate-name", text: st.name }),
          h("span", { class: "mono gate-run", text: st.run, title: st.run })),
        h("div", { class: "gate-flags" },
          h("span", { class: "muted small", text: st.timeout || "30m" }),
          st.stop_on_fail ? h("span", { class: "chip warn", title: "Later steps are skipped if this one fails", text: "stops" }) : null),
        h("div", { class: "actions" }, up, down,
          h("button", { class: "btn", type: "button", text: "Edit", onclick: () => openGateStep(name, st) }),
          rm));
    });
    return h("div", { class: "gate" }, head, rows);
  }

  // A destructive button that needs a second click within 4 s.
  function armedButton(text, armedText, fn) {
    const b = h("button", { class: "btn danger", type: "button", text });
    let armed = null;
    b.addEventListener("click", () => {
      if (!armed) {
        b.classList.add("armed");
        b.textContent = armedText;
        armed = setTimeout(() => { armed = null; b.classList.remove("armed"); b.textContent = text; }, 4000);
        return;
      }
      clearTimeout(armed);
      armed = null;
      busy(b, fn);
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

  // Add (st null) or edit a gate step. With no name, it starts a new
  // gate-only project and can set its repo.
  function openGateStep(name, st) {
    const fresh = !name;
    const fields = [];
    if (fresh) {
      fields.push(h("div", { class: "field-row" },
        field("f-proj", "Project", { placeholder: "myproj", mono: true, required: true }),
        field("f-repo", "Repo (optional)", { placeholder: "~/src/github.com/org/repo", mono: true,
          hint: "agentflow gate finds the project from this checkout." })));
    }
    fields.push(
      field("f-step", "Step name", { value: st ? st.name : "", placeholder: "test", mono: true, required: true }),
      field("f-run", "Command", { value: st ? st.run : "", placeholder: "make test", mono: true, required: true,
        hint: "Runs with /bin/sh -c from the repo root, stdin closed." }),
      field("f-timeout", "Timeout", { value: st ? st.timeout : "", placeholder: "30m", mono: true,
        hint: "Empty means 30m. The whole process group is killed when it runs out." }),
      checkbox("f-stop", "Stop on fail", st && st.stop_on_fail, "Skip the later steps when this one fails."));
    openDialog(st ? "Edit gate step " + st.name : (fresh ? "Add a gate project" : "Add a gate step to " + name),
      "agentflow gate runs these steps in order and records a receipt for each pass.", fields, st ? "Save" : "Add",
      async () => {
        const form = {
          proj: fresh ? val("f-proj") : name, repo: fresh ? val("f-repo") : "",
          step: { name: val("f-step"), run: val("f-run"), timeout: val("f-timeout"), stop_on_fail: $("f-stop").checked },
        };
        if (fresh && groups().some((g) => g.name === form.proj)) {
          throw new Error(form.proj + " already exists. Use its Gate step button.");
        }
        await write("/api/gate/step", { name: form.proj, original: st ? st.name : "", step: form.step });
        if (form.repo) {
          try {
            await write("/api/meta", { name: form.proj, meta: { repo: form.repo } });
          } catch (e) {
            toast("Added the step, but the repo was not saved: " + e.message, true);
            return;
          }
        }
        toast((st ? "Saved step " : "Added step ") + form.step.name);
      });
  }

  function openGateLock(name, gt) {
    openDialog("Gate lock for " + name,
      "agentflow gate holds this named lock for the whole run, so two gates that share it never run at once. Leave it empty for no lock.",
      [field("f-lock", "Lock name", { value: gt.lock, placeholder: name + "-build", mono: true })], "Save",
      async () => {
        await write("/api/gate/lock", { name, lock: val("f-lock") });
        toast("Saved the gate lock of " + name);
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
