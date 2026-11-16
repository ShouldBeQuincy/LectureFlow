// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

// Error codes of this package (v2 P14.3). Every user error of the settings
// service carries one (errors.As finds a *model.CodedError, see userErr);
// the Chinese texts with their {param} placeholders are in
// testdata/api/error_codes_backend.json. params named role hold a role id
// (model.Role*), roles a list of them: the front end translates them before
// interpolating.
const (
	// providers (A5.3, P12)
	CodeProviderInUse         = "settings.provider_in_use"         // roles
	CodeProviderNameTaken     = "settings.provider_name_taken"     // name
	CodeProviderNameEmpty     = "settings.provider_name_empty"     //
	CodeProviderNameTooLong   = "settings.provider_name_too_long"  // max
	CodeProviderTypeInvalid   = "settings.provider_type_invalid"   //
	CodeProviderFieldsMissing = "settings.provider_fields_missing" //
	CodeBaseURLInvalid        = "settings.base_url_invalid"        //
	CodeConcurrencyInvalid    = "settings.max_concurrency_invalid" // min, max
	CodeRPMInvalid            = "settings.rpm_invalid"             // min, max
	CodeBalanceAPIInvalid     = "settings.balance_api_invalid"     //
	CodePriceInvalid          = "settings.price_invalid"           //
	CodeTariffInvalid         = "settings.tariff_invalid"          // (not a tariff.Error; those keep tariff.*)
	CodeOffpeakNeedsPeak      = "settings.offpeak_needs_peak"      //
	// CodeProviderNotFound is the API's code for a missing provider.
	CodeProviderNotFound = "provider.not_found"

	// roles (A5.3)
	CodeRoleNotConfigured   = "settings.role_not_configured"   // role
	CodeRoleProviderDeleted = "settings.role_provider_deleted" // role
	CodeRoleUnknown         = "settings.role_unknown"          // role
	CodeRoleProviderMissing = "settings.role_provider_missing" // role
	CodeRoleModelMissing    = "settings.role_model_missing"    // role
	CodeTemperatureInvalid  = "settings.temperature_invalid"   // min, max
	CodeWindowInvalid       = "settings.window_invalid"        // min, max
	// 深度思考 (.claude/thinking-brief.md): value is the rejected level as
	// sent; provider is the provider's display name.
	CodeThinkingInvalid     = "settings.thinking_invalid"     // value
	CodeThinkingUnsupported = "settings.thinking_unsupported" // provider
	// v3.2.x (D1, P2): the role's (or a manual retry's) output limit and the
	// translate role's sentences per request.
	CodeMaxTokensInvalid = "settings.max_tokens_invalid" // min, max
	CodeBatchInvalid     = "settings.batch_invalid"      // min, max

	// ASR and general settings (§17.2, A5)
	CodeASRModelMissing    = "settings.asr_model_missing"    //
	CodeComputeTypeInvalid = "settings.compute_type_invalid" //
	CodeDeviceInvalid      = "settings.device_invalid"       //
	CodeLanguageMissing    = "settings.language_missing"     //
	CodeMaxParallelInvalid = "settings.max_parallel_invalid" // min, max
	CodeVRAMReserveInvalid = "settings.vram_reserve_invalid" // min, max
	CodeVRAMPerJobInvalid  = "settings.vram_per_job_invalid" // min, max
	CodeBudgetInvalid      = "settings.budget_invalid"       //
	// v3.2.x (P1-c): general llm_idle_timeout_s out of range.
	CodeIdleTimeoutInvalid = "settings.idle_timeout_invalid" // min, max
	// SPEC-v2 I8 (phase 3b freeze): value is the rejected device_mode as sent.
	CodeDeviceModeInvalid = "settings.device_mode_invalid" // value
	CodeMemReserveInvalid = "settings.mem_reserve_invalid" // min, max
	// Power management (v2-step20 freeze; SetASR, task P4): device_mode
	// manual needs at least one device, never a CPU device (requirement 6),
	// and only existing rows. device is the row's label (the id as sent
	// when there is no row).
	CodeManualDevicesEmpty   = "settings.manual_devices_empty"   //
	CodeManualDevicesCPU     = "settings.manual_devices_cpu"     // device
	CodeManualDevicesUnknown = "settings.manual_devices_unknown" // device

	// UI language (P14.6, P14.7)
	CodeLocaleUnsupported = "settings.locale_unsupported" // locale
	CodeZhDisplayInvalid  = "settings.zh_display_invalid" //

	// why "沿用原模型" is not available (A4.3, SnapshotUsableInfo)
	CodeSnapshotNotRecorded     = "snapshot.unusable.not_recorded"     //
	CodeSnapshotInvalid         = "snapshot.unusable.invalid"          //
	CodeSnapshotProviderDeleted = "snapshot.unusable.provider_deleted" // provider
	CodeSnapshotNoAPIKey        = "snapshot.unusable.no_api_key"       // provider
	// cred-enc (keys.go): the stored key cannot be decrypted with this
	// installation's key (another computer or account) and has to be
	// entered again; provider is the display name.
	CodeSnapshotKeyUnreadable = "snapshot.unusable.api_key_unreadable" // provider
	CodeProviderKeyUnreadable = "settings.provider_key_unreadable"     // provider
	// sec3 L2 (keys.go): there is no data key now, so a key entered now is
	// stored in the clear (error: the reason, in English); a provider's key
	// is stored in the clear.
	CodeCredentialsUnavailable = "settings.credentials_unavailable"  // error
	CodeProviderKeyUnprotected = "settings.provider_key_unprotected" // provider
	// SEC8 low-3 (helpers.go applyInput): an API key entered that starts
	// with "enc:", the prefix of a stored sealed value, is refused, so a
	// ciphertext copied from one row cannot be stored in another.
	CodeAPIKeyLooksSealed = "settings.api_key_looks_sealed" //
)

// ErrorCodes lists the codes this package defines (CodeProviderNotFound and
// the tariff.* codes belong to the API and the tariff package).
var ErrorCodes = []string{
	CodeProviderInUse, CodeProviderNameTaken, CodeProviderNameEmpty, CodeProviderNameTooLong,
	CodeProviderTypeInvalid, CodeProviderFieldsMissing, CodeBaseURLInvalid, CodeConcurrencyInvalid,
	CodeRPMInvalid, CodeBalanceAPIInvalid, CodePriceInvalid, CodeTariffInvalid, CodeOffpeakNeedsPeak,
	CodeRoleNotConfigured, CodeRoleProviderDeleted, CodeRoleUnknown, CodeRoleProviderMissing,
	CodeRoleModelMissing, CodeTemperatureInvalid, CodeWindowInvalid, CodeThinkingInvalid, CodeThinkingUnsupported,
	CodeMaxTokensInvalid, CodeBatchInvalid,
	CodeASRModelMissing, CodeComputeTypeInvalid, CodeDeviceInvalid, CodeLanguageMissing,
	CodeMaxParallelInvalid, CodeVRAMReserveInvalid, CodeVRAMPerJobInvalid, CodeBudgetInvalid, CodeIdleTimeoutInvalid,
	CodeDeviceModeInvalid, CodeMemReserveInvalid,
	CodeManualDevicesEmpty, CodeManualDevicesCPU, CodeManualDevicesUnknown,
	CodeLocaleUnsupported, CodeZhDisplayInvalid,
	CodeSnapshotNotRecorded, CodeSnapshotInvalid, CodeSnapshotProviderDeleted, CodeSnapshotNoAPIKey,
	CodeSnapshotKeyUnreadable, CodeProviderKeyUnreadable,
	CodeCredentialsUnavailable, CodeProviderKeyUnprotected, CodeAPIKeyLooksSealed,
}
