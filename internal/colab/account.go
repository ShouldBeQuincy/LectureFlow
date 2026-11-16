// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The Colab account (colab-obs-core; .claude/research/colab-telemetry.md
// §2; user decisions of 2026-10-27, NOTES "用户决定（2026-10-27，约 07:00
// 提问、14:30 前回复）" and the user's additions of about 14:50):
//
//   - ccu-info's three old fields stay strict (api.go); the newer ones
//     (eligibleGpus, ineligibleGpus, freeCcuQuotaInfo) are read leniently:
//     missing or unknown never makes the answer CodeAPIChanged.
//   - The level is paid (a paid balance > 0) or free (anything else, an
//     account that could not be read included: Account.ReadOK says so).
//     Colab gives third-party programs no subscription type: none is shown.
//   - Selectable accelerators: T4 always, the others listed in eligibleGpus
//     only with a paid balance > 0. A picked accelerator the account does
//     not allow is kept as the user's choice; this run borrows a T4 instead
//     (Status.EffectiveAccelerator, LockedReason) and the pick comes back
//     by itself once the account allows it again.
//   - Colab's free quota (fair use) is used whenever there is one: a T4 is
//     borrowed with a paid balance of 0 too, the free tier and an unread
//     account borrow a T4 on a best-effort basis, and "还能再处理约 N 节"
//     on a T4 counts the free quota with the paid balance (which of the two
//     Colab charges is Google's business).
//   - Reading: before a dispatch at most once a minute (every level); when
//     the settings page or the resource bar shows the account and the
//     reading is older than five minutes, in the background (TouchAccount,
//     then Options.OnChange); every five minutes from Run while Colab is on
//     and authorised; at once after an import or a sign-in (the API calls
//     RefreshAccountSoon). Status never contacts Colab.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/model"
)

// Account levels.
const (
	LevelPaid = "paid" // a paid compute-unit balance > 0
	LevelFree = "free" // no paid balance, or the account could not be read
)

// Why the accelerator the user picked is not the one borrowed now
// (Status.LockedReason).
const (
	LockAccountUnknown = "account_unknown" // never read, or eligibleGpus missing
	LockNoBalance      = "no_balance"      // no paid balance (the free quota is for T4 only)
	LockIneligible     = "ineligible"      // not in eligibleGpus
)

// accountMaxAge: a viewer (settings page, resource bar) and Run read the
// account again once the reading is this old.
const accountMaxAge = 5 * time.Minute

// Account is Status.Account: what ccu-info says about the user's account.
// Values are null while unknown (never read).
type Account struct {
	// ReadAt: when the reading shown was taken (null: never).
	ReadAt *time.Time `json:"read_at"`
	// ReadOK: the last attempt to read the account succeeded (false: it
	// failed, the reading shown, if any, is older; or none was made yet).
	// The settings page offers "重新查询" (POST /api/colab/account/refresh).
	ReadOK bool `json:"read_ok"`
	// ReadError: why the last attempt failed (colab.* codes).
	ReadError *model.ErrorInfo `json:"read_error,omitempty"`
	Level     string           `json:"level"` // LevelPaid | LevelFree
	// PaidBalanceCU: currentBalance (paid compute units only).
	PaidBalanceCU *float64 `json:"paid_balance_cu"`
	// AccountRateCUPerHour: consumptionRateHourly, the whole account's
	// ("账户当前消耗": the user's own notebooks in the browser included).
	AccountRateCUPerHour *float64 `json:"account_rate_cu_per_hour"`
	AccountRuntimes      *int     `json:"account_runtimes"` // assignmentsCount
	// FreeQuotaCU and FreeRefillAt: freeCcuQuotaInfo (remainingTokens are
	// thousandths of a compute unit); null when Colab did not send them.
	FreeQuotaCU  *float64   `json:"free_quota_cu"`
	FreeRefillAt *time.Time `json:"free_refill_at"`
	// EligibleGPUs / IneligibleGPUs: the accelerators LectureFlow supports
	// (Accelerators) that the account may / may not borrow; never null.
	EligibleGPUs   []string `json:"eligible_gpus"`
	IneligibleGPUs []string `json:"ineligible_gpus"`
	// EligibilityKnown: eligibleGpus was in the reading (false: only T4).
	EligibilityKnown bool `json:"eligibility_known"`
}

// ---- ccu-info's lenient fields ----

// readCCUExtras fills c's lenient fields from the ccu-info answer o;
// unknown accelerator names go to unknown (logged by the caller).
func readCCUExtras(o obj, c *ccuInfo, unknown func(name string)) {
	if raw, ok := o.m["eligibleGpus"]; ok {
		c.EligibleGPUs, c.EligibilityKnown = gpuList(raw, unknown)
	}
	if raw, ok := o.m["ineligibleGpus"]; ok {
		c.IneligibleGPUs, _ = gpuList(raw, unknown)
	}
	if raw, ok := o.m["freeCcuQuotaInfo"]; ok {
		c.FreeQuotaCU, c.FreeRefillAt = freeQuota(raw)
	}
}

// gpuList reads a list of accelerator names: the supported GPUs, in their
// order of appearance; TPUs and NONE are dropped quietly, anything else is
// reported. ok false: not a list (treated as absent).
func gpuList(raw json.RawMessage, unknown func(string)) ([]string, bool) {
	var items []any
	if string(raw) == "null" || json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	out := []string{}
	for _, it := range items {
		s, isStr := it.(string)
		u := strings.ToUpper(strings.TrimSpace(s))
		switch {
		case !isStr:
			unknown("?")
		case slices.Contains(Accelerators, u):
			if !slices.Contains(out, u) {
				out = append(out, u)
			}
		case slices.Contains(knownAccelerators, u):
		default:
			unknown(safeToken(u))
		}
	}
	return out, true
}

// lenientNumber is a JSON number or a numeric string (proto3 JSON writes
// int64 as a string).
func lenientNumber(raw json.RawMessage) (float64, bool) {
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(s), 64); err != nil {
			return 0, false
		}
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func freeQuota(raw json.RawMessage) (*float64, *time.Time) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, nil
	}
	var cu *float64
	if v, ok := m["remainingTokens"]; ok {
		if f, ok := lenientNumber(v); ok && f >= 0 && f < 1e15 {
			x := math.Round(f) / 1000
			cu = &x
		}
	}
	var at *time.Time
	if v, ok := m["nextRefillTimestampSec"]; ok {
		if f, ok := lenientNumber(v); ok && f > 0 && f < 1e11 {
			t := time.Unix(int64(f), 0).UTC()
			at = &t
		}
	}
	return cu, at
}

// ---- level and selection ----

// selection is what the account allows and what the next borrowing asks
// for.
type selection struct {
	level      string
	selectable []string
	// preferred: the user's pick, or the level's default (L4 paid, T4 free)
	preferred string
	effective string
	locked    string // LockedReason when effective != preferred
	// quotaCU: Colab's free quota, -1 unknown (a T4's budget includes it)
	quotaCU float64
}

func (s selection) tier() string {
	if s.level == LevelPaid {
		return TierPaid
	}
	return TierFree
}

func selectionOf(set Settings, c *ccuSnapshot) selection {
	sel := selection{level: LevelFree, selectable: []string{"T4"}, quotaCU: -1}
	if c != nil {
		if c.info.Balance > 0 {
			sel.level = LevelPaid
			if c.info.EligibilityKnown {
				for _, a := range Accelerators {
					if a != "T4" && slices.Contains(c.info.EligibleGPUs, a) {
						sel.selectable = append(sel.selectable, a)
					}
				}
			}
		}
		if c.info.FreeQuotaCU != nil {
			sel.quotaCU = *c.info.FreeQuotaCU
		}
	}
	sel.preferred = DefaultAccelerator(sel.tier())
	if set.AcceleratorChosen && set.Accelerator != "" {
		sel.preferred = set.Accelerator
	}
	sel.effective = sel.preferred
	if !slices.Contains(sel.selectable, sel.preferred) {
		sel.effective = "T4"
		switch {
		case c == nil:
			sel.locked = LockAccountUnknown
		case c.info.Balance <= 0:
			sel.locked = LockNoBalance
		case !c.info.EligibilityKnown:
			sel.locked = LockAccountUnknown
		default:
			sel.locked = LockIneligible
		}
	}
	return sel
}

// selectionLocked is the selection for the current settings and reading.
func (b *Backend) selectionLocked() selection { return selectionOf(b.settings, b.ccu) }

// liveSettingsLocked are the settings in effect: the accelerator the next
// borrowing asks for and the account's tier.
func (b *Backend) liveSettingsLocked() Settings {
	s := b.settings
	sel := b.selectionLocked()
	s.Accelerator, s.Tier = sel.effective, sel.tier()
	return s
}

func (b *Backend) liveSettings() Settings {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.liveSettingsLocked()
}

// UserSettings are the settings as the settings page shows them: the
// accelerator is the user's pick even when the account does not allow it
// now (or, never picked, the account level's default), Tier the account's.
func (b *Backend) UserSettings() Settings {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.settings
	sel := b.selectionLocked()
	s.Accelerator, s.Tier = sel.preferred, sel.tier()
	return s
}

// accountLocked is Status.Account.
func (b *Backend) accountLocked() Account {
	a := Account{Level: LevelFree, EligibleGPUs: []string{}, IneligibleGPUs: []string{}}
	if c := b.ccu; c != nil {
		t := c.at.UTC()
		bal, rate, n := round2(c.info.Balance), round2(c.info.RatePerHour), c.info.Assignments
		a.ReadAt, a.PaidBalanceCU, a.AccountRateCUPerHour, a.AccountRuntimes = &t, &bal, &rate, &n
		if c.info.Balance > 0 {
			a.Level = LevelPaid
		}
		a.FreeQuotaCU, a.FreeRefillAt = c.info.FreeQuotaCU, c.info.FreeRefillAt
		a.EligibleGPUs = append(a.EligibleGPUs, c.info.EligibleGPUs...)
		a.IneligibleGPUs = append(a.IneligibleGPUs, c.info.IneligibleGPUs...)
		a.EligibilityKnown = c.info.EligibilityKnown
	}
	a.ReadOK = b.ccu != nil && b.ccuErr == nil
	if b.ccuErr != nil {
		a.ReadError = infoOf(accountErr(b.secrets.scrubErr(b.ccuErr)))
	}
	return a
}

// accountErr is the account's own error for a failed reading: a credential
// problem keeps its colab.auth_* code (the settings page says what to do),
// anything else is CodeAccountUnavailable {reason} (the codes of a failed
// lecture would say "the lecture went back to the local devices").
func accountErr(err error) error {
	code := Code(err)
	if strings.HasPrefix(code, "colab.auth_") {
		return err
	}
	reason := "other"
	switch code {
	case CodeUnavailable:
		reason = "unavailable"
	case CodeAPIChanged:
		reason = "api_changed"
	}
	return newErr(CodeAccountUnavailable, map[string]any{"reason": reason}, kindLocal, "", err)
}

// ---- reading ----

// refreshCCU reads the account before a dispatch, at most once per
// ccuMaxAge whatever the outcome (within it the last attempt's error is
// returned again). Callers stop the dispatch on blockingRead errors only.
func (b *Backend) refreshCCU(ctx context.Context) error {
	b.mu.Lock()
	if !b.ccuTry.IsZero() && b.now().Sub(b.ccuTry) < ccuMaxAge {
		err := b.ccuErr
		b.mu.Unlock()
		return err
	}
	b.mu.Unlock()
	return b.readAccount(ctx)
}

// blockingRead: a failed account reading that stops a dispatch: the
// credential (waiting_for remote_auth) or an answer that does not look
// like ccu-info any more (CodeAPIChanged, "失败要响亮"). A transient failure
// does not: the dispatch goes on with the last reading, or without one as
// the free tier (a T4, best effort; the user's addition of 2026-10-27).
func blockingRead(err error) bool {
	return err != nil && Code(err) != CodeUnavailable && !errors.Is(err, context.DeadlineExceeded)
}

// readAccount reads ccu-info once and records the outcome; Options.OnChange
// is told when what Status.Account shows changed.
func (b *Backend) readAccount(ctx context.Context) error {
	info, err := b.tun.ccuInfo(ctx)
	if err != nil && ctx.Err() != nil {
		return err // cancelled: not an attempt
	}
	b.mu.Lock()
	before := b.accountLocked()
	b.ccuTry = b.now()
	if err != nil {
		b.ccuErr = err
	} else {
		b.ccu, b.ccuErr = &ccuSnapshot{info: info, at: b.now()}, nil
	}
	after := b.accountLocked()
	b.mu.Unlock()
	if err == nil {
		b.accountReadOK()
	}
	before.ReadAt, after.ReadAt = nil, nil
	if !reflect.DeepEqual(before, after) {
		if err != nil {
			b.log.Info("colab: reading the account failed", "code", Code(err))
		}
		b.changed()
	}
	return err
}

// changed tells Options.OnChange (SSE changed colab).
func (b *Backend) changed() {
	if b.o.OnChange != nil {
		b.o.OnChange()
	}
}

// TouchAccount is called when the account is shown (GET /api/colab, the
// resource bar's samples): with a usable credential, a reading older than
// five minutes (or none) is renewed in the background, one at a time; the
// answer arrives through Options.OnChange. It never blocks.
func (b *Backend) TouchAccount(ctx context.Context) { b.kickAccount(ctx, false) }

// RefreshAccountSoon renews the reading in the background now (after an
// import or a sign-in).
func (b *Backend) RefreshAccountSoon(ctx context.Context) { b.kickAccount(ctx, true) }

func (b *Backend) kickAccount(ctx context.Context, force bool) {
	if _, err := b.auth.remaining(ctx); err != nil {
		return
	}
	b.mu.Lock()
	due := force || b.ccuTry.IsZero() || b.now().Sub(b.ccuTry) >= accountMaxAge
	if !due || b.acctBusy {
		b.mu.Unlock()
		return
	}
	b.acctBusy = true
	b.mu.Unlock()
	go func() {
		defer func() {
			b.mu.Lock()
			b.acctBusy = false
			b.mu.Unlock()
		}()
		rctx, cancel := context.WithTimeout(b.bg, tunnelTimeout+5*time.Second)
		defer cancel()
		_ = b.readAccount(rctx)
	}()
}

// RefreshAccount reads the account now ("重新查询") and returns what Status
// shows; a failure is in Account.ReadOK / ReadError, never an error. A
// missing or expired credential reads nothing.
func (b *Backend) RefreshAccount(ctx context.Context) Account {
	if _, err := b.auth.remaining(ctx); err == nil {
		_ = b.readAccount(ctx)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accountLocked()
}

// accountTick is Run's part: every five minutes while Colab is switched on
// (Options.Enabled) and authorised.
func (b *Backend) accountTick(ctx context.Context) {
	if b.o.Enabled != nil && !b.o.Enabled(ctx) {
		return
	}
	b.kickAccount(ctx, false)
}

// forgetAccount drops the reading (a credential was imported or deleted:
// it may be another Google account's); the next dispatch or viewer reads
// it again.
func (b *Backend) forgetAccount() {
	b.mu.Lock()
	b.ccu, b.ccuTry, b.ccuErr = nil, time.Time{}, nil
	b.mu.Unlock()
}
