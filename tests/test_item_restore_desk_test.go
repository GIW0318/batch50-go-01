package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-game-a-12"
)

func deskLimits() impl.Limits {
	return impl.Limits{
		RetentionDays:      30,
		MaxReceiptsPerCase: 5,
		MaxCasesPerPeriod:  4,
		MaxValuePerPeriod:  5000,
	}
}

func disposedAt(receiptID, instanceID, itemID string, quantity, value int) impl.DisposedItem {
	return impl.DisposedItem{
		ReceiptID:  receiptID,
		InstanceID: instanceID,
		ItemID:     itemID,
		Name:       itemID + "-name",
		Quantity:   quantity,
		Value:      value,
		Channel:    impl.ChannelDeleted,
	}
}

func deskBaseTime() time.Time {
	return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
}

func TestRecordDisposalWritesReceiptsWithRetention(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	at := deskBaseTime()
	items := []impl.DisposedItem{
		disposedAt("rcpt-sword", "inst-sword", "item-sword", 1, 800),
		disposedAt("rcpt-potion", "inst-potion", "item-potion", 5, 100),
	}
	if err := d.RecordDisposal("char-1", items, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	sword, ok := d.Receipt("rcpt-sword")
	if !ok {
		t.Fatalf("receipt rcpt-sword must be readable after the disposal")
	}
	if sword.InstanceID != "inst-sword" || sword.Quantity != 1 || sword.Value != 800 {
		t.Fatalf("receipt content = %+v, want the disposed sword", sword)
	}
	if sword.Channel != impl.ChannelDeleted {
		t.Fatalf("receipt channel = %q, want %q", sword.Channel, impl.ChannelDeleted)
	}
	if sword.Used {
		t.Fatalf("a fresh receipt must not be marked used")
	}
	if !sword.DisposedAt.Equal(at) {
		t.Fatalf("receipt disposal time = %v, want %v", sword.DisposedAt, at)
	}
	if want := at.AddDate(0, 0, 30); !sword.ExpiresAt.Equal(want) {
		t.Fatalf("receipt expiry = %v, want %v from a 30 day retention", sword.ExpiresAt, want)
	}
	if _, ok := d.Receipt("rcpt-404"); ok {
		t.Fatalf("an unknown receipt must not be readable")
	}
}

func TestRecordDisposalRejectsBlankCharacter(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	items := []impl.DisposedItem{disposedAt("rcpt-1", "inst-1", "item-1", 1, 10)}
	if err := d.RecordDisposal("  ", items, deskBaseTime()); !errors.Is(err, impl.ErrBlankCharacter) {
		t.Fatalf("blank character must be rejected with ErrBlankCharacter, got %v", err)
	}
	if err := d.RecordDisposal("char-1", nil, deskBaseTime()); !errors.Is(err, impl.ErrNoItems) {
		t.Fatalf("an empty disposal must be rejected with ErrNoItems, got %v", err)
	}
	if _, ok := d.Receipt("rcpt-1"); ok {
		t.Fatalf("rejected disposals must not write receipts")
	}
}

func TestRecordDisposalRejectsBadItems(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	zeroQty := []impl.DisposedItem{disposedAt("rcpt-1", "inst-1", "item-1", 0, 10)}
	if err := d.RecordDisposal("char-1", zeroQty, deskBaseTime()); !errors.Is(err, impl.ErrBadItem) {
		t.Fatalf("zero quantity must be rejected with ErrBadItem, got %v", err)
	}
	noInstance := []impl.DisposedItem{disposedAt("rcpt-2", " ", "item-2", 1, 10)}
	if err := d.RecordDisposal("char-1", noInstance, deskBaseTime()); !errors.Is(err, impl.ErrBadItem) {
		t.Fatalf("blank instance id must be rejected with ErrBadItem, got %v", err)
	}
	if err := d.RecordDisposal("char-1", []impl.DisposedItem{disposedAt("rcpt-3", "inst-3", "item-3", 1, 10)}, deskBaseTime()); err != nil {
		t.Fatalf("record the first receipt: %v", err)
	}
	if err := d.RecordDisposal("char-1", []impl.DisposedItem{disposedAt("rcpt-3", "inst-4", "item-4", 1, 10)}, deskBaseTime()); !errors.Is(err, impl.ErrDuplicateReceipt) {
		t.Fatalf("reused receipt id must be rejected with ErrDuplicateReceipt, got %v", err)
	}
	if _, ok := d.Receipt("rcpt-1"); ok {
		t.Fatalf("rejected items must not write receipts")
	}
}

func TestSubmitRejectsUnknownReceipt(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	if _, err := d.Submit("char-1", nil, deskBaseTime()); !errors.Is(err, impl.ErrNoReceipts) {
		t.Fatalf("an empty request must be rejected with ErrNoReceipts, got %v", err)
	}
	if _, err := d.Submit("char-1", []string{"rcpt-404"}, deskBaseTime()); !errors.Is(err, impl.ErrUnknownReceipt) {
		t.Fatalf("unknown receipt must be rejected with ErrUnknownReceipt, got %v", err)
	}
	if got := len(d.OpenCases("char-1")); got != 0 {
		t.Fatalf("rejected requests must not open cases, got %d", got)
	}
}

func TestSubmitRejectsExpiredReceipt(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	at := deskBaseTime()
	if err := d.RecordDisposal("char-1", []impl.DisposedItem{disposedAt("rcpt-old", "inst-old", "item-old", 1, 50)}, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	late := at.AddDate(0, 0, 31)
	if _, err := d.Submit("char-1", []string{"rcpt-old"}, late); !errors.Is(err, impl.ErrReceiptExpired) {
		t.Fatalf("receipt past the retention window must fail with ErrReceiptExpired, got %v", err)
	}
	if got := len(d.OpenCases("char-1")); got != 0 {
		t.Fatalf("expired receipts must not open a case, got %d", got)
	}
	if _, err := d.Submit("char-1", []string{"rcpt-old"}, at.AddDate(0, 0, 29)); err != nil {
		t.Fatalf("a receipt inside the window must still be usable, got %v", err)
	}
}

func TestSubmitCreatesPendingCase(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	at := deskBaseTime()
	items := []impl.DisposedItem{
		disposedAt("rcpt-a", "inst-a", "item-a", 1, 300),
		disposedAt("rcpt-b", "inst-b", "item-b", 2, 120),
	}
	if err := d.RecordDisposal("char-2", items, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	c, err := d.Submit("char-2", []string{"rcpt-a", "rcpt-b"}, at)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if c.CaseID == "" {
		t.Fatalf("a submitted case must carry an id")
	}
	if c.CharacterID != "char-2" {
		t.Fatalf("case character = %q, want char-2", c.CharacterID)
	}
	if c.State != impl.CasePending {
		t.Fatalf("new case state = %q, want %q", c.State, impl.CasePending)
	}
	if len(c.Receipts) != 2 || c.Receipts[0] != "rcpt-a" || c.Receipts[1] != "rcpt-b" {
		t.Fatalf("case receipts = %v, want the submitted order", c.Receipts)
	}
	if !c.OpenedAt.Equal(at) {
		t.Fatalf("case opened at %v, want %v", c.OpenedAt, at)
	}
	st, err := d.Status(c.CaseID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != impl.CasePending || st.CharacterID != "char-2" {
		t.Fatalf("status = %+v, want the pending case of char-2", st)
	}
	open := d.OpenCases("char-2")
	if len(open) != 1 || open[0] != c.CaseID {
		t.Fatalf("open cases = %v, want exactly %s", open, c.CaseID)
	}
}

func TestSubmitEnforcesCaseQuota(t *testing.T) {
	d := impl.NewItemRestoreDesk(impl.Limits{
		RetentionDays:      30,
		MaxReceiptsPerCase: 5,
		MaxCasesPerPeriod:  1,
		MaxValuePerPeriod:  5000,
	})
	at := deskBaseTime()
	if err := d.RecordDisposal("char-3", []impl.DisposedItem{
		disposedAt("rcpt-1", "inst-1", "item-1", 1, 10),
		disposedAt("rcpt-2", "inst-2", "item-2", 1, 10),
	}, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	if _, err := d.Submit("char-3", []string{"rcpt-1"}, at); err != nil {
		t.Fatalf("first case of the period must be accepted, got %v", err)
	}
	if _, err := d.Submit("char-3", []string{"rcpt-2"}, at); !errors.Is(err, impl.ErrCaseQuota) {
		t.Fatalf("second case of the same period must fail with ErrCaseQuota, got %v", err)
	}
	if _, err := d.Submit("char-3", []string{"rcpt-2"}, at.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("the next period must accept the case again, got %v", err)
	}
	if got := len(d.OpenCases("char-3")); got != 2 {
		t.Fatalf("open cases = %d, want 2", got)
	}
}

func TestSubmitEnforcesValueQuota(t *testing.T) {
	d := impl.NewItemRestoreDesk(impl.Limits{
		RetentionDays:      30,
		MaxReceiptsPerCase: 5,
		MaxCasesPerPeriod:  4,
		MaxValuePerPeriod:  1000,
	})
	at := deskBaseTime()
	if err := d.RecordDisposal("char-4", []impl.DisposedItem{
		disposedAt("rcpt-big", "inst-big", "item-big", 1, 900),
		disposedAt("rcpt-small", "inst-small", "item-small", 1, 200),
	}, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	if _, err := d.Submit("char-4", []string{"rcpt-big"}, at); err != nil {
		t.Fatalf("a case inside the value allowance must be accepted, got %v", err)
	}
	if _, err := d.Submit("char-4", []string{"rcpt-small"}, at); !errors.Is(err, impl.ErrValueQuota) {
		t.Fatalf("a case pushing the period past the value cap must fail with ErrValueQuota, got %v", err)
	}
	if got := len(d.OpenCases("char-4")); got != 1 {
		t.Fatalf("open cases = %d, want only the accepted one", got)
	}
}

func TestApproveIssuesMailGrantOnce(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	at := deskBaseTime()
	if err := d.RecordDisposal("char-5", []impl.DisposedItem{
		disposedAt("rcpt-x", "inst-x", "item-x", 1, 500),
		disposedAt("rcpt-y", "inst-y", "item-y", 1, 400),
	}, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	c, err := d.Submit("char-5", []string{"rcpt-x", "rcpt-y"}, at)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	g, err := d.Approve(c.CaseID, "cs-7")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if g.GrantID == "" {
		t.Fatalf("an approved case must produce a grant id")
	}
	if g.CaseID != c.CaseID || g.CharacterID != "char-5" {
		t.Fatalf("grant = %+v, want it bound to case %s of char-5", g, c.CaseID)
	}
	if g.MailID == "" {
		t.Fatalf("the reissue must go through a mail, got an empty mail id")
	}
	if len(g.InstanceIDs) != 2 || g.InstanceIDs[0] != "inst-x" || g.InstanceIDs[1] != "inst-y" {
		t.Fatalf("granted instances = %v, want the disposed instances in case order", g.InstanceIDs)
	}
	if g.Value != 900 {
		t.Fatalf("granted value = %d, want 900", g.Value)
	}
	for _, id := range []string{"rcpt-x", "rcpt-y"} {
		r, ok := d.Receipt(id)
		if !ok {
			t.Fatalf("receipt %s disappeared after the approval", id)
		}
		if !r.Used {
			t.Fatalf("receipt %s must be marked used once the grant is issued", id)
		}
	}
	st, err := d.Status(c.CaseID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != impl.CaseApproved || st.Operator != "cs-7" {
		t.Fatalf("status after approve = %+v, want approved by cs-7", st)
	}
	if grants := d.Grants("char-5"); len(grants) != 1 || grants[0].GrantID != g.GrantID {
		t.Fatalf("grants = %+v, want exactly the issued grant", grants)
	}
	if got := len(d.OpenCases("char-5")); got != 0 {
		t.Fatalf("an approved case must leave the open list, got %d", got)
	}
}

func TestApproveRequiresOperator(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	at := deskBaseTime()
	if err := d.RecordDisposal("char-6", []impl.DisposedItem{disposedAt("rcpt-1", "inst-1", "item-1", 1, 10)}, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	c, err := d.Submit("char-6", []string{"rcpt-1"}, at)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := d.Approve(c.CaseID, "   "); !errors.Is(err, impl.ErrOperatorRequired) {
		t.Fatalf("approval without an operator must fail with ErrOperatorRequired, got %v", err)
	}
	st, _ := d.Status(c.CaseID)
	if st.State != impl.CasePending {
		t.Fatalf("a rejected approval must leave the case pending, got %q", st.State)
	}
	if r, _ := d.Receipt("rcpt-1"); r.Used {
		t.Fatalf("a rejected approval must not consume the receipt")
	}
}

func TestCaseLookupRejectsUnknownCase(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	if _, err := d.Status("case-404"); !errors.Is(err, impl.ErrCaseNotFound) {
		t.Fatalf("unknown case status must fail with ErrCaseNotFound, got %v", err)
	}
	if _, err := d.Approve("case-404", "cs-1"); !errors.Is(err, impl.ErrCaseNotFound) {
		t.Fatalf("approving an unknown case must fail with ErrCaseNotFound, got %v", err)
	}
	if err := d.Reject("case-404", "duplicate ticket"); !errors.Is(err, impl.ErrCaseNotFound) {
		t.Fatalf("rejecting an unknown case must fail with ErrCaseNotFound, got %v", err)
	}
}

func TestRejectClosesCaseAndReleasesReceipts(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	at := deskBaseTime()
	if err := d.RecordDisposal("char-7", []impl.DisposedItem{disposedAt("rcpt-1", "inst-1", "item-1", 1, 60)}, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	c, err := d.Submit("char-7", []string{"rcpt-1"}, at)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := d.Reject(c.CaseID, "  "); !errors.Is(err, impl.ErrReasonRequired) {
		t.Fatalf("reject without a reason must fail with ErrReasonRequired, got %v", err)
	}
	if err := d.Reject(c.CaseID, "sold by the player on purpose"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	st, err := d.Status(c.CaseID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != impl.CaseRejected || st.Reason != "sold by the player on purpose" {
		t.Fatalf("status after reject = %+v, want the rejection note", st)
	}
	if _, err := d.Approve(c.CaseID, "cs-2"); !errors.Is(err, impl.ErrCaseClosed) {
		t.Fatalf("approving a rejected case must fail with ErrCaseClosed, got %v", err)
	}
	if r, _ := d.Receipt("rcpt-1"); r.Used {
		t.Fatalf("a rejected case must not consume the receipt")
	}
	again, err := d.Submit("char-7", []string{"rcpt-1"}, at)
	if err != nil {
		t.Fatalf("a released receipt must be usable again, got %v", err)
	}
	if again.CaseID == c.CaseID {
		t.Fatalf("the reopened request must get a new case id, got the old one back")
	}
}

func TestSubmitRejectsUsedReceipt(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	at := deskBaseTime()
	if err := d.RecordDisposal("char-8", []impl.DisposedItem{disposedAt("rcpt-1", "inst-1", "item-1", 1, 70)}, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	c, err := d.Submit("char-8", []string{"rcpt-1"}, at)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := d.Approve(c.CaseID, "cs-3"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := d.Submit("char-8", []string{"rcpt-1"}, at); !errors.Is(err, impl.ErrReceiptUsed) {
		t.Fatalf("a restored receipt must not open a second case, got %v", err)
	}
	if got := len(d.Grants("char-8")); got != 1 {
		t.Fatalf("grants after the rejected resubmission = %d, want 1", got)
	}
}

func TestSubmitRejectsReceiptsOfGoneCharacter(t *testing.T) {
	d := impl.NewItemRestoreDesk(deskLimits())
	at := deskBaseTime()
	if err := d.RecordDisposal("char-9", []impl.DisposedItem{disposedAt("rcpt-1", "inst-1", "item-1", 1, 80)}, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	if err := d.MarkCharacterGone("char-9"); err != nil {
		t.Fatalf("mark character gone: %v", err)
	}
	if _, err := d.Submit("char-9", []string{"rcpt-1"}, at); !errors.Is(err, impl.ErrCharacterGone) {
		t.Fatalf("a deleted character must not be restored, got %v", err)
	}
	if got := len(d.OpenCases("char-9")); got != 0 {
		t.Fatalf("rejected requests must not open cases, got %d", got)
	}
}

func TestSnapshotRestoreKeepsCasesAndReceipts(t *testing.T) {
	limits := deskLimits()
	d := impl.NewItemRestoreDesk(limits)
	at := deskBaseTime()
	if err := d.RecordDisposal("char-10", []impl.DisposedItem{
		disposedAt("rcpt-1", "inst-1", "item-1", 1, 90),
		disposedAt("rcpt-2", "inst-2", "item-2", 1, 110),
	}, at); err != nil {
		t.Fatalf("record disposal: %v", err)
	}
	c, err := d.Submit("char-10", []string{"rcpt-1"}, at)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewItemRestoreDesk(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, err := restored.Status(c.CaseID)
	if err != nil {
		t.Fatalf("status after restore: %v", err)
	}
	if st.State != impl.CasePending {
		t.Fatalf("restored case state = %q, want pending", st.State)
	}
	if r, ok := restored.Receipt("rcpt-2"); !ok || r.Value != 110 {
		t.Fatalf("restored receipt = %+v ok=%v, want the untouched 110 value receipt", r, ok)
	}
	if r, _ := restored.Receipt("rcpt-1"); r.Used {
		t.Fatalf("a pending case must not consume its receipt across a restart")
	}
	g, err := restored.Approve(c.CaseID, "cs-9")
	if err != nil {
		t.Fatalf("approve after restore: %v", err)
	}
	if g.MailID == "" || len(g.InstanceIDs) != 1 || g.InstanceIDs[0] != "inst-1" {
		t.Fatalf("grant after restore = %+v, want the inst-1 reissue", g)
	}
	if err := restored.Restore([]byte("@not-a-snapshot@")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	st, _ = restored.Status(c.CaseID)
	if st.State != impl.CaseApproved {
		t.Fatalf("a rejected restore must not roll the live state back, got %q", st.State)
	}
}
