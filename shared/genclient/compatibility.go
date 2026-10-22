package genclient

// These names are part of the published SDK. Keep them available when the
// generator disambiguates enum values differently in a later release.
const (
	Activate = PluginOperationAuthorizationRequestActionActivate
	Prepare  = PluginOperationAuthorizationRequestActionPrepare
	Release  = PluginOperationAuthorizationRequestActionRelease
	Suspend  = PluginOperationAuthorizationRequestActionSuspend

	Begin     = PluginPinOperationReceiptActionBegin
	Cancel    = PluginPinOperationReceiptActionCancel
	Reconcile = PluginPinOperationReceiptActionReconcile
	Restore   = PluginPinOperationReceiptActionRestore

	// UserIdHeaderScopes retains the former generated scope key for SDK source
	// compatibility. This constant does not provide authentication or authority.
	UserIdHeaderScopes = "userIdHeader.Scopes"
)
