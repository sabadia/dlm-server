# Writing a dlm plugin

A plugin is a directory with `manifest.json` and a JavaScript file. Reach for one when
a site needs repeated logic the built-in engines lack; for a one-off, `dlm-sources` is
quicker.

## Contents

- [Manifest](#manifest)
- [Script](#script)
- [Hooks](#hooks)
- [API](#api)
- [Limits and installing](#limits-and-installing)

## Manifest

```json
{
  "name": "example-host",
  "version": "1.0.0",
  "title": "Example video host",
  "description": "Resolves example.com watch pages to files",
  "main": "index.js",
  "hooks": {
    "resolve": {"match": ["https://*.example.com/watch/*"]},
    "refresh": {"match": ["https://*.example.com/*"]},
    "done": {}
  },
  "net": ["api.example.com", "*.cdn.example.net"],
  "timeout": "15s"
}
```

| Field | Meaning |
|---|---|
| `name` | Lower-case letters, digits and dashes. |
| `version` | Any string. |
| `main` | Script, relative to the directory; default `index.js`. |
| `hooks` | Hooks implemented. `match` limits one to links matching a pattern, `*` being any text; none means every link. |
| `net` | Hosts `dlm.http` may contact. Empty means no network. |
| `timeout` | Longest one call may run: default 10s, at most 60s. |

## Script

Runs once at start, and registers handlers:

```js
dlm.on("resolve", function (ctx) {
  const page = dlm.http.get("https://api.example.com/v1/video?u=" + encodeURIComponent(ctx.url)).json();
  if (!page.file) return null;                 // not mine: let other plugins try
  return {url: page.file, name: page.title + ".mp4", referrer: ctx.url};
});

dlm.on("refresh", function (ctx) {
  const page = dlm.http.get("https://api.example.com/v1/video?u=" + encodeURIComponent(ctx.pageUrl)).json();
  return {url: page.file};
});

dlm.on("done", function (ctx) {
  dlm.log.info("finished", ctx.task.name);
  dlm.storage.set("last", ctx.task.name);
});
```

## Hooks

| Hook | When | Receives | Returns |
|---|---|---|---|
| `resolve` | A plain link is added or looked up | `{url, originalUrl, pageUrl, referrer, headers}` | `null`, or `{url, name, referrer, userAgent, headers, mirrors, mediaHint}` |
| `refresh` | A link expired and the `plugin` strategy runs | The same, plus `task` | As `resolve` |
| `start`, `error`, `done` | A task begins, fails, completes | `{event, task}` | Nothing; errors are only logged |

`task` is `{id, name, url, originalUrl, pageUrl, queueId, dir, size, downloaded, status,
error: {code, message, retryable}}`. Plugins run in name order and the first `resolve`
to return a link wins. A plugin that throws is logged and skipped, so it never stops a
download. A resolved task keeps the page as `originalUrl` and `pageUrl`.

`refresh` is opt-in: `dlm settings set 'refresh.strategies=["origin-redirect","plugin"]'`.

## API

| | |
|---|---|
| `dlm.on(hook, fn)` | Register a handler for a hook in the manifest. |
| `dlm.log.debug/info/warn/error(...)` | Log; read with `dlm plugin logs NAME`. |
| `dlm.http.get(url, {headers, timeout})` | Synchronous request. |
| `dlm.http.post(url, body, {headers, timeout})` | A string is sent as is; an object as JSON. |
| `dlm.storage.get/set/delete/keys` | Survives restarts; 256 KiB per plugin. |
| `dlm.plugin` | `{name, version}`. |

`dlm.http` answers `{status, headers (lower-case), body, json()}`. It throws for hosts
outside `net`, redirects to them, responses over 8 MiB and timeouts.

## Limits and installing

No file system, processes, `require`, `fetch` or timers. The language is ES5.1 with most
of ES6. A handler may be `async` only if it has finished when it returns, since there
is no event loop. Calls into one plugin never overlap.

`dlm plugin install SOURCE` copies a directory (no links or `.git`; at most 200 files
and 5 MiB) or clones an `https://` repository, which needs `git`. It checks the
manifest, runs the script once, and refuses a plugin that registers no handler.

After installing, test with `dlm plugin get NAME` for its status, then `dlm resolve URL`
on a matching link, then `dlm plugin logs NAME`.
