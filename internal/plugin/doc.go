// Package plugin runs user-written JavaScript plugins that extend dlm: they
// resolve links the built-in engines cannot, refresh expired links, and react
// to downloads starting, failing and completing.
//
// # Layout
//
// A plugin is a directory with a manifest.json and the script it names (index.js
// by default):
//
//	{
//	  "name": "example",
//	  "version": "1.0.0",
//	  "title": "Example host",
//	  "main": "index.js",
//	  "hooks": {
//	    "resolve": {"match": ["https://*.example.com/*"]},
//	    "refresh": {"match": ["https://*.example.com/*"]},
//	    "done":    {}
//	  },
//	  "net": ["api.example.com"],
//	  "timeout": "10s"
//	}
//
// # Script API
//
// The script runs in a sandbox (pure-Go goja: no file system, no processes, no
// require). The global dlm object offers:
//
//	dlm.on(hook, fn)                 register the function for a hook declared in the manifest
//	dlm.log.debug|info|warn|error(…) write to the daemon log
//	dlm.http.get(url, opts)          synchronous HTTP, only to the hosts listed in "net"
//	dlm.http.post(url, body, opts)   (opts: headers, timeout in ms); returns {status, headers, body, json()}
//	dlm.storage.get|set|delete|keys  small persistent key-value store (256 KiB)
//
// # Hooks
//
// resolve(ctx) and refresh(ctx) receive {url, originalUrl, pageUrl, referrer,
// headers, task} and return null (not handled) or {url, name, referrer,
// userAgent, headers, mirrors, mediaHint}: the link to download instead. resolve
// runs when a download is added; refresh when a task's link expired (the plugin
// refresh strategy). start(ctx), error(ctx) and done(ctx) receive {task} and
// return nothing; their errors are only logged.
//
// Every call is limited by the manifest's timeout (default 10 s, at most 60 s):
// a script that runs longer is interrupted. Calls of one plugin never overlap.
package plugin
