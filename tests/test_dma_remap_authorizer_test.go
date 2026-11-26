package task

import (
	"errors"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-13"
)

func dmaLimits() impl.Limits {
	return impl.Limits{
		MaxDevicesPerDomain: 8,
		MaxWindow:           time.Minute,
		MaxWindowTotal:      10 * time.Minute,
		MaxGroups:           8,
		RestoreGrace:        30 * time.Second,
		Now:                 func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func dev(vendor, product, path string) impl.DeviceID {
	return impl.DeviceID{Vendor: vendor, Device: product, Path: path}
}

func funcs(names ...string) []impl.Function {
	out := make([]impl.Function, 0, len(names))
	for _, name := range names {
		out = append(out, impl.Function{Name: name})
	}
	return out
}

func registerGroup(t *testing.T, a *impl.DmaRemapAuthorizer, group string, devices ...impl.DeviceID) {
	t.Helper()
	for i, d := range devices {
		if err := a.RegisterDevice(d, group, funcs("fn-"+d.Path+"-"+string(rune('a'+i)))); err != nil {
			t.Fatalf("register %s must succeed, got %v", d.Path, err)
		}
	}
}

func TestRegisterDeviceRejectsBlankGroup(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	err := a.RegisterDevice(dev("10de", "1eb8", "0000:01:00.0"), "", funcs("gpu"))
	if !errors.Is(err, impl.ErrBlankGroup) {
		t.Fatalf("register with blank group = %v, want ErrBlankGroup", err)
	}
}

func TestRegisterDeviceRequiresFunctions(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	err := a.RegisterDevice(dev("10de", "1eb8", "0000:01:00.0"), "iommu-1", nil)
	if !errors.Is(err, impl.ErrNoFunctions) {
		t.Fatalf("register without functions = %v, want ErrNoFunctions", err)
	}
}

func TestRegisterDeviceRejectsDuplicateDevice(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	d := dev("10de", "1eb8", "0000:01:00.0")
	if err := a.RegisterDevice(d, "iommu-1", funcs("gpu")); err != nil {
		t.Fatalf("first register must succeed, got %v", err)
	}
	if err := a.RegisterDevice(d, "iommu-1", funcs("gpu")); !errors.Is(err, impl.ErrDeviceExists) {
		t.Fatalf("duplicate device = %v, want ErrDeviceExists", err)
	}
}

func TestEnrollBindsWholeGroup(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	first := dev("10de", "1eb8", "0000:01:00.0")
	second := dev("10de", "10f8", "0000:01:00.1")
	registerGroup(t, a, "iommu-1", first, second)
	grant, err := a.Enroll(first, "dom-a")
	if err != nil {
		t.Fatalf("enroll must succeed, got %v", err)
	}
	if grant.Group != "iommu-1" {
		t.Fatalf("grant group = %q, want iommu-1", grant.Group)
	}
	if grant.Domain != "dom-a" {
		t.Fatalf("grant domain = %q, want dom-a", grant.Domain)
	}
	if grant.ID == "" {
		t.Fatalf("grant id is empty")
	}
	if len(grant.Devices) != 2 {
		t.Fatalf("bound devices = %d, want the whole group of 2", len(grant.Devices))
	}
	view, err := a.GroupStatus("iommu-1")
	if err != nil {
		t.Fatalf("group status must be readable, got %v", err)
	}
	if !view.Bound || view.Domain != "dom-a" {
		t.Fatalf("group view = %+v, want bound to dom-a", view)
	}
	if len(view.Devices) != 2 {
		t.Fatalf("group devices = %d, want 2", len(view.Devices))
	}
	for _, dv := range view.Devices {
		if !dv.Bound {
			t.Fatalf("device %s is not marked bound", dv.ID.Path)
		}
	}
	if usage := a.DomainUsage("dom-a"); usage.Devices != 2 {
		t.Fatalf("domain devices = %d, want 2", usage.Devices)
	}
}

func TestEnrollRejectsGroupBoundElsewhere(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	first := dev("10de", "1eb8", "0000:02:00.0")
	second := dev("10de", "10f8", "0000:02:00.1")
	registerGroup(t, a, "iommu-2", first, second)
	if _, err := a.Enroll(first, "dom-a"); err != nil {
		t.Fatalf("first enroll must succeed, got %v", err)
	}
	if _, err := a.Enroll(second, "dom-b"); !errors.Is(err, impl.ErrGroupConflict) {
		t.Fatalf("enroll of a sibling into another domain = %v, want ErrGroupConflict", err)
	}
}

func TestEnrollRejectsUnknownDevice(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	_, err := a.Enroll(dev("8086", "1533", "0000:03:00.0"), "dom-a")
	if !errors.Is(err, impl.ErrUnknownDevice) {
		t.Fatalf("enroll unknown device = %v, want ErrUnknownDevice", err)
	}
}

func TestEnrollEnforcesDomainDeviceLimit(t *testing.T) {
	limits := dmaLimits()
	limits.MaxDevicesPerDomain = 2
	a := impl.NewDmaRemapAuthorizer(limits)
	registerGroup(t, a, "iommu-3",
		dev("10de", "1eb8", "0000:04:00.0"),
		dev("10de", "10f8", "0000:04:00.1"),
		dev("10de", "1ad6", "0000:04:00.2"),
	)
	_, err := a.Enroll(dev("10de", "1eb8", "0000:04:00.0"), "dom-a")
	if !errors.Is(err, impl.ErrDomainLimit) {
		t.Fatalf("enroll of a 3 device group into a 2 device domain = %v, want ErrDomainLimit", err)
	}
}

func TestAuthorizeReturnsActiveLease(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	d := dev("10de", "1eb8", "0000:05:00.0")
	registerGroup(t, a, "iommu-4", d)
	grant, err := a.Enroll(d, "dom-a")
	if err != nil {
		t.Fatalf("enroll must succeed, got %v", err)
	}
	lease, err := a.Authorize(grant.ID, 30*time.Second)
	if err != nil {
		t.Fatalf("authorize must succeed, got %v", err)
	}
	if lease.Domain != "dom-a" || lease.GrantID != grant.ID {
		t.Fatalf("lease = %+v, want domain dom-a and grant %s", lease, grant.ID)
	}
	if lease.Window != 30*time.Second {
		t.Fatalf("lease window = %v, want 30s", lease.Window)
	}
	if !lease.ExpiresAt.Equal(base.Add(30 * time.Second)) {
		t.Fatalf("lease expiry = %v, want %v", lease.ExpiresAt, base.Add(30*time.Second))
	}
	active := a.ActiveLeases("dom-a")
	if len(active) != 1 || active[0].ID != lease.ID {
		t.Fatalf("active leases = %+v, want exactly the new lease", active)
	}
	if usage := a.DomainUsage("dom-a"); usage.Leases != 1 {
		t.Fatalf("domain leases = %d, want 1", usage.Leases)
	}
}

func TestAuthorizeRejectsUnknownGrant(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	if _, err := a.Authorize("grant-never-issued", time.Second); !errors.Is(err, impl.ErrUnknownGrant) {
		t.Fatalf("authorize unknown grant = %v, want ErrUnknownGrant", err)
	}
}

func TestAuthorizeRejectsWindowOverCap(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	d := dev("10de", "1eb8", "0000:06:00.0")
	registerGroup(t, a, "iommu-5", d)
	grant, err := a.Enroll(d, "dom-a")
	if err != nil {
		t.Fatalf("enroll must succeed, got %v", err)
	}
	if _, err := a.Authorize(grant.ID, 90*time.Second); !errors.Is(err, impl.ErrWindowTooLong) {
		t.Fatalf("window above the per window cap = %v, want ErrWindowTooLong", err)
	}
	if _, err := a.Authorize(grant.ID, time.Minute); err != nil {
		t.Fatalf("window at the cap must succeed, got %v", err)
	}
}

func TestAuthorizeEnforcesWindowBudget(t *testing.T) {
	limits := dmaLimits()
	limits.MaxWindowTotal = 60 * time.Second
	a := impl.NewDmaRemapAuthorizer(limits)
	d := dev("10de", "1eb8", "0000:07:00.0")
	registerGroup(t, a, "iommu-6", d)
	grant, err := a.Enroll(d, "dom-a")
	if err != nil {
		t.Fatalf("enroll must succeed, got %v", err)
	}
	if _, err := a.Authorize(grant.ID, 40*time.Second); err != nil {
		t.Fatalf("first window must succeed, got %v", err)
	}
	if _, err := a.Authorize(grant.ID, 40*time.Second); !errors.Is(err, impl.ErrWindowBudget) {
		t.Fatalf("second window above the total budget = %v, want ErrWindowBudget", err)
	}
}

func TestRevokeUnbindsGroupAndRecordsReason(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	d := dev("10de", "1eb8", "0000:08:00.0")
	registerGroup(t, a, "iommu-7", d)
	grant, err := a.Enroll(d, "dom-a")
	if err != nil {
		t.Fatalf("enroll must succeed, got %v", err)
	}
	if _, err := a.Authorize(grant.ID, 30*time.Second); err != nil {
		t.Fatalf("authorize must succeed, got %v", err)
	}
	if err := a.Revoke(d, "hot-unplug during window"); err != nil {
		t.Fatalf("revoke must succeed, got %v", err)
	}
	view, err := a.GroupStatus("iommu-7")
	if err != nil {
		t.Fatalf("group status must be readable, got %v", err)
	}
	if view.Bound {
		t.Fatalf("group is still bound after revoke")
	}
	if !view.Revoked || view.Reason != "hot-unplug during window" {
		t.Fatalf("group view = %+v, want revoked with the recorded reason", view)
	}
	if view.ActiveLeases != 0 {
		t.Fatalf("active leases after revoke = %d, want 0", view.ActiveLeases)
	}
	if usage := a.DomainUsage("dom-a"); usage.Leases != 0 || usage.Devices != 0 {
		t.Fatalf("domain usage after revoke = %+v, want empty", usage)
	}
}

func TestRevokeRejectsUnknownDevice(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	if err := a.Revoke(dev("8086", "1533", "0000:09:00.0"), "gone"); !errors.Is(err, impl.ErrUnknownDevice) {
		t.Fatalf("revoke unknown device = %v, want ErrUnknownDevice", err)
	}
}

func TestSnapshotRestoreKeepsBindingAndWindow(t *testing.T) {
	limits := dmaLimits()
	a := impl.NewDmaRemapAuthorizer(limits)
	d := dev("10de", "1eb8", "0000:0a:00.0")
	registerGroup(t, a, "iommu-8", d)
	grant, err := a.Enroll(d, "dom-a")
	if err != nil {
		t.Fatalf("enroll must succeed, got %v", err)
	}
	lease, err := a.Authorize(grant.ID, 45*time.Second)
	if err != nil {
		t.Fatalf("authorize must succeed, got %v", err)
	}
	data, err := a.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	restored := impl.NewDmaRemapAuthorizer(limits)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	view, err := restored.GroupStatus("iommu-8")
	if err != nil {
		t.Fatalf("restored group must be readable, got %v", err)
	}
	if !view.Bound || view.Domain != "dom-a" {
		t.Fatalf("restored group view = %+v, want bound to dom-a", view)
	}
	active := restored.ActiveLeases("dom-a")
	if len(active) != 1 {
		t.Fatalf("restored active leases = %d, want 1", len(active))
	}
	if !active[0].ExpiresAt.Equal(lease.ExpiresAt) {
		t.Fatalf("restored expiry = %v, want %v", active[0].ExpiresAt, lease.ExpiresAt)
	}
}

func TestRestoreRejectsGarbage(t *testing.T) {
	a := impl.NewDmaRemapAuthorizer(dmaLimits())
	if err := a.Restore([]byte("broken snapshot bytes")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("restore garbage = %v, want ErrBadSnapshot", err)
	}
}
