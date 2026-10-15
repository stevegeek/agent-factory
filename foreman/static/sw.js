// Foreman's service worker: shows the factory's notifications and opens the ticket on a tap.
// It caches nothing; the app always reads the live board.
"use strict";

self.addEventListener("install", function () { self.skipWaiting(); });
self.addEventListener("activate", function (e) { e.waitUntil(self.clients.claim()); });

self.addEventListener("push", function (e) {
  var d = {};
  try { d = e.data ? e.data.json() : {}; } catch (err) { d = { body: e.data ? e.data.text() : "" }; }
  var jobs = [self.registration.showNotification(d.title || "Foreman", {
    body: d.body || "",
    tag: d.tag || undefined,
    icon: "/static/icons/icon-192.png",
    data: { url: typeof d.url === "string" && d.url.charAt(0) === "/" ? d.url : "/" }
  })];
  // the home-screen badge shows how many asks wait on the desk
  if (typeof d.badge === "number" && self.navigator.setAppBadge) {
    jobs.push(d.badge > 0 ? self.navigator.setAppBadge(d.badge) : self.navigator.clearAppBadge());
  }
  e.waitUntil(Promise.all(jobs).catch(function () {}));
});

self.addEventListener("notificationclick", function (e) {
  e.notification.close();
  var u = new URL(e.notification.data && e.notification.data.url || "/", self.location.origin);
  var url = u.origin === self.location.origin ? u.href : self.location.origin + "/"; // only ever a Foreman page
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(function (wins) {
    for (var i = 0; i < wins.length; i++) {
      if (new URL(wins[i].url).origin === self.location.origin && "focus" in wins[i]) {
        return wins[i].focus().then(function (w) { return w.navigate ? w.navigate(url) : w; });
      }
    }
    return self.clients.openWindow(url);
  }));
});
