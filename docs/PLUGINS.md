# Writing a plugin

A plugin is a directory with a `manifest.json` and a JavaScript file. It extends
dlm for sites the built-in engines cannot handle: turning a page link into the
file's real URL, getting a fresh link when one expires, and reacting when a
download starts, fails or finishes.

```sh
dlm plugin install ./my-plugin          # or an https:// git URL, with --ref
dlm plugin list
dlm plugin disable my-plugin
```

A plugin runs inside the daemon. It can only reach the hosts its manifest lists, but
it still decides what links your downloads use: install plugins you trust.

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
    "refresh": {"match": ["https://*.example.com/*", "https://cdn.example.net/*"]},
    "done":    {}
  },
  "net": ["api.example.com", "*.cdn.example.net"],
  "timeout": "15s"
}
```

| Field | |
|---|---|
| `name` | Lower-case letters, digits and dashes; names the plugin. |
| `version` | Any string. |
| `main` | The script, relative to the plugin directory; default `index.js`. |
| `hooks` | The hooks the script implements. `match` limits a hook to links matching a pattern where `*` stands for any text; no `match` means every link. |
| `net` | Hosts `dlm.http` may contact (`api.example.com` or `*.example.com`). Empty: no network. |
| `timeout` | Longest a single call may run (default 10s, at most 60s). |

## Script

The script runs once when the plugin starts and registers its handlers:

```js
dlm.on("resolve", function (ctx) {
  const page = dlm.http.get("https://api.example.com/v1/video?u=" + encodeURIComponent(ctx.url)).json();
  if (!page.file) return null;                       // not mine: let other plugins try
  return {url: page.file, name: page.title + ".mp4", referrer: ctx.url, headers: {"X-Token": page.token}};
});

dlm.on("refresh", function (ctx) {
  // ctx.task has the task; ctx.url is the link that expired
  const page = dlm.http.get("https://api.example.com/v1/video?u=" + encodeURIComponent(ctx.pageUrl)).json();
  return {url: page.file};
});

dlm.on("done", function (ctx) {
  dlm.log.info("finished", ctx.task.name);
  dlm.storage.set("last", ctx.task.name);
});
```

### Hooks

| Hook | When | Receives | Returns |
|---|---|---|---|
| `resolve` | a download is added (or looked up) with a plain link | `{url, originalUrl, pageUrl, referrer, headers}` | `null` (not handled) or `{url, name, referrer, userAgent, headers, mirrors, mediaHint}` |
| `refresh` | the link of a task expired and the `plugin` refresh strategy runs | the same, plus `task` | the same as `resolve` |
| `start`, `error`, `done` | a task begins running, fails, completes | `{event, task}` | nothing (errors are only logged) |

`task` is `{id, name, url, originalUrl, pageUrl, queueId, dir, size, downloaded,
status, error: {code, message, retryable}}`. Plugins are tried in name order and the
first `resolve` that returns a link wins; a plugin that throws is logged and skipped,
so a broken plugin never stops a download. A resolved task keeps the page link as its
`originalUrl` and `pageUrl`, which is what refresh starts from. To use the plugin
refresh, list `plugin` in the `refresh.strategies` setting, for example
`dlm settings set 'refresh.strategies=["origin-redirect","plugin"]'`.

### API

| | |
|---|---|
| `dlm.on(hook, fn)` | Register the handler of a hook declared in the manifest. |
| `dlm.log.debug/info/warn/error(...)` | Write to the daemon log. |
| `dlm.http.get(url, {headers, timeout})` | Synchronous request. |
| `dlm.http.post(url, body, {headers, timeout})` | A string body is sent as is; an object as JSON. |
| `dlm.storage.get/set/delete/keys` | A small key-value store that survives restarts (256 KiB per plugin). |
| `dlm.plugin` | `{name, version}`. |

`dlm.http` answers `{status, headers (lower-case names), body, json()}`. Requests
fail (throw) for hosts outside `net`, for redirects to such hosts, for responses over
8 MiB and after the timeout. There is no file system, no processes, no `require`, no
`fetch` and no timers. The language is ES5.1 with most of ES6; a handler may be
`async` as long as it has finished when it returns, since there is no event loop.

Calls into one plugin never overlap, and each is interrupted when it runs longer than
`timeout`.

## Installing

`dlm plugin install SOURCE` copies a directory (links and `.git` are not copied; at
most 200 files and 5 MiB), or clones an `https://` repository (needs `git`), checks
the manifest, runs the script once and refuses a plugin that registers no handler.
`--replace` upgrades a plugin of the same name and keeps the one that works if the
new one is broken. `dlm plugin rm` deletes the plugin and the data it stored.
