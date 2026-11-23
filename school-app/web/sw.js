// Service worker: shows push notifications (e.g. "your child arrived at school")
// and opens the app when a notification is tapped.
'use strict';

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

self.addEventListener('push', (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (e) { data = { body: event.data && event.data.text() }; }
  event.waitUntil(self.registration.showNotification(data.title || 'Lovely House', {
    body: data.body || '',
    icon: '/brand/icon-192.png',
    badge: '/brand/favicon.png',
    tag: data.tag,
    renotify: !!data.tag,
    data: { url: data.url || '/' },
  }));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = new URL(event.notification.data.url || '/', self.location.origin).href;
  event.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const w of wins) {
      if (new URL(w.url).origin === self.location.origin) {
        await w.focus();
        return w.navigate(url);
      }
    }
    return self.clients.openWindow(url);
  })());
});
