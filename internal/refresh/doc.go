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
package refresh
