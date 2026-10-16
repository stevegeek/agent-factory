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

  // An answer can take a while (opening a PR pushes to GitHub): the tapped button says what is
  // happening and the form takes no second tap. Buttons are not disabled, since a disabled submitter
  // would drop its value from the request.
  var BUSY = { open_pr: "Opening the PR…", merge: "Merging…", file: "Filing…",
               approve: "Approving…", start: "Starting…", retry: "Sending it back…", send_back: "Sending it back…",
               redirect: "Sending it to the designer…", done: "Closing…", drop: "Closing…", not_now: "Closing…" };
  // capture: this runs before htmx's own listener on the form, which takes the submit over
  document.addEventListener("submit", function (e) {
    var form = e.target, btn = e.submitter;
    if (e.defaultPrevented || !form.classList) return;
    if (form.classList.contains("sending")) { e.preventDefault(); e.stopPropagation(); return; }
    form.classList.add("sending");
    form.setAttribute("aria-busy", "true");
    if (!btn || !btn.classList || !btn.classList.contains("btn")) return;
    btn.classList.add("busy");
    var label = btn.querySelector("span"), text = btn.dataset.busy || BUSY[btn.value] || "Working…";
    if (label) { btn.dataset.idle = label.textContent; label.textContent = text; }
  }, true);
  // a failed or refused request leaves the page in place: the form works again
  function unbusy() {
    document.querySelectorAll("form.sending").forEach(function (f) {
      f.classList.remove("sending");
      f.removeAttribute("aria-busy");
      f.querySelectorAll(".btn.busy").forEach(function (b) {
        var label = b.querySelector("span");
        if (label && b.dataset.idle) label.textContent = b.dataset.idle;
        b.classList.remove("busy");
      });
    });
  }
  document.addEventListener("htmx:responseError", unbusy);
  document.addEventListener("htmx:sendError", unbusy);
  window.addEventListener("pageshow", function (e) { if (e.persisted) unbusy(); });

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
    mine.dataset.desk = v.dataset.desk;
    document.title = doc.title;
    badge();
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

  // ---------------------------------------------------------------- copy a draft's diff

  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-copy]");
    if (!b) return;
    var el = document.getElementById(b.dataset.copy);
    if (!el || !navigator.clipboard) return;
    navigator.clipboard.writeText(el.textContent).then(function () {
      b.textContent = "Copied";
      setTimeout(function () { b.textContent = "Copy the diff"; }, 2000);
    }).catch(function () {});
  });

  // ---------------------------------------------------------------- Live: screens that refresh themselves

  var due = {};
  function pollScreens() {
    if (document.hidden) return;
    var t = Date.now();
    document.querySelectorAll("[data-poll]").forEach(function (el) {
      var url = el.dataset.poll;
      if (el.dataset.loading || (due[url] && due[url] > t && el.dataset.filled)) return;
      due[url] = t + 1000 * (parseInt(el.dataset.every, 10) || 5);
      el.dataset.loading = "1";
      fetch(url, { cache: "no-store", credentials: "same-origin", redirect: "manual" })
        .then(function (r) { return r.status === 200 ? r.text() : null; })
        .then(function (text) {
          if (text === null) return;
          // follow the end, unless you scrolled up to read
          var atEnd = !el.dataset.filled || el.scrollHeight - el.scrollTop - el.clientHeight < 40;
          if (el.textContent !== text) el.textContent = text;
          el.dataset.filled = "1";
          if (atEnd) el.scrollTop = el.scrollHeight;
        })
        .catch(function () {})
        .then(function () { delete el.dataset.loading; });
    });
  }
  setInterval(pollScreens, 1000);
  document.addEventListener("htmx:afterSettle", pollScreens);
  pollScreens();

  // ---------------------------------------------------------------- installed app: worker, badge, notifications

  if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js").catch(function () {});

  // the home-screen icon shows how many asks wait on the desk
  function badge() {
    var live = document.getElementById("live"), n = live ? Number(live.dataset.desk) : NaN;
    if (isNaN(n) || !navigator.setAppBadge) return;
    (n > 0 ? navigator.setAppBadge(n) : navigator.clearAppBadge()).catch(function () {});
  }

  function post(path, fields) {
    var meta = document.querySelector('meta[name="csrf"]'), body = new URLSearchParams(fields);
    body.set("csrf", meta ? meta.content : "");
    return fetch(path, { method: "POST", body: body, credentials: "same-origin" }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.error || "the server said " + r.status);
        return j;
      });
    });
  }

  function keyBytes(s) {
    var raw = atob((s + "===".slice((s.length + 3) % 4)).replace(/-/g, "+").replace(/_/g, "/"));
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  }

  function settingsPage() {
    var box = document.getElementById("push");
    if (!box || !box.dataset.key || box.dataset.ready) return;
    box.dataset.ready = "1";
    var state = document.getElementById("push-state"), on = document.getElementById("push-on"),
        test = document.getElementById("push-test"), off = document.getElementById("push-off");
    function show(text, sub) {
      state.textContent = text;
      on.hidden = !!sub; test.hidden = !sub; off.hidden = !sub;
    }
    function fail(err) { state.textContent = "Did not work: " + (err && err.message || err); }
    if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) {
      state.textContent = "This browser can't receive notifications here. On an iPhone, add Foreman to the Home Screen and open it from there.";
      return;
    }
    if (Notification.permission === "denied") {
      state.textContent = "Notifications are blocked for Foreman. Allow them in the phone's Settings → Notifications → Foreman.";
      return;
    }
    var reg;
    navigator.serviceWorker.ready.then(function (r) { reg = r; return r.pushManager.getSubscription(); })
      .then(function (sub) { show(sub ? "On for this device." : "Off for this device.", sub); })
      .catch(fail);
    on.addEventListener("click", function () {
      // the permission prompt must come straight from the tap
      Notification.requestPermission().then(function (p) {
        if (p !== "granted") throw new Error("permission was not given");
        return reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(box.dataset.key) });
      }).then(function (sub) {
        return post("/push/subscribe", { subscription: JSON.stringify(sub) }).then(function () { show("On for this device.", sub); });
      }).catch(fail);
    });
    test.addEventListener("click", function () {
      post("/push/test", {}).then(function (j) { state.textContent = "Sent to " + j.sent + " of " + j.devices + " device(s)."; }).catch(fail);
    });
    off.addEventListener("click", function () {
      reg.pushManager.getSubscription().then(function (sub) {
        if (!sub) return show("Off for this device.", null);
        return post("/push/unsubscribe", { endpoint: sub.endpoint }).then(function () { return sub.unsubscribe(); })
          .then(function () { show("Off for this device.", null); });
      }).catch(fail);
    });
  }

  function pageReady() { badge(); settingsPage(); }
  document.addEventListener("htmx:afterSettle", pageReady);
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", pageReady);
  else pageReady();

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
