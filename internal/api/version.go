package api

const (
	// APIVersion is the version of the REST API under /v1. It only grows,
	// and only when a change could break an existing client (a removed or
	// renamed field, a changed meaning); additions do not change it.
	APIVersion = 1
	// MinClientAPI is the oldest API version a client may have been written
	// for and still work against this daemon. A client whose own version is
	// lower than this must ask its user to update the app.
	MinClientAPI = 1
)
