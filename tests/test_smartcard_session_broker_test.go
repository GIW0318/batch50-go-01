package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-19"
)

const testPIN = "1234"

func brokerConfig() impl.Config {
	return impl.Config{MaxSessionsPerReader: 4, MaxChannelsPerCard: 4, PINRetries: 3, MaxReaders: 4}
}

func provisioned(t *testing.T, b *impl.SmartcardSessionBroker, cardID string) {
	t.Helper()
	if err := b.Provision(cardID, []byte(testPIN)); err != nil {
		t.Fatalf("provision %s: %v", cardID, err)
	}
}

func openSession(t *testing.T, b *impl.SmartcardSessionBroker, reader string) impl.Session {
	t.Helper()
	s, err := b.Open(reader, impl.AID("a0000000031010"))
	if err != nil {
		t.Fatalf("open session on %s: %v", reader, err)
	}
	return s
}

func TestProvisionAndPlug(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	if err := b.Provision("  ", []byte(testPIN)); !errors.Is(err, impl.ErrBadCard) {
		t.Fatalf("blank card id = %v, want ErrBadCard", err)
	}
	for _, pin := range []string{"123", "1234567890123"} {
		if err := b.Provision("card-A", []byte(pin)); !errors.Is(err, impl.ErrBadPIN) {
			t.Fatalf("pin of length %d = %v, want ErrBadPIN", len(pin), err)
		}
	}
	provisioned(t, b, "card-A")
	st, err := b.PINState("card-A")
	if err != nil {
		t.Fatalf("pin state: %v", err)
	}
	if st.Remaining != 3 || st.Locked || st.Seq != 1 {
		t.Fatalf("fresh pin state = %+v, want 3 remaining and unlocked", st)
	}
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug card: %v", err)
	}
	view, err := b.ReaderState("reader-1")
	if err != nil {
		t.Fatalf("reader state: %v", err)
	}
	if !view.Present || view.CardID != "card-A" || view.Sessions != 0 || view.Channels != 0 {
		t.Fatalf("reader view = %+v, want card-A present with no sessions", view)
	}
	if _, err := b.PINState("card-404"); !errors.Is(err, impl.ErrUnknownCard) {
		t.Fatalf("pin state of an unknown card = %v, want ErrUnknownCard", err)
	}
}

func TestPlugDuplicateAndLimit(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(impl.Config{MaxSessionsPerReader: 2, MaxChannelsPerCard: 2, PINRetries: 3, MaxReaders: 2})
	for _, card := range []string{"card-A", "card-B", "card-C"} {
		provisioned(t, b, card)
	}
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug first reader: %v", err)
	}
	if err := b.Plug("reader-1", "card-B"); !errors.Is(err, impl.ErrReaderExists) {
		t.Fatalf("second card on the same reader = %v, want ErrReaderExists", err)
	}
	if err := b.Plug("reader-2", "card-B"); err != nil {
		t.Fatalf("plug second reader: %v", err)
	}
	if err := b.Plug("reader-3", "card-C"); !errors.Is(err, impl.ErrReaderLimit) {
		t.Fatalf("third reader = %v, want ErrReaderLimit", err)
	}
	if _, err := b.ReaderState("reader-3"); !errors.Is(err, impl.ErrUnknownReader) {
		t.Fatalf("rejected reader must not exist, got %v", err)
	}
	view, err := b.ReaderState("reader-1")
	if err != nil || view.CardID != "card-A" || !view.Present {
		t.Fatalf("reader-1 view = %+v err=%v, want the original card", view, err)
	}
}

func TestPlugUnknownCard(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	if err := b.Plug("reader-1", "card-404"); !errors.Is(err, impl.ErrUnknownCard) {
		t.Fatalf("plug of an unprovisioned card = %v, want ErrUnknownCard", err)
	}
	if _, err := b.ReaderState("reader-1"); !errors.Is(err, impl.ErrUnknownReader) {
		t.Fatalf("rejected plug must not create a reader, got %v", err)
	}
	if err := b.Plug("  ", "card-A"); !errors.Is(err, impl.ErrBadReader) {
		t.Fatalf("blank reader name = %v, want ErrBadReader", err)
	}
}

func TestOpenAssignsLowestChannel(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	first := openSession(t, b, "reader-1")
	second := openSession(t, b, "reader-1")
	if first.Channel != 1 || second.Channel != 2 {
		t.Fatalf("channels = %d,%d, want 1 then 2", first.Channel, second.Channel)
	}
	if first.ID == second.ID || first.ID == "" {
		t.Fatalf("session ids = %q,%q, want distinct non empty values", first.ID, second.ID)
	}
	if first.CardID != "card-A" || first.Reader != "reader-1" || first.App != impl.AID("a0000000031010") {
		t.Fatalf("session record = %+v, want card-A on reader-1", first)
	}
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("session seq = %d,%d, want 1 then 2", first.Seq, second.Seq)
	}
	view, _ := b.ReaderState("reader-1")
	if view.Sessions != 2 || view.Channels != 2 {
		t.Fatalf("reader view = %+v, want two sessions on two channels", view)
	}
	if err := b.Close(second.ID); err != nil {
		t.Fatalf("close second session: %v", err)
	}
	view, _ = b.ReaderState("reader-1")
	if view.Sessions != 1 {
		t.Fatalf("reader sessions after close = %d, want 1", view.Sessions)
	}
	third := openSession(t, b, "reader-1")
	if third.Channel != 2 {
		t.Fatalf("channel after close = %d, want the freed channel 2", third.Channel)
	}
}

func TestOpenWithoutCard(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	provisioned(t, b, "card-A")
	if _, err := b.Open("reader-1", impl.AID("a0000000031010")); !errors.Is(err, impl.ErrUnknownReader) {
		t.Fatalf("open on an unknown reader = %v, want ErrUnknownReader", err)
	}
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	if _, err := b.Open("reader-1", impl.AID("  ")); !errors.Is(err, impl.ErrBadApp) {
		t.Fatalf("blank application id = %v, want ErrBadApp", err)
	}
	if err := b.Unplug("reader-1"); err != nil {
		t.Fatalf("unplug: %v", err)
	}
	if _, err := b.Open("reader-1", impl.AID("a0000000031010")); !errors.Is(err, impl.ErrNoCard) {
		t.Fatalf("open on a reader without a card = %v, want ErrNoCard", err)
	}
}

func TestOpenChannelLimitPerCard(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(impl.Config{MaxSessionsPerReader: 4, MaxChannelsPerCard: 2, PINRetries: 3, MaxReaders: 4})
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	for i := 0; i < 2; i++ {
		openSession(t, b, "reader-1")
	}
	if _, err := b.Open("reader-1", impl.AID("a0000000031010")); !errors.Is(err, impl.ErrChannelLimit) {
		t.Fatalf("third channel = %v, want ErrChannelLimit", err)
	}
	view, _ := b.ReaderState("reader-1")
	if view.Sessions != 2 || view.Channels != 2 {
		t.Fatalf("reader view = %+v, want the rejected open to leave no trace", view)
	}
}

func TestOpenSessionLimitPerReader(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(impl.Config{MaxSessionsPerReader: 1, MaxChannelsPerCard: 4, PINRetries: 3, MaxReaders: 4})
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	openSession(t, b, "reader-1")
	if _, err := b.Open("reader-1", impl.AID("a0000000031010")); !errors.Is(err, impl.ErrSessionLimit) {
		t.Fatalf("second session = %v, want ErrSessionLimit", err)
	}
	view, _ := b.ReaderState("reader-1")
	if view.Sessions != 1 {
		t.Fatalf("reader sessions = %d, want the cap 1", view.Sessions)
	}
}

func TestTransmitEchoesAPDUWithStatus(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	s := openSession(t, b, "reader-1")
	apdu := []byte{0x00, 0xA4, 0x04, 0x00, 0x07}
	resp, err := b.Transmit(s.ID, apdu)
	if err != nil {
		t.Fatalf("transmit: %v", err)
	}
	if len(resp) != len(apdu)+2 {
		t.Fatalf("response length = %d, want %d", len(resp), len(apdu)+2)
	}
	for i := range apdu {
		if resp[i] != apdu[i] {
			t.Fatalf("response does not echo the apdu: %v vs %v", resp, apdu)
		}
	}
	if resp[len(apdu)] != 0x90 || resp[len(apdu)+1] != 0x00 {
		t.Fatalf("status word = %02x%02x, want 9000", resp[len(apdu)], resp[len(apdu)+1])
	}
	if _, err := b.Transmit(s.ID, []byte{0x00, 0xA4, 0x04}); !errors.Is(err, impl.ErrBadAPDU) {
		t.Fatalf("short apdu = %v, want ErrBadAPDU", err)
	}
	if _, err := b.Transmit(s.ID, nil); !errors.Is(err, impl.ErrBadAPDU) {
		t.Fatalf("empty apdu = %v, want ErrBadAPDU", err)
	}
}

func TestTransmitUnknownSession(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	for _, id := range []string{"sess-404", ""} {
		if _, err := b.Transmit(id, []byte{0x00, 0xA4, 0x04, 0x00}); !errors.Is(err, impl.ErrUnknownSession) {
			t.Fatalf("transmit on session %q = %v, want ErrUnknownSession", id, err)
		}
	}
	if err := b.Close("sess-404"); !errors.Is(err, impl.ErrUnknownSession) {
		t.Fatalf("close of an unknown session = %v, want ErrUnknownSession", err)
	}
}

func TestVerifyPINWrongDecrementsRemaining(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	s := openSession(t, b, "reader-1")
	for i, want := range []int{2, 1, 0} {
		st, err := b.VerifyPIN(s.ID, []byte("9999"))
		if err != nil {
			t.Fatalf("wrong pin %d: %v", i, err)
		}
		if st.Remaining != want {
			t.Fatalf("remaining after wrong pin %d = %d, want %d", i, st.Remaining, want)
		}
		if st.Locked != (want == 0) {
			t.Fatalf("lock state after wrong pin %d = %v, want %v", i, st.Locked, want == 0)
		}
		persisted, err := b.PINState("card-A")
		if err != nil {
			t.Fatalf("pin state: %v", err)
		}
		if persisted.Remaining != want || persisted.Locked != (want == 0) {
			t.Fatalf("stored pin state after wrong pin %d = %+v", i, persisted)
		}
	}
	if _, err := b.VerifyPIN(s.ID, []byte(testPIN)); !errors.Is(err, impl.ErrPINLocked) {
		t.Fatalf("correct pin on a locked card = %v, want ErrPINLocked", err)
	}
	locked, _ := b.PINState("card-A")
	if !locked.Locked || locked.Remaining != 0 {
		t.Fatalf("locked card state = %+v, want locked with no attempts left", locked)
	}
}

func TestVerifyPINCorrectAndBadLength(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	s := openSession(t, b, "reader-1")
	for i := 0; i < 2; i++ {
		if _, err := b.VerifyPIN(s.ID, []byte("0000")); err != nil {
			t.Fatalf("wrong pin %d: %v", i, err)
		}
	}
	st, err := b.VerifyPIN(s.ID, []byte(testPIN))
	if err != nil {
		t.Fatalf("correct pin: %v", err)
	}
	if st.Remaining != 3 || st.Locked {
		t.Fatalf("pin state after a correct entry = %+v, want the counter reset", st)
	}
	if _, err := b.VerifyPIN(s.ID, []byte("123")); !errors.Is(err, impl.ErrBadPIN) {
		t.Fatalf("three digit pin = %v, want ErrBadPIN", err)
	}
	st, _ = b.PINState("card-A")
	if st.Remaining != 3 {
		t.Fatalf("malformed pin consumed an attempt, remaining = %d", st.Remaining)
	}
}

func TestUnplugInvalidatesSessions(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	s := openSession(t, b, "reader-1")
	if err := b.Unplug("reader-1"); err != nil {
		t.Fatalf("unplug: %v", err)
	}
	if _, err := b.Transmit(s.ID, []byte{0x00, 0xA4, 0x04, 0x00}); !errors.Is(err, impl.ErrSessionGone) {
		t.Fatalf("transmit on a session of a removed card = %v, want ErrSessionGone", err)
	}
	view, err := b.ReaderState("reader-1")
	if err != nil {
		t.Fatalf("reader state: %v", err)
	}
	if view.Present || view.Sessions != 0 || view.Channels != 0 {
		t.Fatalf("reader view after unplug = %+v, want an empty slot", view)
	}
	if _, err := b.Open("reader-1", impl.AID("a0000000031010")); !errors.Is(err, impl.ErrNoCard) {
		t.Fatalf("open after unplug = %v, want ErrNoCard", err)
	}
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("replug: %v", err)
	}
	again := openSession(t, b, "reader-1")
	if again.ID == s.ID {
		t.Fatalf("session id was reused after a replug: %q", again.ID)
	}
	if again.Seq <= s.Seq {
		t.Fatalf("session seq after replug = %d, must move past %d", again.Seq, s.Seq)
	}
	if again.Channel != 1 {
		t.Fatalf("first channel after replug = %d, want 1", again.Channel)
	}
}

func TestConcurrentOpenRespectsSessionLimit(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(impl.Config{MaxSessionsPerReader: 4, MaxChannelsPerCard: 8, PINRetries: 3, MaxReaders: 4})
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	channels := make([]int, 0, 4)
	limited := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := b.Open("reader-1", impl.AID("a0000000031010"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				channels = append(channels, s.Channel)
			case errors.Is(err, impl.ErrSessionLimit):
				limited++
			}
		}()
	}
	wg.Wait()
	if len(channels) != 4 {
		t.Fatalf("concurrent open succeeded %d times, want exactly 4", len(channels))
	}
	if limited != 12 {
		t.Fatalf("concurrent open reported %d limit errors, want 12", limited)
	}
	seen := map[int]bool{}
	for _, ch := range channels {
		if ch < 1 || ch > 4 {
			t.Fatalf("channel %d is outside the allocated range 1..4", ch)
		}
		if seen[ch] {
			t.Fatalf("channel %d was handed out twice: %v", ch, channels)
		}
		seen[ch] = true
	}
	view, _ := b.ReaderState("reader-1")
	if view.Sessions != 4 || view.Channels != 4 {
		t.Fatalf("reader view after the race = %+v, want four sessions", view)
	}
}

func TestSnapshotRestoreKeepsPINAndReaders(t *testing.T) {
	cfg := brokerConfig()
	b := impl.NewSmartcardSessionBroker(cfg)
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	s := openSession(t, b, "reader-1")
	if _, err := b.VerifyPIN(s.ID, []byte("0000")); err != nil {
		t.Fatalf("wrong pin: %v", err)
	}
	data, err := b.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	r := impl.NewSmartcardSessionBroker(cfg)
	if err := r.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, err := r.PINState("card-A")
	if err != nil {
		t.Fatalf("pin state after restore: %v", err)
	}
	if st.Remaining != 2 || st.Locked {
		t.Fatalf("restored pin state = %+v, want 2 remaining and unlocked", st)
	}
	view, err := r.ReaderState("reader-1")
	if err != nil {
		t.Fatalf("reader state after restore: %v", err)
	}
	if !view.Present || view.CardID != "card-A" || view.Sessions != 0 {
		t.Fatalf("restored reader view = %+v, want the card present without sessions", view)
	}
	if _, err := r.Transmit(s.ID, []byte{0x00, 0xA4, 0x04, 0x00}); !errors.Is(err, impl.ErrUnknownSession) {
		t.Fatalf("session from before the restart = %v, want ErrUnknownSession", err)
	}
	fresh := openSession(t, r, "reader-1")
	if fresh.ID == s.ID || fresh.Seq <= s.Seq {
		t.Fatalf("session after restore = %+v, must not reuse %+v", fresh, s)
	}
	reset, err := r.VerifyPIN(fresh.ID, []byte(testPIN))
	if err != nil {
		t.Fatalf("correct pin after restore: %v", err)
	}
	if reset.Remaining != 3 || reset.Locked {
		t.Fatalf("pin state after the correct entry = %+v, want the counter reset", reset)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	b := impl.NewSmartcardSessionBroker(brokerConfig())
	provisioned(t, b, "card-A")
	if err := b.Plug("reader-1", "card-A"); err != nil {
		t.Fatalf("plug: %v", err)
	}
	s := openSession(t, b, "reader-1")
	if _, err := b.VerifyPIN(s.ID, []byte("0000")); err != nil {
		t.Fatalf("wrong pin: %v", err)
	}
	if err := b.Restore([]byte("}{ not a snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot = %v, want ErrBadSnapshot", err)
	}
	st, err := b.PINState("card-A")
	if err != nil || st.Remaining != 2 {
		t.Fatalf("failed restore changed the pin state: %+v err=%v", st, err)
	}
	view, err := b.ReaderState("reader-1")
	if err != nil || !view.Present || view.Sessions != 1 {
		t.Fatalf("failed restore changed the reader view: %+v err=%v", view, err)
	}
	if _, err := b.Transmit(s.ID, []byte{0x00, 0xA4, 0x04, 0x00}); err != nil {
		t.Fatalf("live session must keep working after a rejected restore, got %v", err)
	}
}
