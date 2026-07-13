package admin

// Error codes for admin API responses.
// Returned in the "error_code" JSON field alongside the human-readable "error" field.
// Frontend maps these to i18n keys via api.<error_code>.
const (
	// Common validation
	ErrInvalidID       = "invalid_id"
	ErrInvalidRequest  = "invalid_request"
	ErrNotFound        = "not_found"
	ErrConflict        = "conflict"

	// Auth / permission
	ErrForbidden               = "forbidden"
	ErrInsufficientPermissions = "insufficient_permissions"
	ErrAdminRequired           = "admin_required"
	ErrAccessDenied            = "access_denied"
	ErrInvalidCredentials      = "invalid_credentials"
	ErrTokenGenerationFailed   = "token_generation_failed"
	ErrOrgContextRequired      = "org_context_required"

	// Keys
	ErrKeyNotFound         = "key_not_found"
	ErrBudgetPeriodInvalid = "budget_period_invalid"
	ErrBudgetNegative      = "budget_negative"
	ErrGraceHoursNegative  = "grace_hours_negative"

	// Budget
	ErrAlertNotFound       = "alert_not_found"
	ErrThresholdRange      = "threshold_range"
	ErrBudgetTargetReq     = "budget_target_required"
	ErrBudgetTargetMutual  = "budget_target_mutual"
	ErrWebhookURLInvalid   = "webhook_url_invalid"
	ErrWebhookURLInternal  = "webhook_url_internal"

	// Users
	ErrUserNotFound          = "user_not_found"
	ErrInvalidRoleID         = "invalid_role_id"
	ErrOnlyAdminAssignRole   = "only_admin_assign_role"
	ErrCannotDemoteLastAdmin = "cannot_demote_last_admin"
	ErrCannotDisableLastAdmin = "cannot_disable_last_admin"
	ErrCannotDeleteSelf      = "cannot_delete_self"
	ErrCannotDeleteLastAdmin = "cannot_delete_last_admin"
	ErrOldPasswordRequired   = "old_password_required"
	ErrIncorrectOldPassword  = "incorrect_old_password"
	ErrPasswordTooShort      = "password_too_short"

	// Providers
	ErrProviderURLInvalid    = "provider_url_invalid"
	ErrProviderHasModels     = "provider_has_models"
	ErrProviderExists        = "provider_exists"

	// Models
	ErrPricesNegative        = "prices_negative"
	ErrInvalidRoutingStrategy = "invalid_routing_strategy"

	// System settings
	ErrCircuitBreakerThreshold = "circuit_breaker_threshold"
	ErrCircuitBreakerDuration  = "circuit_breaker_duration"
	ErrRetryBudgetRange        = "retry_budget_range"

	// Debug
	ErrEntryNotFound = "entry_not_found"

	// License — Import/Activate error codes moved to Commercial overlay

	// Teams
	ErrTeamNotFound          = "team_not_found"
	ErrUserAlreadyInTeam     = "user_already_in_team"
	ErrCannotRemoveLastTeamAdmin = "cannot_remove_last_team_admin"

	// Roles
	ErrRoleNotFound            = "role_not_found"
	ErrInvalidRoleName         = "invalid_role_name"
	ErrInvalidDisplayName      = "invalid_display_name"
	ErrAdminCorePermission     = "admin_core_permission"
	ErrAdminCorePermissionRemove = "admin_core_permission_remove"
	ErrSystemRoleDelete        = "system_role_delete"
	ErrRoleHasUsers            = "role_has_users"
	ErrRoleNameDuplicate       = "role_name_duplicate"

	// Insight
	ErrInsightNotFound  = "insight_not_found"
	ErrActionNotFound   = "action_not_found"
	ErrActionNotPending = "action_not_pending"
	ErrAlreadyProcessed = "already_processed"
	ErrInvalidScope     = "invalid_scope"
	ErrInvalidScenario  = "invalid_scenario"

	// Budget workflow
	ErrTargetNotFound          = "target_not_found"
	ErrRequestNotFound         = "request_not_found"
	ErrRequestNotPending       = "request_not_pending"
	ErrRequestAlreadyProcessed = "request_already_processed"
	ErrInvalidReviewAction     = "invalid_review_action"
	ErrOwnTeamOnly             = "own_team_only"
	ErrOwnTeamKeysOnly         = "own_team_keys_only"
	ErrTargetTypeInvalid       = "target_type_invalid"
	ErrTargetIDInvalid         = "target_id_invalid"

	// Guardrails
	ErrInvalidGuardrailType    = "invalid_guardrail_type"
	ErrInvalidConfig           = "invalid_config"
	ErrInvalidDirection        = "invalid_direction"
	ErrInvalidSeverity         = "invalid_severity"
	ErrInvalidAction           = "invalid_action"
	ErrRuleNotFound            = "rule_not_found"
	ErrReferencedRuleNotFound  = "referenced_rule_not_found"
	ErrInvalidChannels         = "invalid_channels"
	ErrCooldownRange           = "cooldown_range"
	ErrInvalidTimeFormat       = "invalid_time_format"
	ErrInvalidGranularity      = "invalid_granularity"

	// Cache
	ErrModelNameRequired = "model_name_required"
	ErrCacheStatsFailed  = "cache_stats_failed"
	ErrCacheTTLRange     = "cache_ttl_range"
	ErrEmbeddingsTTLRange = "embeddings_ttl_range"

	// Secret / encryption
	ErrEncryptionNotEnabled = "encryption_not_enabled"

	// File / upload
	ErrFailedReadFile = "failed_read_file"

	// Playground
	ErrModelRequired        = "model_required"
	ErrMessagesRequired     = "messages_required"
	ErrTooManyMessages      = "too_many_messages"
	ErrNoRouteForModel      = "no_route_for_model"
	ErrNoImageProvider      = "no_image_provider"
	ErrNoAudioProvider      = "no_audio_provider"
	ErrStreamingNotSupported = "streaming_not_supported"
	ErrUnexpectedResponse   = "unexpected_response"
	ErrFileRequired         = "file_required"
	ErrAudioTooLarge        = "audio_too_large"
	ErrTTSFailed            = "tts_failed"
	ErrFailedProcessAudio   = "failed_process_audio"
	ErrNoVideoProvider      = "no_video_provider"
	ErrVideoTooLarge        = "video_too_large"
	ErrVideoTaskFailed      = "video_task_failed"
	ErrVideoTaskTimeout     = "video_task_timeout"
	ErrVideoTaskNotFound    = "video_task_not_found"

	// License import/activate (used by Commercial overlay)
	ErrNoFileUploaded             = "no_file_uploaded"
	ErrEmptyLicense               = "empty_license"
	ErrInvalidLicense             = "invalid_license"
	ErrFingerprintMismatch        = "fingerprint_mismatch"
	ErrLicenseExpired             = "license_expired"
	ErrLicenseKeyRequired         = "license_key_required"
	ErrLicenseServerNotConfigured = "license_server_not_configured"
	ErrActivationFailed           = "activation_failed"
)
