package errcodes

type ErrorCode string

const (
	// PinnedModuleMissingProvider is the short code for when a pinned module definition is requested in the catalogue,
	// but the provider has been deleted.
	PinnedModuleMissingProvider ErrorCode = "MOD-001"
)
