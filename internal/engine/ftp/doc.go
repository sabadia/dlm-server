// Package ftp downloads single files over FTP, FTPS and FTPES.
//
// URL schemes: ftp:// is plain FTP (port 21), ftps:// is implicit TLS (port
// 990) and ftpes:// is explicit TLS (AUTH TLS on port 21). Credentials come
// from the URL's user info; without any the login is anonymous. The path is
// relative to the login directory (RFC 1738); a path starting with "//" (or
// "/%2F") is absolute. Certificates are verified unless the effective
// tlsVerify setting is off.
//
// A download uses one control connection and one passive data connection.
// Resume uses the REST command: the .part file is preallocated to the SIZE of
// the file and filled sequentially, a checkpoint records how much of it is
// written, and a later run continues from there. Before continuing, the size
// and modification time (MDTM) are compared with the ones the bytes were
// downloaded with; a changed file follows refresh.onMismatch (restart or
// fail). A server that refuses REST makes the download start over. Servers
// without SIZE are downloaded without resume support.
//
// Proxies: a custom socks5/socks5h proxy is honoured; an HTTP proxy cannot
// carry FTP and fails the download instead of silently bypassing it. The
// "system" proxy mode connects directly. The transfer is not split across
// connections.
package ftp
