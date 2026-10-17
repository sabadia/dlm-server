// Package nativehost implements the browser Native Messaging host and its
// manifest installer.
//
// # Host
//
// The browser extension reaches the daemon over REST, but it needs the
// daemon's address and API token first. Native Messaging is the channel
// browsers offer for that: `dlm native-host` is started by the browser, talks
// the framed JSON protocol on stdin/stdout (see ReadMessage) and answers:
//
//	{"type":"connect"}  ->  {"type":"connect","ok":true,"baseUrl":"http://127.0.0.1:17171",
//	                         "token":"…","running":true,"version":"1.2.3"}
//	{"type":"ping"}     ->  {"type":"ping","ok":true}
//
// Failures reply {"ok":false,"error":CODE,"message":…} (invalid_request,
// unknown_type, origin_not_allowed, token_unavailable). A request may carry an
// "id" that the reply echoes. The host never writes anything but protocol
// frames to stdout. It reads the daemon's configuration from the config
// directory and flags only: browsers start hosts without the user's
// environment variables.
//
// # Installer
//
// Installer registers the host for the current user with Chrome, Chromium,
// Edge, Brave (manifest "allowed_origins") and Firefox ("allowed_extensions"):
// as manifest files in the browsers' per-user directories on Linux and macOS,
// and as HKEY_CURRENT_USER registry keys pointing to manifest files on
// Windows. The locations live in internal/platform. Manifests cannot carry
// arguments, so they point to a small launcher script that runs
// `dlm native-host` with the config directory and server URL baked in.
package nativehost
