// Package nativehost implements the browser Native Messaging host and its
// manifest installer.
//
// # Host
//
// The browser extension reaches the daemon over REST, but it needs to know
// where the daemon listens and to be given a token of its own. Native
// Messaging is the channel browsers offer for that: `dlm native-host` is
// started by the browser, talks the framed JSON protocol on stdin/stdout (see
// ReadMessage) and answers:
//
//	{"type":"connect"}  ->  {"type":"connect","ok":true,"baseUrl":"http://127.0.0.1:50123",
//	                         "running":true,"version":"1.2.3","apiVersion":1,"tlsFingerprint":"sha256:…"}
//	{"type":"ping"}     ->  {"type":"ping","ok":true}
//	{"type":"pair","name":"…","browser":"chrome","version":"0.1.0"}
//	                    ->  {"type":"pair","ok":true,"pairingId":"…","status":"pending","code":"123456","expiresAt":"…"}
//	{"type":"pair.status","pairingId":"…"}
//	                    ->  {"type":"pair.status","ok":true,"status":"approved","token":"dlmt_…"}
//
// connect reads the daemon's real address from runtime.json on every call, so a
// daemon that started on a random port or restarted is found; the address baked
// into the launcher is only the fallback when no daemon runs. The primary token
// is never handed to the extension. pair asks the daemon (with the primary
// token, which only the host reads from the token file) for a pairing; the user
// allows it in the DLM app, comparing the code, and pair.status returns the
// extension's own token, once. The extension's id comes from the browser (the
// caller origin, or the add-on id Firefox passes), not from the message.
//
// Failures reply {"ok":false,"error":CODE,"message":…} (invalid_request,
// unknown_type, origin_not_allowed, pairing_failed). A request may carry an
// "id" that the reply echoes. The host never writes anything but protocol
// frames to stdout. It reads the daemon's configuration from the config
// directory and flags only: browsers start hosts without the user's
// environment variables. The published extension's origin and add-on id are
// built in (BuiltinChromiumID, BuiltinFirefoxID); any other extension must be in
// server.allowed_origins.
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
