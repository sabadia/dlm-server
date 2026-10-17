// Package browser manages the internal headless Chromium browser: discovery,
// pooling, network sniffing and cookie synchronisation.
//
// # Browsers
//
// Manager runs browsers on demand through the Chrome DevTools Protocol
// (chromedp). It uses the configured executable, else the first installed
// Chrome, Chromium, Edge or Brave (Discover), else a chrome-headless-shell
// downloaded from the Chrome for Testing endpoints (Fetcher). Every site group
// (SiteGroup: the registrable domain) has its own persistent profile and at
// most one browser process; the number of processes is bounded, idle ones are
// shut down gracefully so cookies reach the profile.
//
// # Pages
//
// Resolver opens a page in the browser of its group: the stored cookies of the
// site go in (PushCookies), a Sniffer records what the page downloads, and the
// cookies the page set come back (PullCookies). The Sniffer reports HLS and
// DASH manifests, progressive video and audio, file responses and downloads
// the browser starts, including JS-made blob: downloads, which only exist in
// the browser and are saved to a staging directory. Stream segments, later
// range requests, images, scripts and API calls are not candidates.
//
// A PageResult and its staged files are kept for a while. The browser engine
// (engine/browserx) imports a staged file as a task, and the page-reload
// refresh strategy (refresh.PageReload) reopens a task's page to find its new
// link.
//
// # Tests
//
// Tests that need a real browser carry the build tag "browser" and run with
// `go test -tags browser ./internal/browser/...`; CI runs them on Linux.
package browser
