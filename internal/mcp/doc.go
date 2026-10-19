// Package mcp exposes the application use-cases as Model Context Protocol
// tools, so an AI assistant can add, watch and manage downloads.
//
// The tools call the same services as the REST API; the daemon serves them over
// streamable HTTP at /mcp behind the API token. What an assistant may change is
// narrower than what the REST API allows: tasks cannot be given a directory,
// and settings changes are limited to the speed limit, connections, retries,
// file-exists policy and auto-categorizing. Settings that run programs or
// redirect traffic (post actions, proxies, headers, directories) are read-only
// here.
package mcp
