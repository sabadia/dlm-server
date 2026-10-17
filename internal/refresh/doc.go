// Package refresh detects expired download links and refreshes them through a
// configurable strategy chain.
//
// # Expiry detection
//
// A link is classified as expired (engine.KindLinkExpired, task error code
// "link_expired") when the origin answers 401, 403, 404 or 410, answers an
// HTML page although the file's content type is known to be something else,
// or serves different content than the one fingerprinted (another size, a
// changed strong ETag so that If-Range fails). Those checks live in the HTTP
// engine, next to the requests. This package adds the proactive half:
// SignedURLExpiry reads the expiry that signed URLs carry (AWS and Google
// signature V4, S3 V2, CloudFront, Azure SAS and generic exp/expires
// parameters next to a signature) so a refresh can happen before the link
// breaks.
//
// # Refresh chain
//
// When a download fails with an expired link and refresh.auto is on, the
// service layer (service.RefreshService) tries refresh.strategies in order:
//
//   - origin-redirect (Strategy, here): re-request the task's original URL; its
//     redirect target is the fresh link;
//   - extension-capture: the task waits in awaiting_link, up to
//     refresh.awaitTimeout, for a browser capture that matches its fingerprint;
//   - manual: the task waits in awaiting_link, without timeout, until a user
//     sets a link;
//   - page-reload (browser) and plugin: registered by later phases.
//
// A candidate link is validated before it replaces the old one: it is probed
// and its fingerprint (size, strong ETag, announced file name) compared with
// what was downloaded so far (Validate). On a match the download resumes from
// its segments; on a mismatch refresh.onMismatch decides: restart (discard the
// partial data), fail (reject the link) or ask (wait for a decision).
// refresh.maxAttempts bounds the attempts made without download progress, and
// every step is recorded in the task's refresh history.
package refresh
