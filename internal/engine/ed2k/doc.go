// Package ed2k downloads files from the eDonkey2000 network: ed2k://|file|
// links, on top of the goed2k client.
//
// One client is owned by the Engine, started when the first transfer needs it
// and closed by Engine.Close. It finds sources through the servers of
// Config.Servers (host:port) and Config.ServerMet (a server.met file), and
// through Kad when enabled; with neither a download only finds sources a link
// names itself: besides the standard parts a link may end in
// "|/|sources,ip:port,ip:port|/", the eMule way of passing known peers.
//
// Resolve needs no network: a link carries the name, size and hash. A download
// fills <path>.part, which is renamed to the path when the file is complete and
// verified. The resume data of the client (completed pieces and blocks, known
// peers) is kept in the engine state, so a restart continues where it stopped.
//
// Limits: the library has no download rate limit, so the speed limit of a task
// is not applied to ed2k transfers; Config.UploadLimitKB caps the upload. A
// finished file is not shared further, unlike seeding a torrent.
package ed2k
