// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"errors"

	"lectureflow/internal/model"
)

// Error codes of the client (v2 P14.3). The errors of Complete are
// *model.CodedError (job.error stores the code and params, the Chinese text
// is the detail); the Chinese texts with their {param} placeholders are in
// testdata/api/error_codes_backend.json. role params hold a role id
// (model.Role*), translated by the front end before interpolation.
const (
	// target resolution
	CodeSnapshotProviderDeleted = "llm.snapshot_provider_deleted" // provider
	CodeNoModel                 = "llm.no_model"                  // role
	CodeNoAPIKey                = "llm.no_api_key"                // provider
	CodeUnknownRole             = "llm.unknown_role"              // role (config.toml only)
	CodeProviderNotConfigured   = "llm.provider_not_configured"   // role, provider (config.toml only)

	// the last failure of a request (SPEC §12, §17.6, appendix D2); every
	// one has role and retries (transport retries made), some more
	CodeTruncated     = "llm.truncated"            // role, retries, provider, model, max_tokens (0 = the server's limit)
	CodeRefusal       = "llm.refusal"              // role, retries, provider, model, category ("" = none given)
	CodeNetwork       = "llm.network"              // role, retries
	CodeTimeout       = "llm.timeout"              // role, retries, seconds
	CodeIdleTimeout   = "llm.idle_timeout"         // role, retries, seconds (the idle timeout of a streamed request; SEC4 M5)
	CodeTimeLimit     = "llm.time_limit"           // role, retries, seconds, provider, model (a streamed answer still coming at the attempt timeout; SEC4 M4)
	CodeBadResponse   = "llm.bad_response"         // role, retries
	CodeRejected      = "llm.rejected"             // role, retries, status, provider, model
	CodeNoBalance     = "llm.insufficient_balance" // role, retries, provider
	CodeRateLimited   = "llm.rate_limited"         // role, retries
	CodeOverloaded    = "llm.overloaded"           // role, retries
	CodeServerError   = "llm.server_error"         // role, retries, status
	CodeRequestFailed = "llm.request_failed"       // role, retries, status, provider, model

	// "测试连接" and "探测模型" (A5.3); the provider's own body is only in the detail
	CodeProbeNoModel       = "llm.probe.no_model"             //
	CodeProbeNoModelNoList = "llm.probe.no_model_list_failed" //
	CodeProbeBadBaseURL    = "llm.probe.bad_base_url"         //
	CodeProbeTimeout       = "llm.probe.timeout"              // seconds
	CodeProbeNetwork       = "llm.probe.network"              //
	CodeProbeHTTP          = "llm.probe.http"                 // status
	CodeProbeNotJSON       = "llm.probe.not_json"             //
	CodeProbeBadModelList  = "llm.probe.bad_model_list"       //
	CodeProbeNoAPIKey      = "llm.probe.no_api_key"           // provider
	// CodeProbeFailed: a failed probe whose error has no code of its own.
	CodeProbeFailed = "llm.probe.failed"
)

// ErrorCodes lists the codes this package defines.
var ErrorCodes = []string{
	CodeSnapshotProviderDeleted, CodeNoModel, CodeNoAPIKey, CodeUnknownRole, CodeProviderNotConfigured,
	CodeTruncated, CodeRefusal, CodeNetwork, CodeTimeout, CodeIdleTimeout, CodeTimeLimit, CodeBadResponse, CodeRejected, CodeNoBalance,
	CodeRateLimited, CodeOverloaded, CodeServerError, CodeRequestFailed,
	CodeProbeNoModel, CodeProbeNoModelNoList, CodeProbeBadBaseURL, CodeProbeTimeout, CodeProbeNetwork,
	CodeProbeHTTP, CodeProbeNotJSON, CodeProbeBadModelList, CodeProbeNoAPIKey, CodeProbeFailed,
}

// probeError is an error of TestConnection / ProbeModels: its text is the
// Chinese message as before (the API caches it as text: "探测模型失败：…")
// and errors.As finds its coded form, with the message as the detail. The
// message never contains the key.
type probeError struct {
	msg   string
	coded *model.CodedError
}

func (e *probeError) Error() string { return e.msg }

// As lets errors.As(err, &ce) with ce *model.CodedError find the coded form.
func (e *probeError) As(target any) bool {
	if p, ok := target.(**model.CodedError); ok {
		*p = e.coded
		return true
	}
	return false
}

func probeErr(code string, params map[string]any, msg string) error {
	return &probeError{msg: msg, coded: &model.CodedError{Info: model.ErrorInfo{Code: code, Params: params}, Err: errors.New(msg)}}
}

// failedTest is the TestResult of a failed test: Error the message,
// ErrorInfo its code with the message as the detail.
func failedTest(err error) TestResult {
	res := TestResult{Error: err.Error()}
	var ce *model.CodedError
	if errors.As(err, &ce) {
		info := ce.Info
		info.Detail = err.Error()
		res.ErrorInfo = &info
	}
	return res
}
