// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"errors"
	"strconv"
	"strings"

	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/remote"
	"lectureflow/internal/state"
)

// Codes of the stages' job errors, notes and warnings (SPEC-v2 P14.3). A
// failed run returns a *model.CodedError (model.Errorf; the scheduler stores
// its ErrorInfo in job.error, the Chinese text as the detail); a note or a
// warning is written with its coded form (job.stats.note_info /
// warning_info), the Chinese text staying in note / warning until the i18n
// sweep ends. The Chinese texts with their {param} placeholders are in
// testdata/api/error_codes_backend.json.
//
// Conventions (also in that file): a note made of a main sentence and
// optional parts is the main sentence's code with the parts, in order, in
// params.extra ([]model.ErrorInfo; the front end joins them with "；").
// Lists of numbers are given as one string ("2, 5"); n is the main count
// (plural forms).

// Job errors (kind error).
const (
	CodeASRAudioMissing     = "asr.audio_missing"          // file
	CodeASRResultUnparsable = "asr.result_unparsable"      //
	CodeASRNoSpeech         = "asr.no_speech"              //
	CodeASRFakeUnreadable   = "asr.fake_result_unreadable" // path

	// SPEC-v2 I7/I9 (phase 3b freeze). device is the device's label (or
	// its id without a row). The OpenVINO worker reports the first three
	// with an "ERROR_CODE <name>" stdout line before it exits (brief
	// section A); a missing python_openvino is devices.ov_python_missing.
	CodeASROVDeviceUnavailable  = "asr.ov_device_unavailable"   // device
	CodeASROVModelPrepareFailed = "asr.ov_model_prepare_failed" // model, precision
	CodeASROVNoSpace            = "asr.ov_no_space"             // free_bytes, need_bytes (lead ruling 4: < 10 GB)
	CodeASRDeviceMissing        = "asr.device_missing"          // device: the snapshot names a device or backend this build cannot run
	CodeASRMLXDeviceUnavailable = "asr.mlx_device_unavailable"  // device: worker/asr_mlx.py ERROR_CODE device_unavailable (SPEC-v3 V5)
	CodeSelfTestNoAudio         = "selftest.no_audio"           // no repaired lecture to take the clip from (lead ruling 7)
	CodeSelfTestCompileTimeout  = "selftest.compile_timeout"    // minutes (devices.SelfTestCompileLimit)

	// Python workers (SPEC §6.0). The worker codes are {prefix}.worker_*
	// with prefix asr (asr.py) or pdf (extract_pdf.py).
	CodeWorkerPythonMissing = "worker.python_missing" // path
	CodeWorkerScriptMissing = "worker.script_missing" // path
	CodePDFResultUnparsable = "pdf.result_unparsable" //

	// No interpreter at all (3.1.0): the desktop application without the
	// component and without the key in config.toml (config WorkerPython
	// ""): the python component (extract_pdf.py) or asr-cuda (asr.py, the
	// NVIDIA and CPU devices) is not installed.
	CodeWorkerPythonNotInstalled = "worker.python_not_installed" //
	CodeWorkerFWNotInstalled     = "worker.fw_not_installed"     //

	CodePrepMediaMissing      = "prep.media_missing"       // file
	CodePrepMediaVanished     = "prep.media_vanished"      // file
	CodePrepFFmpegMissing     = "prep.ffmpeg_missing"      // path
	CodePrepFFmpegStartFailed = "prep.ffmpeg_start_failed" // path
	// Another program has the file open, so it cannot be replaced (Windows;
	// test-chaos 3): name is the file's name only (audio.wav, stream.mp4;
	// an export file's name), never its path. Retryable.
	CodePrepFileInUse   = "prep.file_in_use"   // name
	CodeExportFileInUse = "export.file_in_use" // name

	CodeSummaryNoSentences = "summary.no_sentences" //
	CodeSummaryEmpty       = "summary.empty"        //

	CodeRepairNoSummary = "repair.no_summary"        //
	CodeRepairAllFailed = "repair.all_chunks_failed" // n (chunks), attempts

	CodeTranslateNoSummary     = "translate.no_summary"                 //
	CodeTranslateFailed        = "translate.sentences_failed"           // n, ids ("#3, #17…"), attempts
	CodeTranslatePrefixChanged = "translate.incremental_prefix_changed" //

	CodeSnapshotMissing  = "snapshot.missing"        //
	CodeSentencesChanged = "stage.sentences_changed" //
	CodeOutputUnusable   = "stage.output_unusable"   // attempts

	CodeGlossaryBadSource       = "glossary.bad_source"            // source
	CodeGlossaryNoSyllabus      = "glossary.no_syllabus"           //
	CodeGlossarySyllabusMissing = "glossary.syllabus_missing"      // filename
	CodeGlossarySyllabusNotUTF8 = "glossary.syllabus_not_utf8"     // filename
	CodeGlossaryReviewEnqueue   = "glossary.review_enqueue_failed" //
	CodeMaterialFileMissing     = "material.file_missing"          // filename
	CodeMaterialNotExtracted    = "material.not_extracted"         // filename
	CodeMaterialTermsUnparsable = "material.terms_unparsable"      // filename, n (chunks), attempts
	CodeJanitorFailedFallback   = "janitor.failed_fallback"        // cause (code of the janitor's error, may be absent)
	CodeJanitorFallbackFailed   = "janitor.fallback_failed"        // the G2 rule after a janitor failure failed too
)

// worker error suffixes (codes {prefix}+suffix, see workerCodes)
const (
	workerStartFailed = ".worker_start_failed" //
	workerInputError  = ".worker_input_error"  //
	workerFailed      = ".worker_failed"       // exit_code
	workerError       = ".worker_error"        //
	workerNoOutput    = ".worker_no_output"    //
)

// worker code prefixes
const (
	workerASR    = "asr"
	workerASROV  = "asr_ov"  // worker/asr_openvino.py (SPEC-v2 I7, phase 3b freeze)
	workerASRMLX = "asr_mlx" // worker/asr_mlx.py (SPEC-v3 V5)
	workerPDF    = "pdf"
)

// Notes and warnings (kinds note, warning).
const (
	NoteGlossarySeedNoText = "glossary.seed_no_text"     //
	NoteGlossarySeedDone   = "glossary.seed_done"        // n, inserted, filled_zh; extra fallback_applied, seed_capped
	NoteFallbackApplied    = "glossary.fallback_applied" // reference, disabled
	NoteGlossarySeedCapped = "glossary.seed_capped"      // returned, max
	NoteMaterialNoText     = "material.no_text"          //
	NoteMaterialTermsDone  = "material.terms_done"       // n, inserted, filled_zh; extra fallback_applied, terms_capped, chunks_skipped, chunks_refused
	NoteMaterialCapped     = "material.terms_capped"     // chunks, returned, max
	NoteMaterialSkipped    = "material.chunks_skipped"   // total, chunks
	WarnMaterialTermsLost  = "material.terms_lost"       // filename, total, chunks, attempts

	// NoteMaterialRefused, WarnMaterialTermsRefused: chunks the provider
	// declined (llm.refusal), skipped (review fix of v3.2.x P1).
	NoteMaterialRefused      = "material.chunks_refused" // total, chunks
	WarnMaterialTermsRefused = "material.terms_refused"  // filename, total, chunks

	NoteJanitorDone        = "janitor.done"                // n, enforce, reference, disable, merge, fix_zh, rejected; extra noops, split, batches
	NoteJanitorNoops       = "janitor.noops"               // n
	NoteJanitorIgnored     = "janitor.ignored"             // n (the report's note; extra split)
	NoteJanitorSplit       = "janitor.split"               // batches
	NoteJanitorBatches     = "janitor.batches"             // batches
	NoteJanitorNothing     = "janitor.nothing_to_do"       //
	NoteJanitorUnavailable = "janitor.unavailable"         // n (terms changed by the G2 rule)
	NoteRepairChunksFailed = "repair.chunks_failed"        // total, chunks, ranges, attempts
	WarnRepairUnrepaired   = "repair.sentences_unrepaired" // ranges, attempts

	// NoteRepairChunksRefused, WarnRepairRefused: chunks the provider
	// declined (llm.refusal); their sentences keep the transcription (review
	// fix of v3.2.x P1). Joined to the two above when both happen.
	NoteRepairChunksRefused = "repair.chunks_refused"    // total, chunks, ranges
	WarnRepairRefused       = "repair.sentences_refused" // ranges

	// WarnTranslatePending (v3.2.x P1, Q2a): translate finished and n
	// sentences have no translation ("待处理"); ids are their numbers. The
	// reason of each sentence is its qa_error (qa.CodeSentenceUnusable,
	// qa.CodeSentenceRefused; kind error, shown on the sentence).
	WarnTranslatePending   = "translate.sentences_pending" // n, ids ("#3, #17…")
	NotePrepKeptOriginal   = "prep.kept_original"          // stream_status
	NotePrepEncoderRetried = "prep.stream_encoder_retried" // failed (encoders, "h264_qsv"), encoder (qsv-retry)
	WarnPrepMediaTruncated = "prep.media_truncated"        // processed, declared ("m:ss" / "h:mm:ss"; test-media I9)
	WarnPrepLowVolume      = "prep.low_volume"             // no params (round 2 N06: audio.wav below lowVolumeDBFS)
)

// Why the janitor's action was dropped (JanitorRejected.WhyCode, kind note).
const (
	WhyUnknownTerm   = "janitor.rejected.unknown_term"
	WhyLocked        = "janitor.rejected.locked"
	WhyDuplicate     = "janitor.rejected.duplicate"
	WhyUnknownAction = "janitor.rejected.unknown_action"
	WhyNoTarget      = "janitor.rejected.no_target"
	WhySelf          = "janitor.rejected.self"
	WhyTargetOff     = "janitor.rejected.target_disabled"
	WhyTargetGone    = "janitor.rejected.target_gone"
	WhyNoZh          = "janitor.rejected.no_zh"
	WhyUserZh        = "janitor.rejected.user_zh"
	WhyZhTooLong     = "janitor.rejected.zh_too_long" // over ctxbuild.TermZhMaxChars (AUD5 low-1)
)

// ErrorCodes lists the codes this package defines (errors, notes,
// warnings). The media.* codes of stream.go and the organizer's codes
// (title.*, chapters.*, filename_rule.*) are the API catalog's.
var ErrorCodes = func() []string {
	out := []string{
		CodeASRAudioMissing, CodeASRResultUnparsable, CodeASRNoSpeech, CodeASRFakeUnreadable,
		CodeASROVDeviceUnavailable, CodeASROVModelPrepareFailed, CodeASROVNoSpace, CodeASRDeviceMissing,
		CodeASRMLXDeviceUnavailable,
		CodeSelfTestNoAudio, CodeSelfTestCompileTimeout,
		CodeWorkerPythonMissing, CodeWorkerScriptMissing, CodePDFResultUnparsable,
		CodeWorkerPythonNotInstalled, CodeWorkerFWNotInstalled,
		CodePrepMediaMissing, CodePrepMediaVanished, CodePrepFFmpegMissing, CodePrepFFmpegStartFailed,
		CodePrepFileInUse, CodeExportFileInUse,
		CodeSummaryNoSentences, CodeSummaryEmpty, CodeRepairNoSummary, CodeRepairAllFailed,
		CodeTranslateNoSummary, CodeTranslateFailed, CodeTranslatePrefixChanged,
		WarnTranslatePending, qa.CodeSentenceUnusable, qa.CodeSentenceRefused, // v3.2.x P1: pending sentences
		CodeSnapshotMissing, CodeSentencesChanged, CodeOutputUnusable,
		CodeGlossaryBadSource, CodeGlossaryNoSyllabus, CodeGlossarySyllabusMissing, CodeGlossarySyllabusNotUTF8,
		CodeGlossaryReviewEnqueue, CodeMaterialFileMissing, CodeMaterialNotExtracted, CodeMaterialTermsUnparsable,
		CodeJanitorFailedFallback, CodeJanitorFallbackFailed,
		NoteGlossarySeedNoText, NoteGlossarySeedDone, NoteFallbackApplied, NoteGlossarySeedCapped,
		NoteMaterialNoText, NoteMaterialTermsDone, NoteMaterialCapped, NoteMaterialSkipped, WarnMaterialTermsLost, NoteMaterialRefused, WarnMaterialTermsRefused,
		NoteJanitorDone, NoteJanitorNoops, NoteJanitorIgnored, NoteJanitorSplit, NoteJanitorBatches, NoteJanitorNothing, NoteJanitorUnavailable,
		NoteRepairChunksFailed, WarnRepairUnrepaired, NoteRepairChunksRefused, WarnRepairRefused, NotePrepKeptOriginal, NotePrepEncoderRetried, WarnPrepMediaTruncated, WarnPrepLowVolume,
		WhyUnknownTerm, WhyLocked, WhyDuplicate, WhyUnknownAction, WhyNoTarget, WhySelf,
		WhyTargetOff, WhyTargetGone, WhyNoZh, WhyUserZh, WhyZhTooLong,
		CodeRenumberedAuto, // kinds.go: a note on the course page (lecture kinds)
		// asr_textrepair.go: "修复断句" (textrepair-int)
		CodeTextRepairNoSentences, CodeTextRepairNoWords, CodeTextRepairUnparsable, CodeTextRepairLanguage,
		// pdf_limits.go: extract_pdf.py's resource limits (security review 2, M2)
		CodePDFDecodeBudget, CodePDFMemoryLimit, CodePDFTimeout,
	}
	// asr_remote.go: the SSH inference servers' codes (ssh-mvp; defined in internal/remote)
	out = append(out, remote.ErrorCodes...)
	for _, p := range []string{workerASR, workerASROV, workerASRMLX, workerPDF, workerTextRepairCode} {
		for _, s := range []string{workerStartFailed, workerInputError, workerFailed, workerError, workerNoOutput} {
			out = append(out, p+s)
		}
	}
	return out
}()

// noteInfo is the coded form of a note made of parts (convention above):
// the first part, with the others in params.extra; nil without parts.
func noteInfo(parts ...model.ErrorInfo) *model.ErrorInfo { return state.JoinNote(parts...) }

// part is one ErrorInfo of a note.
func part(code string, params map[string]any) model.ErrorInfo {
	return model.ErrorInfo{Code: code, Params: params}
}

// numList renders numbers as a language-neutral list param: "2, 5".
func numList(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}

// codeOf is the code of err's coded form ("" when it has none).
func codeOf(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return ce.Info.Code
	}
	return ""
}

// detailOf is the text of err without its code: the wrapped error of its
// coded form when it has one (what job.error keeps as the detail).
func detailOf(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) && ce.Err != nil {
		return ce.Err.Error()
	}
	return err.Error()
}

// withText is an error with its own text that still wraps err (errors.Is
// and errors.As see through it): a detail that should not repeat err's code.
type withText struct {
	text string
	err  error
}

func (e *withText) Error() string { return e.text }
func (e *withText) Unwrap() error { return e.err }
