// Package task owns the offline earnings ledger: it converts offline time into
// pending resources, keeps boosts bounded per kind and settles claims once.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Errors reported by the earnings ledger.
var (
	ErrBadPlayer      = errors.New("player id is required")
	ErrUnknownPlayer  = errors.New("unknown player")
	ErrBadTime        = errors.New("settle time is before the last online moment")
	ErrBadCap         = errors.New("offline cap must be positive")
	ErrBadRate        = errors.New("offline rate is invalid")
	ErrBadBagCapacity = errors.New("bag capacity must not be negative")
	ErrLimit          = errors.New("earnings table limit reached")
	ErrBadSnapshot    = errors.New("snapshot payload is invalid")
)

const snapshotVersion = 1

// Bonus is one multiplicative boost. Bonuses sharing a Kind are folded by
// taking the highest percent; Until marks the moment the boost expires.
type Bonus struct {
	Kind    string
	Percent int
	Until   time.Time
}

// Rate is the production rate of one player. Base is in units per hour.
type Rate struct {
	Base    int64
	Bonuses []Bonus
}

// Reward is one settled amount that is either still pending or already banked.
type Reward struct {
	Player  string
	Amount  int64
	Seconds int64
	Capped  bool
}

// OfflineReport describes one settlement window.
type OfflineReport struct {
	Player  string
	From    time.Time
	To      time.Time
	Seconds int64
	Amount  int64
	Capped  bool
}

// Limits bounds the ledger tables.
type Limits struct {
	MaxPlayers int
	MaxBonuses int
}

type pendingAccount struct {
	reward     Reward
	hasAccount bool
}

type playerState struct {
	rate          Rate
	hasRate       bool
	lastOnline    time.Time
	hasLastOnline bool
	settledAt     time.Time
	hasSettled    bool
	pending       pendingAccount
	bank          int64
}

// OfflineEarningsLedger owns baselines, pending entries and player balances.
type OfflineEarningsLedger struct {
	mu          sync.RWMutex
	limits      Limits
	cap         time.Duration
	bagCapacity int
	bagLimited  bool
	players     map[string]*playerState
}

type snapshotData struct {
	Version     int                       `json:"version"`
	Cap         int64                     `json:"cap_nanoseconds"`
	BagCapacity int                       `json:"bag_capacity"`
	BagLimited  bool                      `json:"bag_limited"`
	Players     map[string]snapshotPlayer `json:"players"`
}

type snapshotPlayer struct {
	Rate          Rate      `json:"rate"`
	HasRate       bool      `json:"has_rate"`
	LastOnline    time.Time `json:"last_online"`
	HasLastOnline bool      `json:"has_last_online"`
	SettledAt     time.Time `json:"settled_at"`
	HasSettled    bool      `json:"has_settled"`
	Pending       Reward    `json:"pending"`
	HasPending    bool      `json:"has_pending"`
	Bank          int64     `json:"bank"`
}

// NewOfflineEarningsLedger builds a ledger with the given caps.
func NewOfflineEarningsLedger(limits Limits) *OfflineEarningsLedger {
	if limits.MaxPlayers < 0 {
		limits.MaxPlayers = 0
	}
	if limits.MaxBonuses < 0 {
		limits.MaxBonuses = 0
	}
	return &OfflineEarningsLedger{
		limits:  limits,
		players: make(map[string]*playerState),
	}
}

// SetCap sets the maximum offline span that may be settled in one go.
func (l *OfflineEarningsLedger) SetCap(d time.Duration) error {
	if d <= 0 {
		return ErrBadCap
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = d
	return nil
}

// SetBagCapacity limits how many units one Claim can move into the bag. Calling
// it with zero models a currently full bag; the zero value before configuration
// means unlimited.
func (l *OfflineEarningsLedger) SetBagCapacity(n int) error {
	if n < 0 {
		return ErrBadBagCapacity
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.bagCapacity = n
	l.bagLimited = true
	return nil
}

// SetRate replaces the production rate of one player.
func (l *OfflineEarningsLedger) SetRate(player string, rate Rate) error {
	if player == "" {
		return ErrBadPlayer
	}

	rate = cloneRate(rate)
	if err := validateRate(rate, l.limits.MaxBonuses); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	p, exists := l.players[player]
	if !exists {
		if l.limits.MaxPlayers >= 0 && len(l.players) >= l.limits.MaxPlayers {
			return ErrLimit
		}
		p = &playerState{}
		l.players[player] = p
	}
	p.rate = rate
	p.hasRate = true
	return nil
}

// Record stores the last online moment of one player.
func (l *OfflineEarningsLedger) Record(player string, at time.Time) error {
	if player == "" {
		return ErrBadPlayer
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	p, exists := l.players[player]
	if !exists {
		if len(l.players) >= l.limits.MaxPlayers {
			return ErrLimit
		}
		p = &playerState{}
		l.players[player] = p
	}
	p.lastOnline = at
	p.hasLastOnline = true
	return nil
}

// Settle turns the offline span into a pending entry.
func (l *OfflineEarningsLedger) Settle(player string, now time.Time) (OfflineReport, error) {
	if player == "" {
		return OfflineReport{}, ErrBadPlayer
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	p, ok := l.players[player]
	if !ok {
		return OfflineReport{}, ErrUnknownPlayer
	}

	from, hasFrom := accrualStart(p)
	if !hasFrom {
		from = now
	}
	if now.Before(from) {
		return OfflineReport{}, ErrBadTime
	}
	if l.cap <= 0 {
		return OfflineReport{}, ErrBadCap
	}
	if !p.hasRate || p.rate.Base <= 0 {
		return OfflineReport{}, ErrBadRate
	}

	report, settled := calculate(player, p.rate, from, now, l.cap)
	p.settledAt = now
	p.hasSettled = true

	if p.pending.hasAccount {
		p.pending.reward.Amount += settled.Amount
		p.pending.reward.Seconds += settled.Seconds
		p.pending.reward.Capped = p.pending.reward.Capped || settled.Capped
	} else {
		p.pending = pendingAccount{reward: settled, hasAccount: true}
	}

	return report, nil
}

// Preview computes the reward of a settlement without touching the ledger.
func (l *OfflineEarningsLedger) Preview(player string, now time.Time) (Reward, error) {
	if player == "" {
		return Reward{}, ErrBadPlayer
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	p, ok := l.players[player]
	if !ok {
		return Reward{}, ErrUnknownPlayer
	}

	from, hasFrom := accrualStart(p)
	if !hasFrom {
		from = now
	}
	if now.Before(from) {
		return Reward{}, ErrBadTime
	}
	if l.cap <= 0 {
		return Reward{}, ErrBadCap
	}
	if !p.hasRate || p.rate.Base <= 0 {
		return Reward{}, ErrBadRate
	}

	_, reward := calculate(player, p.rate, from, now, l.cap)
	return reward, nil
}

// Claim moves the pending entry into the player balance.
func (l *OfflineEarningsLedger) Claim(player string) (Reward, error) {
	if player == "" {
		return Reward{}, ErrBadPlayer
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	p, ok := l.players[player]
	if !ok {
		return Reward{}, ErrUnknownPlayer
	}
	if !p.pending.hasAccount {
		return Reward{}, nil
	}
	if p.pending.reward.Amount <= 0 {
		p.pending = pendingAccount{}
		return Reward{}, nil
	}

	claimed := p.pending.reward
	fit := claimed.Amount
	if l.bagLimited {
		capacity := int64(l.bagCapacity)
		if fit > capacity {
			fit = capacity
		}
	}
	if fit <= 0 {
		return Reward{}, nil
	}
	claimed.Amount = fit

	p.bank += claimed.Amount
	p.pending.reward.Amount -= fit
	if p.pending.reward.Amount <= 0 {
		p.pending = pendingAccount{}
	}
	return claimed, nil
}

// Pending reads the entry that waits for a claim.
func (l *OfflineEarningsLedger) Pending(player string) (Reward, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	p, ok := l.players[player]
	if !ok || !p.pending.hasAccount {
		return Reward{}, false
	}
	return p.pending.reward, true
}

// Bank reads the amount already claimed by one player.
func (l *OfflineEarningsLedger) Bank(player string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()

	p, ok := l.players[player]
	if !ok {
		return 0
	}
	return p.bank
}

// Snapshot serialises baselines, rates, pending entries and balances.
func (l *OfflineEarningsLedger) Snapshot() ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	data := snapshotData{
		Version:     snapshotVersion,
		Cap:         int64(l.cap),
		BagCapacity: l.bagCapacity,
		BagLimited:  l.bagLimited,
		Players:     make(map[string]snapshotPlayer, len(l.players)),
	}
	for id, p := range l.players {
		data.Players[id] = snapshotPlayer{
			Rate:          cloneRate(p.rate),
			HasRate:       p.hasRate,
			LastOnline:    p.lastOnline,
			HasLastOnline: p.hasLastOnline,
			SettledAt:     p.settledAt,
			HasSettled:    p.hasSettled,
			Pending:       p.pending.reward,
			HasPending:    p.pending.hasAccount,
			Bank:          p.bank,
		}
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	return payload, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (l *OfflineEarningsLedger) Restore(data []byte) error {
	var decoded snapshotData
	if len(data) == 0 {
		return ErrBadSnapshot
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if err := validateSnapshot(decoded, l.limits.MaxPlayers, l.limits.MaxBonuses); err != nil {
		return err
	}

	players := make(map[string]*playerState, len(decoded.Players))
	for id, src := range decoded.Players {
		p := &playerState{
			rate:          cloneRate(src.Rate),
			hasRate:       src.HasRate,
			lastOnline:    src.LastOnline,
			hasLastOnline: src.HasLastOnline,
			settledAt:     src.SettledAt,
			hasSettled:    src.HasSettled,
			pending: pendingAccount{
				reward:     src.Pending,
				hasAccount: src.HasPending,
			},
			bank: src.Bank,
		}
		if src.HasPending {
			p.pending.reward.Player = id
		}
		players[id] = p
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = time.Duration(decoded.Cap)
	l.bagCapacity = decoded.BagCapacity
	l.bagLimited = decoded.BagLimited
	l.players = players
	return nil
}

func accrualStart(p *playerState) (time.Time, bool) {
	switch {
	case p.hasSettled && p.hasLastOnline && p.lastOnline.After(p.settledAt):
		return p.lastOnline, true
	case p.hasSettled:
		return p.settledAt, true
	case p.hasLastOnline:
		return p.lastOnline, true
	default:
		return time.Time{}, false
	}
}

func calculate(player string, rate Rate, from, to time.Time, cap time.Duration) (OfflineReport, Reward) {
	windowEnd := to
	capped := false
	if cap > 0 && to.Sub(from) > cap {
		capped = true
		windowEnd = from.Add(cap)
	}

	cuts := bonusCuts(rate.Bonuses, from, windowEnd)
	var seconds, amount int64
	segmentStart := from
	for _, cut := range cuts {
		segmentSeconds, segmentAmount := calculateSegment(rate, segmentStart, cut)
		seconds += segmentSeconds
		amount += segmentAmount
		segmentStart = cut
	}
	segmentSeconds, segmentAmount := calculateSegment(rate, segmentStart, windowEnd)
	seconds += segmentSeconds
	amount += segmentAmount

	report := OfflineReport{
		Player:  player,
		From:    from,
		To:      to,
		Seconds: seconds,
		Amount:  amount,
		Capped:  capped,
	}
	reward := Reward{
		Player:  player,
		Amount:  amount,
		Seconds: seconds,
		Capped:  capped,
	}
	return report, reward
}

func bonusCuts(bonuses []Bonus, from, end time.Time) []time.Time {
	cutSet := make(map[time.Time]struct{})
	for _, bonus := range bonuses {
		if bonus.Until.IsZero() {
			continue
		}
		if bonus.Until.After(from) && bonus.Until.Before(end) {
			cutSet[bonus.Until] = struct{}{}
		}
	}

	cuts := make([]time.Time, 0, len(cutSet))
	for cut := range cutSet {
		cuts = append(cuts, cut)
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })
	return cuts
}

func calculateSegment(rate Rate, from, to time.Time) (int64, int64) {
	if !to.After(from) {
		return 0, 0
	}

	seconds := int64(to.Sub(from) / time.Second)
	if seconds <= 0 {
		return 0, 0
	}

	amount := rate.Base * seconds / 3600
	bestByKind := make(map[string]int)
	for _, bonus := range rate.Bonuses {
		active := bonus.Until.IsZero() || bonus.Until.After(from)
		if !active {
			continue
		}
		if bonus.Percent > bestByKind[bonus.Kind] {
			bestByKind[bonus.Kind] = bonus.Percent
		}
	}

	kinds := make([]string, 0, len(bestByKind))
	for kind := range bestByKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		percent := bestByKind[kind]
		amount = amount * int64(100+percent) / 100
	}
	return seconds, amount
}

func cloneRate(rate Rate) Rate {
	if rate.Bonuses != nil {
		rate.Bonuses = append([]Bonus(nil), rate.Bonuses...)
	}
	return rate
}

func validateRate(rate Rate, maxBonuses int) error {
	if rate.Base <= 0 {
		return ErrBadRate
	}
	for _, bonus := range rate.Bonuses {
		if bonus.Percent < 0 {
			return ErrBadRate
		}
	}
	if maxBonuses >= 0 && len(rate.Bonuses) > maxBonuses {
		return ErrLimit
	}
	return nil
}

func validateSnapshot(data snapshotData, maxPlayers, maxBonuses int) error {
	if data.Version != snapshotVersion {
		return fmt.Errorf("%w: unsupported version", ErrBadSnapshot)
	}
	if data.Cap < 0 || data.BagCapacity < 0 {
		return fmt.Errorf("%w: negative ledger setting", ErrBadSnapshot)
	}
	if maxPlayers >= 0 && len(data.Players) > maxPlayers {
		return fmt.Errorf("%w: too many players", ErrBadSnapshot)
	}

	for id, player := range data.Players {
		if id == "" {
			return fmt.Errorf("%w: empty player", ErrBadSnapshot)
		}
		if player.Bank < 0 || player.Pending.Amount < 0 || player.Pending.Seconds < 0 {
			return fmt.Errorf("%w: negative balance", ErrBadSnapshot)
		}
		if player.HasRate {
			if err := validateRate(player.Rate, maxBonuses); err != nil {
				return fmt.Errorf("%w: %v", ErrBadSnapshot, err)
			}
		}
		if player.HasPending && player.Pending.Amount < 0 {
			return fmt.Errorf("%w: invalid pending reward", ErrBadSnapshot)
		}
	}
	return nil
}
