// Gopherdex landing page: search, release history and docs.
// Page state lives in the URL: /?q=<query> shows results, /?m=<module>&v=<version> shows a library.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  // h builds DOM nodes without innerHTML, so module data is always text.
  function h(tag, props, ...children) {
    const el = document.createElement(tag);
    for (const [key, value] of Object.entries(props || {})) {
      if (value == null || value === false) continue;
      if (key === "class") el.className = value;
      else if (key.startsWith("on")) el.addEventListener(key.slice(2), value);
      else el.setAttribute(key, value === true ? "" : value);
    }
    for (const child of children.flat()) {
      if (child == null || child === false) continue;
      el.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return el;
  }

  // ---------- Copy + toast ----------
  const toast = $("toast");
  let toastTimer;
  function showToast(message) {
    toast.textContent = message;
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toast.hidden = true; }, 2000);
  }
  async function copy(text) {
    try {
      await navigator.clipboard.writeText(text);
      showToast("Copied: " + text);
    } catch (e) {
      showToast("Your browser blocked the clipboard. Select the command and copy it by hand.");
    }
  }
  document.addEventListener("click", (event) => {
    const button = event.target.closest("[data-copy]");
    if (button) copy(button.getAttribute("data-copy"));
  });

  // ---------- Sign-up namespace preview ----------
  const nsInput = document.querySelector("[data-namespace-input]");
  const nsPreview = $("ns-preview");
  if (nsInput && nsPreview) {
    nsInput.addEventListener("input", () => {
      nsPreview.textContent = nsInput.value.trim().toLowerCase() || "your-name";
    });
  }

  // ---------- Formatting ----------
  const dateFormat = new Intl.DateTimeFormat(undefined, { dateStyle: "medium" });
  const relativeFormat = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  function hasTime(iso) { return iso && !iso.startsWith("0001-"); }
  function relative(iso) {
    const seconds = (new Date(iso).getTime() - Date.now()) / 1000;
    const units = [["year", 31536000], ["month", 2592000], ["week", 604800], ["day", 86400], ["hour", 3600], ["minute", 60]];
    for (const [unit, size] of units) {
      if (Math.abs(seconds) >= size) return relativeFormat.format(Math.round(seconds / size), unit);
    }
    return "just now";
  }
  function when(iso) {
    if (!hasTime(iso)) return "Release date unknown";
    return `${dateFormat.format(new Date(iso))} · ${relative(iso)}`;
  }
  const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;

  // ---------- API ----------
  // One in-flight request per channel; a newer request cancels the older one.
  const inflight = new Map();
  async function getJSON(url, channel = "main") {
    inflight.get(channel)?.abort();
    const controller = new AbortController();
    inflight.set(channel, controller);
    const response = await fetch(url, { signal: controller.signal, headers: { Accept: "application/json" } });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.error || `The server answered ${response.status}.`);
    return body;
  }

  const synopses = new Map(); // module path → synopsis from search results
  const hostedPaths = new Set(); // modules served by this registry

  // ---------- Search page ----------
  // Filters and sorting are plain GET forms; apply them as soon as they change.
  for (const form of [$("filters"), document.querySelector(".sort-form")]) {
    form?.addEventListener("change", () => form.submit());
  }
  document.querySelector(".filter-actions button")?.setAttribute("hidden", "");

  // Public modules from pkg.go.dev load after the registry's own results.
  const publicBox = $("public-results");
  if (publicBox) loadPublicResults(publicBox);

  async function loadPublicResults(box) {
    const list = $("public-list");
    box.hidden = false;
    list.replaceChildren(h("li", { class: "muted loading-row" }, "Searching pkg.go.dev…"));
    try {
      const data = await getJSON(`/api/search?q=${encodeURIComponent(box.dataset.query)}&scope=public`, "public");
      list.replaceChildren(...data.results.map(resultItem));
      box.hidden = data.results.length === 0;
    } catch (error) {
      if (error.name !== "AbortError") box.hidden = true;
    }
  }

  function resultItem(r) {
    if (r.synopsis) synopses.set(r.path, r.synopsis);
    return h("li", {},
      h("a", { class: "result", href: `/?m=${encodeURIComponent(r.path)}` },
        h("span", { class: "result-path" }, r.path),
        h("span", { class: "result-meta" }, r.version && h("span", { class: "pill p-version" }, r.version)),
        h("p", { class: "result-syn" }, r.synopsis || "No summary.")));
  }

  // ---------- Library view for public modules (/?m=…) ----------
  const librarySection = $("library");
  const libraryBody = $("library-body");
  if (!librarySection) return;

  function readState() {
    const params = new URLSearchParams(location.search);
    return { m: params.get("m") || "", v: params.get("v") || "" };
  }

  function navigate(state) {
    const params = new URLSearchParams();
    for (const key of ["m", "v"]) if (state[key]) params.set(key, state[key]);
    history.pushState(state, "", params.toString() ? `/?${params}` : "/");
    render(state, true);
  }

  function render(state, scroll) {
    if (state.m) {
      showLibrary(state, scroll);
    } else {
      librarySection.hidden = true;
      document.title = "Gopherdex · Go library search";
    }
  }

  window.addEventListener("popstate", () => render(readState(), false));

  document.addEventListener("click", (event) => {
    const link = event.target.closest("[data-module]");
    if (link && !event.metaKey && !event.ctrlKey && !event.shiftKey) {
      event.preventDefault();
      navigate({ m: link.getAttribute("data-module"), v: link.getAttribute("data-version") || "" });
    }
  });

  // ---------- Library ----------
  async function showLibrary(state, scroll) {
    document.title = `${state.m} · Gopherdex`;
    librarySection.hidden = false;
    libraryBody.replaceChildren(backButton(state), h("p", { class: "loading" }, `Loading the release history of ${state.m}…`));
    if (scroll) librarySection.scrollIntoView({ block: "start" });

    const url = `/api/modules/${state.m.split("/").map(encodeURIComponent).join("/")}` + (state.v ? `?version=${encodeURIComponent(state.v)}` : "");
    try {
      const mod = await getJSON(url);
      libraryBody.replaceChildren(backButton(state), ...libraryView(mod, state));
    } catch (error) {
      if (error.name === "AbortError") return;
      libraryBody.replaceChildren(backButton(state),
        h("h2", {}, state.m),
        h("p", { class: "notice danger" }, error.message));
    }
  }

  function backButton() {
    return h("button", { type: "button", class: "btn btn-link back", onclick: () => history.back() }, "← Back");
  }

  function libraryView(mod, state) {
    const hosted = mod.origin === "hosted";
    if (hosted) hostedPaths.add(mod.path);
    const selected = mod.versions.find((v) => v.version === mod.version);
    const latest = mod.versions.find((v) => v.version === mod.latest);
    const synopsis = mod.synopsis || synopses.get(mod.path) || "";
    const install = installCommand(mod.path, mod.version === mod.latest ? "latest" : mod.version);

    const head = h("div", { class: "lib-head" },
      h("div", {},
        h("span", { class: `pill ${hosted ? "p-hosted" : "p-public"}` }, hosted ? "Hosted in this registry" : "Public module"),
        h("h2", {}, mod.path),
        synopsis && h("p", {}, synopsis)),
      h("div", { class: "lib-actions" },
        h("div", { class: "cmd" }, h("code", {}, install), h("button", { type: "button", "data-copy": install }, "Copy")),
        h("div", { class: "links" },
          mod.docsURL && h("a", { href: mod.docsURL, rel: "noopener" }, "Docs on pkg.go.dev ↗"),
          mod.repository && h("a", { href: mod.repository, rel: "noopener nofollow" }, "Repository ↗"),
          mod.modURL && h("a", { href: mod.modURL }, "go.mod"),
          mod.zipURL && h("a", { href: mod.zipURL }, "Source zip"))));

    const stats = h("dl", { class: "stats" },
      stat("Latest", mod.latest),
      stat("Released", latest && hasTime(latest.time) ? dateFormat.format(new Date(latest.time)) : "—"),
      stat("Versions", String(mod.totalVersions)),
      stat("Go", mod.goMod && mod.goMod.go ? mod.goMod.go : "—"));

    const notices = [];
    if (mod.deprecated) notices.push(h("p", { class: "notice danger" }, h("strong", {}, "Deprecated. "), mod.deprecated));
    if (selected && selected.retracted) {
      notices.push(h("p", { class: "notice danger" }, h("strong", {}, `${mod.version} is retracted. `), selected.retractRationale || "The authors ask you not to use it."));
    }
    for (const warning of mod.warnings || []) notices.push(h("p", { class: "notice" }, warning));

    return [head, stats, ...notices, h("div", { class: "lib-grid" }, historyPanel(mod, state), docsPanel(mod))];
  }

  // installCommand mirrors the server's rule: modules under a public module
  // host install with plain go get; everything else served here needs the
  // registry's proxy named explicitly.
  function installCommand(path, version) {
    const cmd = `go get ${path}@${version}`;
    const d = document.body.dataset;
    const ownHost = path.startsWith(d.moduleHost + "/");
    if (ownHost && d.zeroConfig === "true") return cmd;
    if (!ownHost && !hostedPaths.has(path)) return cmd; // public module from proxy.golang.org
    return `GOPROXY=${d.proxyUrl} GONOSUMDB=${path.split("/")[0]} ${cmd}`;
  }

  function stat(label, value) {
    return h("div", {}, h("dt", {}, label), h("dd", {}, value));
  }

  function historyPanel(mod, state) {
    const hosted = mod.origin === "hosted";
    const list = h("ol", { class: "timeline" });
    for (const v of mod.versions) {
      const classes = [v.version === mod.latest && "is-latest", v.retracted && "is-retracted"].filter(Boolean).join(" ");
      let action;
      if (v.version === mod.version) {
        action = h("span", { class: "v-action muted" }, "Viewing");
      } else if (hosted) {
        action = h("a", { class: "v-action", href: `/?m=${encodeURIComponent(mod.path)}&v=${encodeURIComponent(v.version)}`, "data-module": mod.path, "data-version": v.version }, "View docs");
      } else {
        action = h("a", { class: "v-action", href: v.docsURL, rel: "noopener" }, "Docs ↗");
      }
      list.append(h("li", { class: classes || null, "aria-current": v.version === mod.version ? "true" : null },
        h("span", { class: "dot", "aria-hidden": "true" }),
        h("div", {},
          h("div", { class: "v-line" },
            h("span", { class: "v-name" }, v.version),
            v.version === mod.latest && h("span", { class: "pill p-latest" }, "Latest"),
            v.prerelease && h("span", { class: "pill p-pre" }, "Pre-release"),
            v.retracted && h("span", { class: "pill p-retracted" }, "Retracted")),
          h("div", { class: "v-meta" }, when(v.time), v.commit && [" · ", h("code", {}, v.commit.slice(0, 7))]),
          v.retracted && v.retractRationale && h("p", { class: "v-note" }, v.retractRationale)),
        action));
    }
    const shown = mod.versions.length;
    return h("section", { class: "panel", "aria-labelledby": "history-title" },
      h("div", { class: "panel-head" },
        h("h3", { id: "history-title" }, "Release history"),
        h("p", {}, "Newest first")),
      list,
      mod.totalVersions > shown && h("p", { class: "more" },
        `Showing the newest ${shown} of ${mod.totalVersions} versions. `,
        mod.versionsURL && h("a", { href: mod.versionsURL, rel: "noopener" }, "See all on pkg.go.dev ↗")));
  }

  function docsPanel(mod) {
    const blocks = [];

    if (mod.origin === "hosted") {
      const packages = h("div", {});
      if (mod.packages.length === 0) {
        packages.append(h("p", { class: "docs-empty" }, `${mod.version} has no Go packages to document.`));
      }
      mod.packages.forEach((pkg, i) => packages.append(packageDetails(pkg, i === 0)));
      blocks.push(h("section", { class: "panel", "aria-labelledby": "docs-title" },
        h("div", { class: "panel-head" }, h("h3", { id: "docs-title" }, "Documentation"), h("p", {}, `${mod.version} · ${plural(mod.packages.length, "package")}`)),
        packages));
    } else {
      blocks.push(h("section", { class: "panel" },
        h("div", { class: "panel-head" }, h("h3", {}, "Documentation"), h("p", {}, mod.version)),
        h("div", { class: "panel-body" },
          h("p", { class: "muted" }, "Docs for public modules are published on pkg.go.dev, built from the same version."),
          h("a", { class: "btn btn-primary", href: mod.docsURL, rel: "noopener" }, `Open ${mod.version} docs ↗`))));
    }

    if (mod.goMod) {
      const direct = mod.goMod.require.filter((r) => !r.indirect);
      const indirect = mod.goMod.require.length - direct.length;
      const list = h("ul", { class: "req" });
      if (mod.goMod.require.length === 0) list.append(h("li", {}, h("span", { class: "muted" }, "No dependencies.")));
      for (const r of mod.goMod.require) {
        list.append(h("li", {}, h("code", {}, r.path), h("span", { class: "muted" }, r.version, r.indirect ? " · indirect" : "")));
      }
      blocks.push(h("section", { class: "panel" },
        h("div", { class: "panel-head" }, h("h3", {}, "Dependencies"),
          h("p", {}, `${plural(direct.length, "direct requirement")}${indirect ? `, ${indirect} indirect` : ""}`)),
        list));
    }

    if (mod.readme) {
      blocks.push(h("details", { class: "panel pkg" },
        h("summary", {}, h("code", {}, "README"), h("span", {}, "From the module source")),
        h("pre", { class: "readme" }, mod.readme)));
    }
    return h("div", { class: "stack" }, blocks);
  }

  function packageDetails(pkg, open) {
    const body = h("div", { class: "pkg-body" });
    if (pkg.doc) body.append(docText(pkg.doc));
    if (pkg.funcs.length) {
      body.append(h("h4", {}, "Functions"));
      for (const fn of pkg.funcs) body.append(symbol(fn));
    }
    if (pkg.types.length) {
      body.append(h("h4", {}, "Types"));
      for (const t of pkg.types) {
        const el = symbol(t);
        for (const fn of [...t.funcs, ...t.methods]) el.append(symbol(fn));
        body.append(el);
      }
    }
    if (!pkg.doc && !pkg.funcs.length && !pkg.types.length) {
      body.append(h("p", { class: "muted" }, "This package has no exported functions or types."));
    }
    return h("details", { class: "pkg", open },
      h("summary", {}, h("code", {}, pkg.importPath), h("span", {}, pkg.synopsis || `package ${pkg.name}`)),
      body);
  }

  function symbol(sym) {
    return h("div", { class: "sym" }, h("pre", {}, sym.decl), sym.doc && docText(sym.doc));
  }

  // docText renders a Go doc comment: blank lines separate paragraphs,
  // indented blocks are code, and other line breaks are just wrapping.
  function docText(text) {
    const el = h("div", { class: "doc-text" });
    for (const block of text.trim().split(/\n\s*\n/)) {
      const lines = block.split("\n");
      if (lines.every((line) => /^[ \t]/.test(line))) {
        el.append(h("pre", {}, lines.map((line) => line.replace(/^\t| {1,4}/, "")).join("\n")));
      } else {
        el.append(h("p", {}, lines.map((line) => line.trim()).join(" ")));
      }
    }
    return el;
  }

  render(readState(), false);
})();

// Symbol filter on the documentation tab: typing narrows each package's
// index; Enter jumps to the first match.
(() => {
  const input = document.querySelector("[data-sym-filter]");
  if (!input) return;
  const items = [...document.querySelectorAll("[data-sym-list] li")];
  const matches = () => items.filter((li) => !li.hidden);
  input.addEventListener("input", () => {
    const q = input.value.trim().toLowerCase();
    for (const li of items) li.hidden = q !== "" && !li.textContent.toLowerCase().includes(q);
    for (const d of document.querySelectorAll(".sym-index")) if (q) d.open = true;
  });
  input.addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    const first = matches()[0];
    if (first) {
      e.preventDefault();
      first.querySelector("a").click();
    }
  });
})();
