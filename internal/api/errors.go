// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/glossary"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
	"lectureflow/internal/tariff"
)

// HTTP error bodies (SPEC-v2 P14.3, phase-2 lead ruling 6): every error
// response is ErrorBody {"error": {"code", "params", "detail"}}. The front
// end shows t("errors."+code, params) and offers detail under "查看详情".
//
// errorCatalog is the one list of codes: testdata/api/error_codes.json is
// generated from it ({code: {zh, params}}, for the front end and i18n;
// TestErrorCodesCatalog keeps the file in step), and its Chinese texts render
// the transitional BatchResult.Error strings (kept until the i18n extraction).

// errDef is one code of the catalog: the Chinese text with {param}
// placeholders, and the params the API sends with it.
type errDef struct {
	zh     string
	params []string
}

// Error codes. Grouped by area; external codes (tariff.*, media.* written by
// other packages) are listed too because the front end translates them.
var errorCatalog = map[string]errDef{
	// generic
	"api.not_found":                  {"没有这个接口：{method} {path}", []string{"method", "path"}},
	"request.method_not_allowed":     {"只支持 GET", nil},
	"request.bad_json":               {"请求格式错误：需要 JSON", nil},
	"request.invalid":                {"请求内容无效", nil},
	"request.nothing_to_change":      {"没有要修改的内容（{fields}）", []string{"fields"}},
	"request.bad_param":              {"参数 {name} 无效", []string{"name"}},
	"server.internal":                {"服务器内部错误", nil},
	"server.unavailable":             {"这项服务没有启动（{service}）", []string{"service"}},
	"common.not_found":               {"找不到请求的对象（可能已被删除），请刷新页面", nil},
	"common.duplicate":               {"名称已存在", nil},
	"state.conflict":                 {"当前状态不允许这个操作（状态可能刚刚变化），请刷新后再试", nil},
	"stage.busy":                     {"有阶段正在运行，请等它结束后再试", nil},
	"stage.invalid":                  {"未知的阶段或操作（{stage} / {action}）", []string{"stage", "action"}},
	"stage.unknown_action":           {"未知的操作，只支持 cancel、retry、rerun", []string{"action"}},
	model.ErrNeedsModelChoice:        {"模型设置已变化，需要选择沿用原模型还是使用当前设置", nil},
	"model_choice.snapshot_unusable": {"原模型不可用，请改选“使用当前设置”", nil},
	// v3.2.x P1-b: the retry with a model named by hand (v32x_retry.go)
	codeManualConflict:    {"临时指定的模型只能和“临时指定”这一选项一起提交", nil},
	codeManualIncomplete:  {"临时指定模型时缺少 {field}", []string{"field"}},
	codeManualUnsupported: {"只有摘要、修复和翻译的重试可以临时指定模型（{stage} / {action}）", []string{"stage", "action"}},
	// v3.2.x P1-b: "用当前设置的模型重新翻译这句" (sentenceOp, use_current)
	codeSentenceNotPending: {"这句已有可用的译文，只有未能翻译的句子才能改用当前设置的模型重新翻译", nil},

	// desktop mode and the application endpoints (SPEC-v3 V1.2, V1.1, V4)
	"auth.token_required":                 {"缺少或错误的访问令牌，请从 LectureFlow 应用中打开这个页面", nil},
	"auth.login_required":                 {"需要先登录网页访问才能继续，请前往登录页面输入访问密钥", nil},
	"auth.bad_host":                       {"请求的主机名不被允许", nil},
	"web_access.not_available":            {"此操作只能在运行 LectureFlow 的电脑上完成，不能通过网页访问执行", nil},
	"app_settings.close_behavior_invalid": {"关闭窗口时的行为“{value}”无效（ask、background 或 quit）", []string{"value"}},
	"app_settings.update_channel_invalid": {"更新渠道“{value}”无效（stable 或 beta）", []string{"value"}},
	"onboarding_state.invalid":            {"引导状态无效：{field}", []string{"field"}},
	"onboarding_state.too_large":          {"引导状态太大（最多 {max_bytes}）", []string{"max_bytes"}},
	// components and onboarding keys (SPEC-v3 V3, V4.2; handlers_components.go,
	// handlers_envkeys.go). The component manager's own codes are in
	// error_codes_backend.json (components.ErrorCodes).
	"components.sources_invalid": {"下载源设置无效（{group}：{field}）", []string{"group", "field"}},
	"onboarding.env_key_missing": {"环境变量 {env} 没有设置，无法读取 {provider_type} 的 API key", []string{"provider_type", "env"}},
	// application update relay (handlers_update.go, SPEC-v3 V8)
	"update.not_available":   {"现在没有可以下载或安装的更新", nil},
	"update.state_invalid":   {"更新状态无效（{field}）", []string{"field"}},
	"update.check_failed":    {"检查更新失败，稍后会自动再试", nil},
	"update.download_failed": {"下载更新失败，可以重试", nil},
	"update.install_failed":  {"安装更新失败，下次退出时会再试", nil},
	// settings page "App" section (SPEC-v3 V11, handlers_appinfo.go)
	"app.open_logs_failed": {"无法打开日志文件夹：{path}", []string{"path"}},
	"app.cross_site":       {"拒绝了来自其他网站的请求", nil},

	// changing the data directory and importing data (SPEC-v3 V1.3; handlers_datadir.go, internal/relocate)
	"data_dir.path_invalid":           {"“{path}”不是有效的文件夹路径，请选择一个完整的文件夹路径", []string{"path"}},
	"data_dir.path_not_allowed":       {"“{path}”在网络位置上。出于安全考虑，网络上的文件夹只能在文件夹选择框里选中后使用（之前的选择可能已失效，或应用重启过），请重新选择这个文件夹", []string{"path"}},
	"data_dir.path_not_granted":       {"出于安全考虑，“{path}”需要先在文件夹选择框里选中才能使用（之前的选择可能已失效，或应用重启过），请重新选择这个文件夹", []string{"path"}},
	"data_dir.same_dir":               {"这就是现在的数据目录，请选择另一个文件夹", nil},
	"data_dir.target_not_empty":       {"“{path}”里已经有文件。请选择一个空文件夹，或者新建一个文件夹", []string{"path"}},
	"data_dir.target_inside_source":   {"新位置不能在现在的数据目录里面，请选择别处的文件夹", nil},
	"data_dir.target_contains_source": {"新位置不能包含现在的数据目录，请选择别处的文件夹", nil},
	"data_dir.target_not_writable":    {"无法写入“{path}”，请换一个有写入权限的文件夹", []string{"path"}},
	"data_dir.no_space":               {"空间不足：需要 {need_bytes}，目标磁盘只剩 {free_bytes}", []string{"need_bytes", "free_bytes"}},
	"data_dir.copy_failed":            {"复制数据时出错（{path}）。原来的数据没有改动，可以稍后重试", []string{"path"}},
	"data_dir.verify_failed":          {"复制后的数据没有通过校验（{what}）。原来的数据没有改动，可以重试", []string{"what"}},
	"data_dir.source_changed":         {"复制期间数据发生了变化。原来的数据没有改动，请在没有任务运行时重试", nil},
	"data_dir.components_failed":      {"复制后无法更新转写组件的位置。原来的数据没有改动，可以重试", nil},
	"data_dir.location_failed":        {"无法保存新的数据目录位置。原来的数据没有改动，可以重试", nil},
	"data_dir.cancelled":              {"复制已取消，原来的数据没有改动", nil},
	"data_dir.busy":                   {"有 {running} 个任务正在运行、{queued} 个在排队。请等它们结束或取消后再试", []string{"running", "queued"}},
	"data_dir.in_progress":            {"正在复制数据，请等它完成", nil},
	"data_dir.env_override":           {"数据目录由环境变量 LECTUREFLOW_DATA_DIR 指定，不能在这里更改", nil},
	"data_dir.path_too_long":          {"“{data_dir}”的路径太长：转写组件里最深的文件路径会有 {path_len} 个字符，超过 Windows 的上限 {max}。请选一个更短的文件夹（例如 D:\\LectureFlow）", []string{"data_dir", "path_len", "max"}},
	"data_import.not_lectureflow":     {"“{path}”不是 LectureFlow 的数据目录（里面应当有 lectureflow.db）", []string{"path"}},
	"data_import.too_new":             {"这份数据来自更新的 LectureFlow（数据版本 {version}，本程序支持到 {supported}）。请先更新 LectureFlow", []string{"version", "supported"}},
	"data_import.not_fresh":           {"现在的资料库里已经有 {courses} 门课程、{lectures} 节讲座，只能导入到空的资料库", []string{"courses", "lectures"}},
	"data_import.same_dir":            {"这就是现在的数据目录（或与它重叠），请选择开发版的 data 文件夹", nil},
	"data_import.modified":            {"“{path}”里的资料库含有 LectureFlow 自己不会写入的内容（例如用数据库工具加过或改过的表、列、视图、触发器，或格式不对的记录编号、文件名）。为了安全，不能导入", []string{"path"}},
	"data_import.outside_path":        {"要导入的资料库里有 {count} 个文件路径指向资料库自己的文件夹之外，或指向另一条记录的文件（例如 {path}）。为了不让 LectureFlow 读取资料库之外的文件，不能导入", []string{"count", "path"}},
	"old_data_dir.none":               {"没有记录需要清理的旧数据目录", nil},
	"old_data_dir.mismatch":           {"只能删除记录的旧数据目录：{path}", []string{"path"}},
	"old_data_dir.is_current":         {"这是现在正在使用的数据目录，不能删除", nil},
	"old_data_dir.not_lectureflow":    {"“{path}”看起来不是 LectureFlow 的数据目录，为安全起见没有删除", []string{"path"}},
	"old_data_dir.delete_failed":      {"删除旧数据目录时出错（{path}），部分文件可能还在", []string{"path"}},

	// uploads (SPEC §17.4, §17.5)
	"upload.too_large":      {"文件超过 {max_bytes} 上限", []string{"max_bytes"}},
	"upload.no_space":       {"磁盘剩余空间不足：剩余 {free_bytes}，上传后至少要保留 {keep_bytes}。请删除不需要的讲座释放空间后再上传", []string{"free_bytes", "keep_bytes"}},
	"upload.not_multipart":  {"上传格式错误：需要 multipart/form-data", nil},
	"upload.field_too_long": {"字段 {field} 太长", []string{"field"}},
	"upload.multiple_files": {"一次只能上传一个文件", nil},
	"upload.no_filename":    {"缺少文件名", nil},
	"upload.no_file":        {"没有收到文件（表单字段应为 file）", nil},
	"upload.interrupted":    {"上传中断", nil},
	// desktop mode: a local file copied by path (SPEC-v3 V1.4)
	"upload.path_invalid":     {"文件路径无效：{path}", []string{"path"}},
	"upload.path_not_found":   {"找不到文件：{path}（可能已被移动或删除）", []string{"path"}},
	"upload.path_not_file":    {"这不是一个普通文件：{path}", []string{"path"}},
	"upload.path_in_data_dir": {"这个文件已经在 LectureFlow 的数据目录里，不能再次添加：{path}", []string{"path"}},
	"upload.path_unreadable":  {"无法读取文件：{path}（可能没有权限，或正被其他程序占用）", []string{"path"}},
	// no grant of the desktop main process (pathgrant.go, security audit FB1 L-7)
	"upload.path_not_allowed": {"这个文件的选择已失效（可能已经添加过，或选择后隔得太久、重启过应用），请重新选择或拖入这个文件：{path}", []string{"path"}},
	"upload.copy_failed":      {"复制文件失败：{path}", []string{"path"}},

	// courses (v1.1 A8.1, v2 P13.4, P13.6)
	"course.not_found":          {"找不到这门课程（可能已被删除）", nil},
	"course.name_empty":         {"课程名称不能为空", nil},
	"course.name_too_long":      {"课程名称太长（最多 {max} 个字符）", []string{"max"}},
	"course.code_too_long":      {"课程代码太长（最多 {max} 个字符）", []string{"max"}},
	"course.code_has_space":     {"课程代码不能包含空格（它会出现在引用标记中，例如 CSC1003 W02L1@12:34）", nil},
	"course.term_start_invalid": {"开课日期无效：{value}（格式 YYYY-MM-DD）", []string{"value"}},
	"course.skip_week_invalid":  {"不计入教学周的日期无效：{value}（格式 YYYY-MM-DD）", []string{"value"}},
	"calendar.renumber":         {"这会改变 {count} 节讲座的编号，之前导出的引用将与新编号不一致", []string{"count"}},
	"course.numbering_invalid":  {"编号方式“{value}”无效（auto、always 或 never）", []string{"value"}},

	// lecture kinds (.claude/kinds-brief.md section 3)
	"kind.not_found":         {"找不到这个讲座类型（可能已被删除）", nil},
	"kind.builtin":           {"内置类型不能修改或删除", nil},
	"kind.in_use":            {"有 {count} 节讲座正在使用这个类型，先把它们改成其他类型，再删除这个类型或修改它的代码与分组", []string{"count"}},
	"kind.name_empty":        {"类型名称不能为空", nil},
	"kind.name_too_long":     {"类型名称太长（最多 {max} 个字符）", []string{"max"}},
	"kind.code_invalid":      {"代码“{code}”无效：须为 1–3 个大写字母", []string{"code"}},
	"kind.group_invalid":     {"分组“{group}”无效（session 或 material）", []string{"group"}},
	"kind.nothing_to_change": {"没有要修改的内容（name、code、group）", nil},
	"kind.code_taken":        {"代码 {code} 已被其他类型使用", []string{"code"}},
	"kind.name_taken":        {"已经有名为“{name}”的类型", []string{"name"}},
	"kind.unknown":           {"没有“{kind}”这种讲座类型", []string{"kind"}},

	// file-name rules: why a rule or a file name failed (calendar.Problem,
	// sent as error_info / problems_info of the preview; P13.3, P14.3)
	calendar.CodeRegexInvalid:       {"正则表达式无法编译（须为 Go RE2 语法，不支持后行断言和反向引用）：{error}", []string{"error"}},
	calendar.CodeRegexGroupsMissing: {"正则表达式缺少命名分组 {groups}", []string{"groups"}},
	calendar.CodeNoMatch:            {"文件名与规则不匹配", nil},
	calendar.CodeMonthUnknown:       {"无法识别月份“{value}”", []string{"value"}},
	calendar.CodeDayUnknown:         {"无法识别日期“{value}”", []string{"value"}},
	calendar.CodeYearUnknown:        {"无法识别年份“{value}”", []string{"value"}},
	calendar.CodeTimeUnknown:        {"无法识别时间“{time}”", []string{"time"}},
	calendar.CodeTimeInvalid:        {"时间不合法：{time}", []string{"time"}},
	calendar.CodeDateInvalid:        {"日期不合法：{date}", []string{"date"}},
	calendar.CodeAfterUpload:        {"解析出的日期 {date} 晚于上传时间 {uploaded}", []string{"date", "uploaded"}},
	calendar.CodeBeforeTerm:         {"解析出的日期 {date} 早于开课日期 {term_start} 超过 {days} 天", []string{"date", "term_start", "days"}},
	calendar.CodeNoSamples:          {"没有可用于校验的文件名", nil},
	calendar.CodeLowMatch:           {"只匹配了 {matched} / {total} 个文件名，少于 {percent}%", []string{"matched", "total", "percent"}},
	calendar.CodeFileInvalid:        {"{filename}：{reason}", []string{"filename", "reason"}},

	// translation check of a sentence (Sentence.qa_error_info, SPEC §7.4;
	// qa.Failure, the stored reason is English); several failures: the
	// first one's code, the others in params.extra
	qa.CodeEmpty:          {"译文为空", nil},
	qa.CodeTermMissing:    {"术语“{term}”必须译为“{zh}”", []string{"term", "zh", "extra"}},
	qa.CodeMostlyEnglish:  {"译文大部分是英文（拉丁字母占 {percent}%）", []string{"percent", "extra"}},
	qa.CodeLengthRatio:    {"长度比为每个英文词 {ratio} 个字，不在 [{min}, {max}] 范围内", []string{"ratio", "min", "max", "extra"}},
	qa.CodeNumberMissing:  {"原句中的数字 {number} 没有出现在译文中", []string{"number", "extra"}},
	qa.CodeNumbersMissing: {"原句中的数字 {numbers} 没有出现在译文中", []string{"numbers", "extra"}},

	// lectures (SPEC §8, v1.1 A3, A7, v2 P13)
	"lecture.not_found":           {"找不到这节讲座（可能已被删除）", nil},
	"lecture.title_empty":         {"标题不能为空", nil},
	"lecture.seq_readonly":        {"讲座序号由录制时间自动计算，不能直接修改", nil},
	"lecture.recorded_at_invalid": {"录制时间无效：{value}（格式 YYYY-MM-DD 或 YYYY-MM-DDTHH:MM）", []string{"value"}},
	"lecture.not_draft":           {"只有草稿可以替换媒体文件；已启动的讲座请删除后重新上传", nil},
	"lecture.running":             {"「{title}」有阶段正在运行，不能直接删除；可以先取消，或确认“先取消再删除”", []string{"title"}},
	"lecture.delete_timeout":      {"讲座的任务没有在 15 秒内停止，未删除，请稍后再试", nil},
	"lecture.draft":               {"这节讲座还是草稿，请先启动", nil},
	"lecture.already_launched":    {"这节讲座已经启动", nil},
	"lecture.media_deleted":       {"原始媒体文件已按设置删除（不保留原始媒体），无法重跑预处理", nil},
	"lecture.batch_empty":         {"没有选择讲座（ids 为空）", nil},
	"lecture.batch_too_many":      {"一次最多处理 {max} 节讲座", []string{"max"}},
	"audio.not_ready":             {"音频还没有准备好（预处理完成后才能播放）", nil},
	"audio.bad_range":             {"start_ms / end_ms 无效", nil},
	"audio.past_end":              {"start_ms 超出了音频的长度", nil},
	"audio.ffmpeg_failed":         {"ffmpeg 截取音频失败", nil},
	"export.bad_name":             {"文件名无效", nil},
	"export.not_found":            {"导出文件不存在（可能正在重新导出），请刷新后再试", nil},
	"review.busy":                 {"处理中，稍后再审（修复或翻译正在运行）", nil},
	"patch.not_found":             {"找不到这条修改（可能已被重新生成），请刷新", nil},
	"patch.changed":               {"这条修改的审阅状态已经变化，请刷新后再试", nil},
	"patch.edit_empty":            {"编辑后的文本不能为空", nil},
	"patch.unknown_action":        {"未知的审阅操作，只支持 accept、reject、edit、reset", nil},
	"sentence.not_found":          {"找不到这一句（句子可能已经重新生成），请刷新", nil},
	"sentence.bad_idx":            {"句子编号无效", nil},
	"sentence.text_empty":         {"原文不能为空", nil},
	"sentence.unknown_op":         {"未知的句子操作，只支持 edit、reset、retranslate", nil},

	// player (v2 P6.4, P7, P8, appendix D6) and titles (P13.5)
	"media.generating":           {"正在生成播放文件，完成前可以先用音频模式播放", nil},
	"media.not_ready":            {"预处理完成后才能播放", nil},
	"media.audio_only":           {"这节讲座只有音频，没有视频", nil},
	"media.no_space":             {"磁盘空间不足，暂未生成播放文件", nil},
	"media.corrupt":              {"文件不完整或已损坏", nil},
	"media.unsupported":          {"文件格式不支持", nil},
	"media.no_audio":             {"文件中没有音轨", nil},
	"media.original_missing":     {"原始媒体文件已不在，无法生成播放文件", nil},
	"media.no_h264_encoder":      {"没有可用的 H.264 编码器，视频无法转码，先以音频播放", []string{"tried"}}, // SPEC-v3 V7
	"media.encoder_failed":       {"视频编码器在转码时出错（不是文件损坏），可以重试", []string{"tried"}},      // qsv-retry: every encoder failed itself
	"media.format_rejected":      {"媒体文件的实际格式（{format}）不是支持的音视频格式，可能会读取其他文件。为安全起见不处理这个文件，请先转换为 MP4、MKV、MP3 等常见格式后重新上传", []string{"format"}},
	"media.failed":               {"处理这个文件时出错", nil},
	"chapters.not_configured":    {"还没有为“章节”角色选择模型，请先在设置中配置", nil},
	"chapters.not_found":         {"还没有生成章节", nil}, // unused since GET .../chapters answers 200 (power-brief ruling 21); kept registered (lead ruling 9.4)
	"chapters.summary_not_ready": {"摘要完成后才能生成章节", nil},
	"resume.bad_position":        {"播放位置无效", nil},
	"title.user_set":             {"标题是你设定的，确认后才会覆盖", nil},
	"title.not_configured":       {"还没有为“标题与简介”角色选择模型，请先在设置中配置", nil},
	"title.summary_not_ready":    {"摘要完成后才能生成标题与简介", nil},
	// codes of failed auxiliary tasks (job.error, organizer-p2), translated
	// by the front end like the HTTP ones
	"title.no_summary":             {"摘要完成后才能生成标题与简介", nil},
	"title.invalid_output":         {"模型返回的标题与简介格式不正确，可以重试", nil},
	"chapters.no_summary":          {"摘要完成后才能生成章节", nil},
	"chapters.too_short":           {"讲座太短，无法分出章节", nil},
	"chapters.invalid_output":      {"模型返回的章节不符合要求（重试一次后仍然失败），可以重新生成", nil},
	"filename_rule.invalid_output": {"模型没有返回可用的文件名规则，可以重试", nil},
	"filename_rule.rejected":       {"识别出的规则只匹配了 {matched} / {total} 个文件名，没有采用", []string{"matched", "total"}},

	// file-name rules (P13.3)
	"filename_rule.not_configured": {"还没有为“标题与简介”角色选择模型，无法识别文件名格式，请先在设置中配置", nil},
	"filename_rule.no_samples":     {"这门课程还没有讲座，无法识别文件名格式", nil},
	"filename_rule.regex_empty":    {"请填写正则表达式", nil},
	"filename_rule.invalid":        {"正则表达式无效", nil},
	"filename_rule.no_proposal":    {"没有待确认的文件名格式", nil},
	"filename_rule.no_undo":        {"没有可以撤销的自动设置", nil},

	// terms and the glossary janitor (SPEC §8, v2 G1, G3)
	"term.not_found":            {"找不到这个术语（可能已被删除）", nil},
	"term.en_empty":             {"英文术语不能为空", nil},
	"term.duplicate":            {"术语“{en}”已经在术语表中", []string{"en"}},
	"term.lock_only_unlock":     {"只能解锁（locked: false）；修改译法、级别或启用状态时会自动锁定", nil},
	"term.unlock_with_edit":     {"解锁不能和其他修改一起提交", nil},
	"term.bad_tier":             {"级别只能是 enforce（强制）或 reference（参考）", nil},
	"glossary.bad_run":          {"run 只能是 last，且不能与 version 同时使用", nil},
	"glossary.bad_version":      {"version 必须是术语表版本号（非负整数）", nil},
	"glossary.no_runs":          {"这门课程还没有整理过术语表", nil},
	"glossary.report_not_found": {"找不到术语表 v{version} 的整理报告", []string{"version"}},
	"glossary.no_reports":       {"这门课程还没有改动过术语表的整理报告", nil},
	"glossary.undo_missing":     {"缺少要撤销的改动（version 与 term_id）", nil},
	"glossary.already_undone":   {"这一项已经撤销过了", nil},
	"glossary.undo_not_found":   {"找不到要撤销的改动（报告或术语可能已被删除），请刷新", nil},

	// materials (SPEC §9.3)
	"material.not_found":                 {"找不到这份课件（可能已被删除）", nil},
	"material.pdf_only":                  {"课件只支持 PDF 文件（收到：{filename}）", []string{"filename"}},
	"material.lecture_id_missing":        {"缺少 lecture_id（整门课通用时传 null）", nil},
	"material.lecture_id_type":           {"lecture_id 必须是字符串或 null", nil},
	"material.lecture_not_in_course":     {"要绑定的讲座不属于这门课程", nil},
	"material.bind_missing":              {"缺少适用范围：需要 bind，或 v1 的 lecture_id（整门课通用时传 null）", nil},
	"material.bind_invalid":              {"课件的适用范围无效（scope 须为 week、lecture 或 course；lecture 须带 lecture_id）", nil},
	"material.bind_week_invalid":         {"教学周须为 1 到 60 之间的整数", nil},
	"material.bind_lecture_other_course": {"要绑定的讲座不属于这门课程", nil},
	"material.busy":                      {"这份课件正在处理，不能删除；可以先取消，或等它结束", nil},

	// syllabus (v1.1 A8)
	"syllabus.busy":          {"正在从当前大纲提取术语，请等它结束后再替换大纲", nil},
	"syllabus.bad_type":      {"大纲只支持 .txt、.md、.pdf 文件（收到：{filename}）", []string{"filename"}},
	"syllabus.pdf_failed":    {"无法从 PDF 中提取文字", nil},
	"syllabus.no_text":       {"文件中没有可提取的文字（扫描版 PDF 没有文字层），请改传 .txt/.md 或直接粘贴文本", nil},
	"syllabus.bad_body":      {"请求格式错误：需要 multipart 文件，或带 text 字段的 JSON", nil},
	"syllabus.empty":         {"大纲内容不能为空", nil},
	"syllabus.no_raw":        {"还没有大纲原文：请先上传文件或粘贴文本", nil},
	"syllabus.format_failed": {"大纲整理失败", nil},
	"syllabus.format_empty":  {"模型没有返回任何内容，请点击“重新生成”", nil},

	// settings (v1.1 A5, v2 P12, P14.7). Coded settings errors (codes-p2,
	// testdata/api/error_codes_backend.json) keep their own codes; an error
	// of the settings package that is still worded only in Chinese travels
	// in params.text under this fallback code.
	"settings.message":            {"{text}", []string{"text"}},
	"provider.not_found":          {"找不到这个提供商（可能已被删除），请刷新", nil},
	"system.paused_missing":       {"缺少 paused", nil},
	tariff.CodeInvalidJSON:        {"峰谷时段配置不是有效的 JSON", nil},
	tariff.CodeInvalidTimezone:    {"时区无效：{timezone}", []string{"timezone"}},
	tariff.CodeInvalidWindow:      {"第 {index} 个高峰时段无效（{from}–{to}）", []string{"index", "from", "to", "days"}},
	tariff.CodeInvalidHolidays:    {"节假日设置只能为空或 CN（收到：{value}）", []string{"value"}},
	tariff.CodeInvalidMultiplier:  {"空闲时段倍率必须大于 0 且不超过 1（收到：{value}）", []string{"value"}},
	tariff.CodeInvalidPrice:       {"模型 {model} 的价格无效", []string{"model"}},
	tariff.CodeInvalidHolidayData: {"节假日数据无效（{year} {date}）", []string{"year", "date"}},

	// where the built-in holiday dates come from (HolidaysView.source_info)
	"holidays.source_cn": {"国务院办公厅发布的节假日安排（已收录 {years} 年），只含放假日期（包括放假期间的周末），不含调休上班日；尚未公布的年份没有收录", []string{"years"}},

	// diagnostics (SPEC §17.1): SystemCheck.detail_info and hint_info, shown
	// like the error texts; check.checking marks a check still running
	"check.checking":               {"正在检查…", nil},
	"check.not_run":                {"尚未运行（点击“重新检查”运行）", nil},
	"check.crashed":                {"检查时出错：{error}", []string{"error"}},
	"check.dir_writable":           {"{path} 可写", []string{"path"}},
	"check.dir_not_writable":       {"{path} 不可写：{error}", []string{"path", "error"}},
	"check.run_failed":             {"无法运行 {program}：{error}", []string{"program", "error"}},
	"check.python_version_unknown": {"无法识别 Python 版本：{version}", []string{"version"}},
	"check.import_ok":              {"可以导入 {module}", []string{"module"}},
	"check.import_failed":          {"无法导入 {module}：{error}", []string{"module", "error"}},
	"check.scheduler_off":          {"调度器未启动", nil},
	"check.gpu_fake":               {"测试模式（asr.fake_result），不使用 GPU", nil},
	"check.gpu_unreadable":         {"无法读取 GPU 显存：{error}", []string{"error"}},
	"check.gpu_vram":               {"总显存 {total} MB，剩余 {free} MB", []string{"total", "free"}},
	"check.cuda_fake":              {"测试模式（asr.fake_result），跳过推理自检", nil},
	"check.selftest_failed":        {"推理自检失败：{error}", []string{"error"}},
	"check.selftest_ok":            {"自检通过", nil},
	"check.provider_deleted":       {"提供商已删除", nil},
	"check.key_set":                {"已配置（{key}）", []string{"key"}},
	"check.key_from_config":        {"已配置（{key}，来自config.toml）", []string{"key"}},
	"check.key_from_env":           {"已配置（{key}，来自环境变量 {env}）", []string{"key", "env"}},
	"check.key_local_service":      {"本机服务，不需要 API key", nil},
	"check.key_local_model":        {"本地模型，不需要 API key", nil},
	"check.key_missing":            {"没有设置 API key", nil},
	"check.key_env_unset":          {"环境变量 {env} 未设置", []string{"env"}},
	"check.key_not_configured":     {"没有配置 API key", nil},
	"check.api_skipped_deleted":    {"未检查：提供商已删除", nil},
	"check.api_skipped_no_key":     {"未检查：缺少 API key", nil},
	"check.api_skipped":            {"未检查", nil},
	"check.api_failed":             {"请求失败：{error}", []string{"error"}},
	"check.api_ok":                 {"连通（{ms} ms）", []string{"ms"}},
	"check.disk_unreadable":        {"无法读取剩余空间：{error}", []string{"error"}},
	"check.disk_free":              {"剩余 {free_mb}", []string{"free_mb"}},
	"check.hint_data_dir":          {"检查 config.toml 中的 data_dir 和该目录的权限", nil},
	"check.hint_ffmpeg":            {"安装 ffmpeg 并加入 PATH，或在 config.toml 中设置 ffmpeg 的完整路径", nil},
	"check.hint_python":            {"按 README 在项目目录创建 .venv，或在 config.toml 中设置 python 的路径", nil},
	"check.hint_python_version":    {"需要 Python 3.10 或更新版本", nil},
	"check.hint_pip":               {"在项目的 .venv 中执行 pip install -r worker/requirements.txt", nil},
	"check.hint_nvidia":            {"确认已安装 NVIDIA 驱动，nvidia-smi 可以运行（也可在 config.toml 中设置 nvidia_smi 的路径）", nil},
	"check.hint_cpu":               {"转写设备为 CPU：CPU 转写速度很慢，一节课可能需要数小时（可在设置页的“转写”中修改）", nil},
	"check.hint_selftest":          {"确认 faster-whisper 与 CUDA 库已安装（worker/requirements.txt）；首次运行需要联网下载 tiny 模型", nil},
	"check.hint_key_settings":      {"在设置页的“提供商”中编辑「{provider}」并填写 API key", []string{"provider"}},
	codeHintKeyPlainHTTP:           {"这个提供商的地址用的是 http：API key 和讲座内容会不加密地经过网络。服务支持 https 的话，建议在设置页的“提供商”中改用 https 地址", nil},
	"check.hint_key_env":           {"设置环境变量 {env} 后重启服务", []string{"env"}},
	"check.hint_key_config":        {"在 config.toml 的 [llm.providers.{provider}] 中设置 api_key_env", []string{"provider"}},
	"check.hint_api":               {"离线时转写仍可运行；联网后点击“重新检查”。若持续失败请检查 base_url、API key 和模型名", nil},
	"check.hint_disk_error":        {"剩余空间少于 2 GB，新的上传会被拒绝。请删除不需要的讲座或清理磁盘", nil},
	"check.hint_disk_warn":         {"剩余空间少于 5 GB，建议删除不需要的讲座", nil},

	// a role's model is not in its provider's probed model list (role_models.go)
	"check.role_model_unlisted":      {"{roles}使用的 {model} 不在 {provider} 的模型列表里，可能已停用", []string{"roles", "model", "provider"}},
	"check.hint_role_model_unlisted": {"在设置页的“角色分配”中换成 {provider} 模型列表里的模型；列表可能过期时，先在“提供商”中重新探测模型", []string{"provider"}},

	// OpenVINO checks (SPEC-v2 I10, phase 3b freeze; at most warn, lead
	// ruling 13). The import check reuses check.import_ok / import_failed
	// with module openvino.
	"check.openvino_venv_ok":      {"OpenVINO 环境：{path}", []string{"path"}},
	"check.openvino_venv_missing": {"没有找到 OpenVINO 环境（{path}）。Intel 核显与 NPU 不可用，不影响 CUDA 转写", []string{"path"}},
	"check.openvino_devices":      {"识别到的设备：{devices}", []string{"devices"}},
	"check.openvino_no_devices":   {"OpenVINO 没有识别到可用的 Intel 设备", nil},
	"check.npu_driver_ok":         {"NPU 驱动可用（{version}）", []string{"version"}},
	"check.npu_missing":           {"没有检测到 NPU", nil},
	"check.npu_driver_failed":     {"NPU 驱动不可用：{error}", []string{"error"}},
	"check.hint_openvino_venv":    {"需要 Intel 核显或 NPU 转写时，按 worker/requirements-openvino.txt 在项目目录建立 .venv-ov，然后点击“重新检查”", nil},
	"check.hint_npu_driver":       {"NPU 驱动通过 Windows 更新或电脑厂商的驱动程序安装；本程序不会安装或升级驱动", nil},
}

// httpError is an error with an HTTP status and its coded body.
type httpError struct {
	status int
	info   model.ErrorInfo
}

func (e *httpError) Error() string {
	if e.info.Detail != "" {
		return e.info.Code + ": " + e.info.Detail
	}
	return e.info.Code
}

// apiErr builds an httpError (params may be nil).
func apiErr(status int, code string, params map[string]any) error {
	return &httpError{status: status, info: model.ErrorInfo{Code: code, Params: params}}
}

// apiErrDetail is apiErr with the original error text for "查看详情".
func apiErrDetail(status int, code string, params map[string]any, detail string) error {
	return &httpError{status: status, info: model.ErrorInfo{Code: code, Params: params, Detail: detail}}
}

// writeError writes an error body.
func writeError(w http.ResponseWriter, status int, code string, params map[string]any) {
	writeJSON(w, status, ErrorBody{Error: model.ErrorInfo{Code: code, Params: params}})
}

// writeErrorInfo writes an error body with a prepared ErrorInfo.
func writeErrorInfo(w http.ResponseWriter, status int, info model.ErrorInfo) {
	writeJSON(w, status, ErrorBody{Error: info})
}

// notFoundAs gives db.ErrNotFound a specific code (404).
func notFoundAs(err error, code string) error {
	if errors.Is(err, db.ErrNotFound) {
		return apiErr(http.StatusNotFound, code, nil)
	}
	return err
}

// classify maps an error to a status code and its ErrorInfo. busyCode, when
// non-empty, replaces the generic code of state.ErrBusy (busyParams go with
// it). Status 0 = the client went away (nothing to send); 500 = unexpected
// (the caller logs it).
func classify(ctx context.Context, err error, busyCode string, busyParams map[string]any) (int, model.ErrorInfo) {
	var he *httpError
	var mbe *http.MaxBytesError
	var nm *state.NeedsModelChoiceError
	var te *tariff.Error
	var ce *model.CodedError
	info := func(code string, params map[string]any) model.ErrorInfo {
		return model.ErrorInfo{Code: code, Params: params}
	}
	detailed := func(code string, params map[string]any) model.ErrorInfo {
		return model.ErrorInfo{Code: code, Params: params, Detail: err.Error()}
	}
	text := func() map[string]any { return map[string]any{"text": err.Error()} }
	switch {
	case errors.As(err, &he):
		return he.status, he.info
	case errors.As(err, &mbe):
		return http.StatusRequestEntityTooLarge, info("upload.too_large", map[string]any{"max_bytes": mbe.Limit})
	case errors.As(err, &nm):
		return http.StatusConflict, info(model.ErrNeedsModelChoice, nil)
	// v2 P12: a tariff or holiday-data validation error keeps its own code.
	case errors.As(err, &te) && errors.Is(err, settings.ErrInvalid):
		return http.StatusBadRequest, model.ErrorInfo{Code: te.Code, Params: te.Params, Detail: err.Error()}
	// A coded error (settings validation, a stage function the API calls,
	// e.g. the calendar service rejecting a rule): its own code and params,
	// the wrapped error as detail; the status from the sentinel it also
	// matches (codedStatus).
	case errors.As(err, &ce):
		info := ce.Info
		if info.Detail == "" && ce.Err != nil {
			info.Detail = ce.Err.Error()
		}
		return codedStatus(err), info
	// Settings errors worded only in Chinese (Error() is the message).
	case errors.Is(err, settings.ErrInvalid):
		return http.StatusBadRequest, info("settings.message", text())
	case errors.Is(err, settings.ErrRoleNotConfigured), errors.Is(err, settings.ErrProviderInUse):
		return http.StatusConflict, info("settings.message", text())
	case errors.Is(err, db.ErrDuplicate):
		if err.Error() == db.ErrDuplicate.Error() {
			return http.StatusConflict, info("common.duplicate", nil)
		}
		return http.StatusConflict, detailed("common.duplicate", nil)
	case errors.Is(err, db.ErrNotFound):
		return http.StatusNotFound, info("common.not_found", nil)
	case errors.Is(err, state.ErrMediaMissing):
		return http.StatusConflict, info("lecture.media_deleted", nil)
	case errors.Is(err, state.ErrDraft):
		return http.StatusConflict, info("lecture.draft", nil)
	case errors.Is(err, state.ErrAlreadyLaunched):
		return http.StatusConflict, info("lecture.already_launched", nil)
	case errors.Is(err, state.ErrSnapshotUnusable):
		return http.StatusConflict, detailed("model_choice.snapshot_unusable", nil)
	case errors.Is(err, sched.ErrDeleteTimeout):
		return http.StatusConflict, info("lecture.delete_timeout", nil)
	case errors.Is(err, state.ErrBusy):
		if busyCode == "" {
			return http.StatusConflict, detailed("stage.busy", nil)
		}
		return http.StatusConflict, info(busyCode, busyParams)
	case errors.Is(err, glossary.ErrAlreadyUndone):
		return http.StatusConflict, info("glossary.already_undone", nil)
	case errors.Is(err, state.ErrConflict), errors.Is(err, state.ErrIllegalTransition):
		return http.StatusConflict, detailed("state.conflict", nil)
	case errors.Is(err, state.ErrBadInput):
		return http.StatusBadRequest, detailed("request.invalid", nil)
	case errors.Is(err, sched.ErrUnknownAction):
		return http.StatusBadRequest, detailed("stage.unknown_action", nil)
	case ctx.Err() != nil:
		return 0, model.ErrorInfo{}
	}
	return http.StatusInternalServerError, detailed("server.internal", nil)
}

// codedStatus is the HTTP status of a *model.CodedError, decided by the
// sentinel it wraps as well: invalid input 400, conflicts 409, missing
// objects 404; anything else 400.
func codedStatus(err error) int {
	switch {
	case errors.Is(err, db.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, settings.ErrRoleNotConfigured), errors.Is(err, settings.ErrProviderInUse),
		errors.Is(err, db.ErrDuplicate), errors.Is(err, state.ErrConflict):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// failBusy is fail with a specific code for state.ErrBusy.
func (s *Server) failBusy(w http.ResponseWriter, r *http.Request, err error, busyCode string, busyParams map[string]any) {
	var nm *state.NeedsModelChoiceError
	if errors.As(err, &nm) {
		writeJSON(w, http.StatusConflict, s.needsChoiceView(r.Context(), nm, false))
		return
	}
	status, info := classify(r.Context(), err, busyCode, busyParams)
	switch status {
	case 0:
		// client went away; nothing useful to send
	case http.StatusInternalServerError:
		route, path := logRoute(r)
		slog.Error("api", "method", r.Method, "route", route, "path", path, "err", err)
		writeErrorInfo(w, status, info)
	default:
		writeErrorInfo(w, status, info)
	}
	var he *httpError
	if err != nil && !errors.As(err, &he) {
		route, path := logRoute(r)
		slog.Debug("api error", "method", r.Method, "route", route, "path", path, "err", err)
	}
}

// logRoute is what an error's log line names of the request, as the
// slow-request log does (SEC32 信息-5): the route pattern and its path with
// a lecture's or course's id filled in (slowLogPath), never the request's
// own path (an export's file name is the lecture's title).
func logRoute(r *http.Request) (route, path string) {
	route = r.Pattern
	if route == "" {
		route = "(unmatched)"
	}
	return route, slowLogPath(r.Pattern, r.PathValue("id"))
}

// fail writes the error response of err. A *state.NeedsModelChoiceError
// becomes the 409 body of A4.3 ({"error":{"code":"needs_model_choice"},"diff":[...]}).
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.failBusy(w, r, err, "", nil)
}

// batchError is the per-id error of a batch result: the coded error and
// its Chinese text (BatchResult.Error stays Chinese until the i18n
// extraction; the front end prefers error_info).
func (s *Server) batchError(ctx context.Context, err error, busyCode string) (string, *model.ErrorInfo) {
	status, info := classify(ctx, err, busyCode, nil)
	if status == http.StatusInternalServerError {
		slog.Error("api batch item", "err", err)
	}
	if info.Code == "" {
		info = model.ErrorInfo{Code: "server.internal", Detail: err.Error()}
	}
	return zhText(info), &info
}

// zhText renders an ErrorInfo with the catalog's Chinese text (transitional:
// only for strings the frozen types still carry, such as BatchResult.Error).
func zhText(info model.ErrorInfo) string {
	def, ok := errorCatalog[info.Code]
	if !ok {
		if info.Detail != "" {
			return info.Detail
		}
		return info.Code
	}
	out := def.zh
	keys := make([]string, 0, len(info.Params))
	for k := range info.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = strings.ReplaceAll(out, "{"+k+"}", fmtParam(info.Params[k]))
	}
	return out
}

// fmtParam renders one param value for zhText.
func fmtParam(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
