// Keeps an open page current without a reload, and keeps a note until it is sent.
// Every 30 s (while the tab is visible) it asks for the page's own URL with the board version the
// page was rendered from; the server answers 204 until something changes. A page with a form you
// have started on is never replaced: only the parts outside it update, and if the question itself
// moved on, a bar says so.
(function () {
  "use strict";
  var EVERY = 30000, DRAFT = "foreman-draft:", KEEP = 7 * 864e5;
  var busy = false, stopped = false, last = Date.now();

  function store(fn) {
    try { return fn(window.localStorage); } catch (e) { return null; }
  }

  function readDraft(s, key) {
    try {
      var d = JSON.parse(s.getItem(key) || "null");
      return d && typeof d.text === "string" ? d : null;
    } catch (e) { return null; }
  }

  function saveDraft(id, d) {
    store(function (s) {
      if (d) s.setItem(DRAFT + id, JSON.stringify(d)); else s.removeItem(DRAFT + id);
    });
  }

  function restoreDrafts() {
    store(function (s) {
      document.querySelectorAll("textarea[data-draft]").forEach(function (ta) {
        var d = readDraft(s, DRAFT + ta.dataset.draft);
        if (d && !d.sent && !ta.value) ta.value = d.text;
      });
    });
  }

  // After an answer the page shows a toast naming the ticket ("T0003: …"): that ticket's sent note
  // is forgotten if it went through. Any other sent note never got its toast (a failed or expired
  // request), so it becomes a draft again.
  function settleDrafts(lost) {
    var toast = document.querySelector(".toast"), m = toast && /^(T[0-9]{4,6}):/.exec(toast.textContent.trim());
    var done = m && !toast.classList.contains("error") ? DRAFT + m[1] : null;
    store(function (s) {
      for (var i = s.length - 1; i >= 0; i--) {
        var k = s.key(i);
        if (!k || k.indexOf(DRAFT) !== 0) continue;
        var d = readDraft(s, k);
        if (!d || Date.now() - d.at > KEEP || (d.sent && k === done)) s.removeItem(k);
        else if (d.sent && (lost || m)) { d.sent = false; s.setItem(k, JSON.stringify(d)); }
      }
    });
    restoreDrafts();
  }

  document.addEventListener("input", function (e) {
    var ta = e.target;
    if (ta.matches && ta.matches("textarea[data-draft]")) {
      saveDraft(ta.dataset.draft, ta.value.trim() ? { text: ta.value, at: Date.now() } : null);
    }
  });

  // An answer that needs the note, clicked with the box empty: point at the box instead of a round trip
  // to an error. The server still refuses it without a note.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest && e.target.closest("button[data-needs-note]");
    var ta = btn && btn.form && btn.form.querySelector("textarea[name=note]");
    if (!ta || ta.value.trim()) return;
    e.preventDefault();
    var box = ta.closest(".composer"), hint = box && box.querySelector(".hint");
    if (box) box.classList.add("want");
    if (hint) hint.textContent = "Write the note first: “" + btn.querySelector("span").textContent + "” sends it to the agent.";
    ta.focus();
  }, true);
  document.addEventListener("input", function (e) {
    var box = e.target.closest && e.target.closest(".composer.want");
    if (box && e.target.value.trim()) box.classList.remove("want");
  });

  document.addEventListener("submit", function (e) {
    var ta = e.target.querySelector && e.target.querySelector("textarea[data-draft]");
    if (ta && ta.value.trim()) saveDraft(ta.dataset.draft, { text: ta.value, at: Date.now(), sent: true });
  }, true);

  // ---------------------------------------------------------------- live updates

  function started() {
    var f = document.querySelector("#detail form.act");
    if (!f) return false;
    if (f.contains(document.activeElement)) return true;
    var ta = f.querySelector("textarea");
    if (ta && ta.value.trim()) return true;
    var boxes = f.querySelectorAll("input[type=checkbox]");
    for (var i = 0; i < boxes.length; i++) if (boxes[i].checked !== boxes[i].defaultChecked) return true;
    return false;
  }

  function stamp(doc) {
    var h = doc.getElementById("tk-head");
    return h ? h.dataset.stamp : "";
  }

  function swap(old, fresh) {
    var open = {};
    old.querySelectorAll("details[open][data-key]").forEach(function (d) { open[d.dataset.key] = true; });
    fresh.querySelectorAll("details[data-key]").forEach(function (d) { if (open[d.dataset.key]) d.open = true; });
    old.replaceWith(fresh);
    if (window.htmx) window.htmx.process(fresh);
  }

  function movedOn() {
    var detail = document.getElementById("detail");
    if (!detail || document.getElementById("moved")) return;
    var bar = document.createElement("div"), text = document.createElement("span"), btn = document.createElement("button");
    bar.id = "moved";
    bar.className = "moved";
    bar.setAttribute("role", "status");
    text.textContent = "This has moved on since you opened it. Your note is kept.";
    btn.type = "button";
    btn.className = "btn";
    btn.textContent = "Reload";
    btn.addEventListener("click", function () { window.location.reload(); });
    bar.append(text, btn);
    detail.prepend(bar);
  }

  function apply(doc) {
    var app = document.querySelector(".app"), fresh = doc.querySelector(".app"), v = doc.getElementById("live");
    var mine = document.getElementById("live");
    if (!app || !fresh || !v || !mine) return;
    // a different question must never slide in under the pointer: only a reload brings it
    var moved = stamp(doc) !== stamp(document);
    if (!moved && !started()) {
      swap(app, fresh);
      restoreDrafts();
    } else {
      document.querySelectorAll("[data-live][id]").forEach(function (el) {
        if (moved && el.closest("#detail")) return;
        var f = doc.getElementById(el.id);
        if (f) swap(el, f);
      });
    }
    if (moved) {
      movedOn();
      stopped = true; // the bar stays until a reload; polling again would only hide that the form is stale
      return;
    }
    mine.dataset.v = v.dataset.v;
    document.title = doc.title;
  }

  function poll() {
    var live = document.getElementById("live");
    if (stopped || busy || document.hidden || !live) return;
    busy = true;
    last = Date.now();
    fetch(window.location.href, { headers: { "X-Foreman-Live": live.dataset.v }, cache: "no-store",
                                  credentials: "same-origin", redirect: "manual" })
      .then(function (r) {
        if (r.type === "opaqueredirect") { stopped = true; return null; } // logged out: the next click shows the login
        if (r.status !== 200) return null;
        return r.text();
      })
      .then(function (html) {
        // the page may have changed under the request (a click, an answer): then this answer is stale
        if (html && document.getElementById("live") === live) apply(new DOMParser().parseFromString(html, "text/html"));
      })
      .catch(function () {})
      .then(function () { busy = false; });
  }

  setInterval(poll, EVERY);
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden && Date.now() - last >= EVERY) poll();
  });
  document.addEventListener("htmx:afterSettle", function () { settleDrafts(false); });
  // htmx does not swap an error answer (an expired login or form) or a failed request: nothing was sent
  document.addEventListener("htmx:responseError", function () { settleDrafts(true); });
  document.addEventListener("htmx:sendError", function () { settleDrafts(true); });
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", function () { settleDrafts(true); });
  else settleDrafts(true);
})();
