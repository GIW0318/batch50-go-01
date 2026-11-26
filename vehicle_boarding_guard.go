// Package task guards the seats of every spawned vehicle: who sits where,
// who holds the driving right, and which dismount events have been emitted.
package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Seat roles. Every vehicle has exactly one driver seat.
const (
	RoleDriver    = "driver"
	RoleGunner    = "gunner"
	RolePassenger = "passenger"

	roleSpectator = "spectator"
)

// DismountReason explains why a player was pushed out of a vehicle.
type DismountReason string

// Dismount reasons written into the event log.
const (
	ReasonDestroyed    DismountReason = "destroyed"
	ReasonGlideTimeout DismountReason = "glide_timeout"
	ReasonKicked       DismountReason = "kicked"
)

// Errors returned by the guard.
var (
	ErrVehicleExists   = errors.New("vehicle already spawned")
	ErrVehicleNotFound = errors.New("vehicle not found")
	ErrBadSeat         = errors.New("seat spec is invalid")
	ErrNoDriverSeat    = errors.New("vehicle needs exactly one driver seat")
	ErrSeatLimit       = errors.New("seat count exceeds the vehicle limit")
	ErrSeatTaken       = errors.New("seat is already taken")
	ErrNotOnBoard      = errors.New("player is not on this vehicle")
	ErrBadPlayer       = errors.New("player id is required")
	ErrNotDriver       = errors.New("player does not hold the driving right")
	ErrPassengerLimit  = errors.New("passenger seat limit reached")
	ErrNotGliding      = errors.New("vehicle is not in the glide window")
	ErrBadSnapshot     = errors.New("snapshot payload is invalid")
)

// SeatSpec describes one seat at spawn time.
type SeatSpec struct {
	Index int    `json:"index"`
	Role  string `json:"role"`
}

// Limits bounds one vehicle.
type Limits struct {
	MaxSeats      int
	MaxPassengers int
	GlideSeconds  int
	MaxEventsKept int
}

// Boarding is the receipt of one successful boarding.
type Boarding struct {
	VehicleID   string `json:"vehicle_id"`
	PlayerID    string `json:"player_id"`
	SeatIndex   int    `json:"seat_index"`
	Role        string `json:"role"`
	Seq         int64  `json:"seq"`
	Reconnected bool   `json:"reconnected"`
}

// SeatState is the public view of one seat.
type SeatState struct {
	VehicleID string `json:"vehicle_id"`
	PlayerID  string `json:"player_id"`
	SeatIndex int    `json:"seat_index"`
	Role      string `json:"role"`
	Occupied  bool   `json:"occupied"`
}

// DismountEvent is one forced exit appended to the per vehicle log.
type DismountEvent struct {
	VehicleID string         `json:"vehicle_id"`
	PlayerID  string         `json:"player_id"`
	SeatIndex int            `json:"seat_index"`
	Reason    DismountReason `json:"reason"`
	Seq       int64          `json:"seq"`
}

// GlideInfo reports whether a vehicle runs without a driver.
type GlideInfo struct {
	VehicleID string `json:"vehicle_id"`
	Gliding   bool   `json:"gliding"`
	DriverID  string `json:"driver_id"`
	SinceSeq  int64  `json:"since_seq"`
}

type seatSlot struct {
	index  int
	role   string
	player string
}

type seatRef struct {
	index int
	role  string
}

type glideWindow struct {
	driverID string
	sinceSeq int64
	started  time.Time
}

type vehicleState struct {
	id         string
	seats      map[int]*seatSlot
	order      []int
	events     []DismountEvent
	glide      *glideWindow
	lastSeats  map[string]seatRef
	spectators map[string]bool
}

func (v *vehicleState) occupiedCount() int {
	n := 0
	for _, seat := range v.seats {
		if seat.player != "" {
			n++
		}
	}
	return n
}

func (v *vehicleState) passengerCount() int {
	n := 0
	for _, seat := range v.seats {
		if seat.player != "" && seat.role == RolePassenger {
			n++
		}
	}
	return n
}

func (v *vehicleState) seatOfPlayer(playerID string) (*seatSlot, bool) {
	for _, seat := range v.seats {
		if seat.player == playerID && playerID != "" {
			return seat, true
		}
	}
	return nil, false
}

func (v *vehicleState) driverSeat() *seatSlot {
	for _, seat := range v.seats {
		if seat.role == RoleDriver {
			return seat
		}
	}
	return nil
}

func (v *vehicleState) clearTransientState() {
	v.glide = nil
	v.lastSeats = make(map[string]seatRef)
	v.spectators = make(map[string]bool)
}

// VehicleBoardingGuard owns the per vehicle seat tables.
type VehicleBoardingGuard struct {
	mu       sync.Mutex
	limits   Limits
	vehicles map[string]*vehicleState
	seq      int64
}

// NewVehicleBoardingGuard builds a guard with the given caps.
func NewVehicleBoardingGuard(limits Limits) *VehicleBoardingGuard {
	return &VehicleBoardingGuard{
		limits:   limits,
		vehicles: make(map[string]*vehicleState),
	}
}

func validRole(role string) bool {
	return role == RoleDriver || role == RoleGunner || role == RolePassenger
}

func blankPlayer(playerID string) bool {
	return strings.TrimSpace(playerID) == ""
}

func buildVehicle(vehicleID string, seats []SeatSpec, limits Limits) (*vehicleState, error) {
	v := &vehicleState{
		id:         vehicleID,
		seats:      make(map[int]*seatSlot, len(seats)),
		order:      make([]int, 0, len(seats)),
		events:     make([]DismountEvent, 0),
		lastSeats:  make(map[string]seatRef),
		spectators: make(map[string]bool),
	}
	drivers := 0
	for _, spec := range seats {
		if spec.Index < 0 || !validRole(spec.Role) {
			return nil, ErrBadSeat
		}
		if _, exists := v.seats[spec.Index]; exists {
			return nil, ErrBadSeat
		}
		if spec.Role == RoleDriver {
			drivers++
		}
		v.seats[spec.Index] = &seatSlot{index: spec.Index, role: spec.Role}
		v.order = append(v.order, spec.Index)
	}
	if drivers > 1 {
		return nil, ErrBadSeat
	}
	if len(seats) > limits.MaxSeats {
		return nil, ErrSeatLimit
	}
	if drivers == 0 {
		return nil, ErrNoDriverSeat
	}
	sort.Ints(v.order)
	return v, nil
}

// Spawn registers one vehicle and its seat layout.
func (g *VehicleBoardingGuard) Spawn(vehicleID string, seats []SeatSpec) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.vehicles == nil {
		g.vehicles = make(map[string]*vehicleState)
	}
	if _, exists := g.vehicles[vehicleID]; exists {
		return ErrVehicleExists
	}
	v, err := buildVehicle(vehicleID, seats, g.limits)
	if err != nil {
		return err
	}
	g.vehicles[vehicleID] = v
	return nil
}

func (g *VehicleBoardingGuard) appendEvent(v *vehicleState, playerID string, seatIndex int, reason DismountReason) DismountEvent {
	g.seq++
	event := DismountEvent{
		VehicleID: v.id,
		PlayerID:  playerID,
		SeatIndex: seatIndex,
		Reason:    reason,
		Seq:       g.seq,
	}
	v.events = append(v.events, event)
	if g.limits.MaxEventsKept > 0 && len(v.events) > g.limits.MaxEventsKept {
		keep := g.limits.MaxEventsKept
		trimmed := make([]DismountEvent, keep)
		copy(trimmed, v.events[len(v.events)-keep:])
		v.events = trimmed
	}
	return event
}

func (g *VehicleBoardingGuard) boarding(v *vehicleState, playerID string, seatIndex int, role string, reconnected bool) Boarding {
	g.seq++
	return Boarding{
		VehicleID:   v.id,
		PlayerID:    playerID,
		SeatIndex:   seatIndex,
		Role:        role,
		Seq:         g.seq,
		Reconnected: reconnected,
	}
}

// Board places a player into one seat.
func (g *VehicleBoardingGuard) Board(vehicleID, playerID string, seatIndex int) (Boarding, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	v, ok := g.vehicles[vehicleID]
	if !ok {
		return Boarding{}, ErrVehicleNotFound
	}
	if blankPlayer(playerID) {
		return Boarding{}, ErrBadPlayer
	}
	seat, ok := v.seats[seatIndex]
	if !ok {
		return Boarding{}, ErrBadSeat
	}
	if seat.player != "" {
		return Boarding{}, ErrSeatTaken
	}
	previousSeat, alreadyOnBoard := v.seatOfPlayer(playerID)
	if seat.role == RolePassenger && v.passengerCount() >= g.limits.MaxPassengers &&
		(!alreadyOnBoard || previousSeat.role != RolePassenger) {
		return Boarding{}, ErrPassengerLimit
	}

	if alreadyOnBoard {
		previousSeat.player = ""
	}
	seat.player = playerID
	delete(v.lastSeats, playerID)
	delete(v.spectators, playerID)
	if seat.role == RoleDriver {
		v.glide = nil
	} else if alreadyOnBoard && previousSeat.role == RoleDriver && v.occupiedCount() > 0 {
		v.glide = &glideWindow{
			driverID: playerID,
			sinceSeq: g.seq,
			started:  time.Now(),
		}
	}
	return g.boarding(v, playerID, seat.index, seat.role, false), nil
}

// Leave removes a player from the vehicle.
func (g *VehicleBoardingGuard) Leave(vehicleID, playerID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	v, ok := g.vehicles[vehicleID]
	if !ok {
		return ErrVehicleNotFound
	}
	if blankPlayer(playerID) {
		return ErrBadPlayer
	}
	seat, ok := v.seatOfPlayer(playerID)
	if !ok {
		if v.spectators[playerID] {
			delete(v.spectators, playerID)
			return nil
		}
		return ErrNotOnBoard
	}

	seat.player = ""
	v.lastSeats[playerID] = seatRef{index: seat.index, role: seat.role}
	if seat.role == RoleDriver {
		if v.occupiedCount() > 0 {
			v.glide = &glideWindow{
				driverID: playerID,
				sinceSeq: g.seq,
				started:  time.Now(),
			}
		} else {
			v.glide = nil
		}
	} else if v.glide != nil && v.occupiedCount() == 0 {
		v.glide = nil
	}
	return nil
}

// TransferDriver hands the driving right to another occupant of the same vehicle.
func (g *VehicleBoardingGuard) TransferDriver(vehicleID, fromPlayer, toPlayer string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	v, ok := g.vehicles[vehicleID]
	if !ok {
		return ErrVehicleNotFound
	}
	fromSeat, ok := v.seatOfPlayer(fromPlayer)
	if !ok || fromSeat.role != RoleDriver {
		return ErrNotDriver
	}
	if fromPlayer == toPlayer {
		return ErrNotOnBoard
	}
	toSeat, ok := v.seatOfPlayer(toPlayer)
	if !ok {
		return ErrNotOnBoard
	}

	fromSeat.player, toSeat.player = toPlayer, fromPlayer
	return nil
}

// ForceDismount clears every seat at once and returns the pushed players.
func (g *VehicleBoardingGuard) ForceDismount(vehicleID string, reason DismountReason) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	v, ok := g.vehicles[vehicleID]
	if !ok {
		return nil, ErrVehicleNotFound
	}
	players := make([]string, 0, v.occupiedCount())
	for _, index := range v.order {
		seat := v.seats[index]
		if seat.player == "" {
			continue
		}
		playerID := seat.player
		seat.player = ""
		players = append(players, playerID)
		g.appendEvent(v, playerID, seat.index, reason)
	}
	v.clearTransientState()
	return players, nil
}

// SeatOf reports where one player sits.
func (g *VehicleBoardingGuard) SeatOf(vehicleID, playerID string) (SeatState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	v, ok := g.vehicles[vehicleID]
	if !ok {
		return SeatState{}, ErrVehicleNotFound
	}
	seat, ok := v.seatOfPlayer(playerID)
	if !ok {
		return SeatState{}, ErrNotOnBoard
	}
	return SeatState{
		VehicleID: v.id,
		PlayerID:  playerID,
		SeatIndex: seat.index,
		Role:      seat.role,
		Occupied:  true,
	}, nil
}

// Occupants lists the taken seats by seat index.
func (g *VehicleBoardingGuard) Occupants(vehicleID string) []SeatState {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]SeatState, 0)
	v, ok := g.vehicles[vehicleID]
	if !ok {
		return out
	}
	for _, index := range v.order {
		seat := v.seats[index]
		if seat.player == "" {
			continue
		}
		out = append(out, SeatState{
			VehicleID: v.id,
			PlayerID:  seat.player,
			SeatIndex: seat.index,
			Role:      seat.role,
			Occupied:  true,
		})
	}
	return out
}

// Events returns a copy of the forced exit log of one vehicle.
func (g *VehicleBoardingGuard) Events(vehicleID string) []DismountEvent {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]DismountEvent, 0)
	v, ok := g.vehicles[vehicleID]
	if !ok {
		return out
	}
	out = append(out, v.events...)
	return out
}

// GlideState reports the driverless window of one vehicle.
func (g *VehicleBoardingGuard) GlideState(vehicleID string) (GlideInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	v, ok := g.vehicles[vehicleID]
	if !ok {
		return GlideInfo{}, ErrVehicleNotFound
	}
	info := GlideInfo{VehicleID: v.id}
	if v.glide != nil {
		info.Gliding = true
		info.DriverID = v.glide.driverID
		info.SinceSeq = v.glide.sinceSeq
	}
	return info, nil
}

func (g *VehicleBoardingGuard) glideExpired(v *vehicleState, now time.Time) bool {
	if v.glide == nil {
		return false
	}
	deadline := v.glide.started.Add(time.Duration(g.limits.GlideSeconds) * time.Second)
	return !now.Before(deadline)
}

// Tick advances the server clock and settles expired glide windows.
func (g *VehicleBoardingGuard) Tick(now time.Time) ([]DismountEvent, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	expired := make([]string, 0)
	for id, v := range g.vehicles {
		if g.glideExpired(v, now) {
			expired = append(expired, id)
		}
	}
	sort.Strings(expired)

	out := make([]DismountEvent, 0)
	for _, id := range expired {
		v := g.vehicles[id]
		for _, index := range v.order {
			seat := v.seats[index]
			if seat.player == "" {
				continue
			}
			playerID := seat.player
			seat.player = ""
			out = append(out, g.appendEvent(v, playerID, seat.index, ReasonGlideTimeout))
		}
		v.clearTransientState()
	}
	return out, nil
}

// Reconnect puts a dropped player back on the vehicle.
func (g *VehicleBoardingGuard) Reconnect(vehicleID, playerID string) (Boarding, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	v, ok := g.vehicles[vehicleID]
	if !ok {
		return Boarding{}, ErrVehicleNotFound
	}
	if blankPlayer(playerID) {
		return Boarding{}, ErrBadPlayer
	}

	if seat, onBoard := v.seatOfPlayer(playerID); onBoard {
		return g.boarding(v, playerID, seat.index, seat.role, true), nil
	}
	if v.spectators[playerID] {
		return g.boarding(v, playerID, -1, roleSpectator, true), nil
	}

	previous, ok := v.lastSeats[playerID]
	if !ok {
		return Boarding{}, ErrNotOnBoard
	}

	target := -1
	if seat, exists := v.seats[previous.index]; exists && seat.role == previous.role && seat.player == "" {
		target = previous.index
	} else if previous.role == RoleDriver {
		for _, index := range v.order {
			seat := v.seats[index]
			if seat.role == RolePassenger && seat.player == "" {
				if v.passengerCount() < g.limits.MaxPassengers {
					target = index
				}
				break
			}
		}
	}

	if target >= 0 {
		seat := v.seats[target]
		seat.player = playerID
		delete(v.lastSeats, playerID)
		delete(v.spectators, playerID)
		if seat.role == RoleDriver {
			v.glide = nil
		}
		return g.boarding(v, playerID, seat.index, seat.role, true), nil
	}

	v.spectators[playerID] = true
	return g.boarding(v, playerID, -1, roleSpectator, true), nil
}

const snapshotVersion = 1

type snapshotSeat struct {
	Index    int    `json:"index"`
	Role     string `json:"role"`
	PlayerID string `json:"player_id,omitempty"`
}

type snapshotSeatRef struct {
	Index int    `json:"index"`
	Role  string `json:"role"`
}

type snapshotGlide struct {
	DriverID        string `json:"driver_id"`
	SinceSeq        int64  `json:"since_seq"`
	StartedUnixNano int64  `json:"started_unix_nano"`
}

type snapshotVehicle struct {
	ID         string                     `json:"id"`
	Seats      []snapshotSeat             `json:"seats"`
	Events     []DismountEvent            `json:"events"`
	Glide      *snapshotGlide             `json:"glide,omitempty"`
	LastSeats  map[string]snapshotSeatRef `json:"last_seats,omitempty"`
	Spectators []string                   `json:"spectators,omitempty"`
}

type snapshotEnvelope struct {
	Version  int               `json:"version"`
	Seq      int64             `json:"seq"`
	Vehicles []snapshotVehicle `json:"vehicles"`
}

// Snapshot serialises every vehicle, seat and event.
func (g *VehicleBoardingGuard) Snapshot() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ids := make([]string, 0, len(g.vehicles))
	for id := range g.vehicles {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	envelope := snapshotEnvelope{
		Version:  snapshotVersion,
		Seq:      g.seq,
		Vehicles: make([]snapshotVehicle, 0, len(ids)),
	}
	for _, id := range ids {
		v := g.vehicles[id]
		sv := snapshotVehicle{
			ID:         v.id,
			Seats:      make([]snapshotSeat, 0, len(v.order)),
			Events:     make([]DismountEvent, len(v.events)),
			LastSeats:  make(map[string]snapshotSeatRef, len(v.lastSeats)),
			Spectators: make([]string, 0, len(v.spectators)),
		}
		copy(sv.Events, v.events)
		for _, index := range v.order {
			seat := v.seats[index]
			sv.Seats = append(sv.Seats, snapshotSeat{
				Index:    seat.index,
				Role:     seat.role,
				PlayerID: seat.player,
			})
		}
		for playerID, ref := range v.lastSeats {
			sv.LastSeats[playerID] = snapshotSeatRef{Index: ref.index, Role: ref.role}
		}
		for playerID := range v.spectators {
			sv.Spectators = append(sv.Spectators, playerID)
		}
		sort.Strings(sv.Spectators)
		if v.glide != nil {
			sv.Glide = &snapshotGlide{
				DriverID:        v.glide.driverID,
				SinceSeq:        v.glide.sinceSeq,
				StartedUnixNano: v.glide.started.UnixNano(),
			}
		}
		envelope.Vehicles = append(envelope.Vehicles, sv)
	}
	return json.Marshal(envelope)
}

func decodeSnapshot(data []byte) (snapshotEnvelope, error) {
	var envelope snapshotEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return snapshotEnvelope{}, ErrBadSnapshot
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return snapshotEnvelope{}, ErrBadSnapshot
	}
	if envelope.Version != snapshotVersion || envelope.Seq < 0 {
		return snapshotEnvelope{}, ErrBadSnapshot
	}
	return envelope, nil
}

func (g *VehicleBoardingGuard) buildRestoredState(envelope snapshotEnvelope) (map[string]*vehicleState, error) {
	restored := make(map[string]*vehicleState, len(envelope.Vehicles))
	for _, sv := range envelope.Vehicles {
		if _, duplicate := restored[sv.ID]; duplicate {
			return nil, ErrBadSnapshot
		}
		v := &vehicleState{
			id:         sv.ID,
			seats:      make(map[int]*seatSlot, len(sv.Seats)),
			order:      make([]int, 0, len(sv.Seats)),
			events:     make([]DismountEvent, 0, len(sv.Events)),
			lastSeats:  make(map[string]seatRef, len(sv.LastSeats)),
			spectators: make(map[string]bool, len(sv.Spectators)),
		}
		drivers := 0
		players := make(map[string]bool)
		for _, seat := range sv.Seats {
			if seat.Index < 0 || !validRole(seat.Role) {
				return nil, ErrBadSnapshot
			}
			if _, duplicate := v.seats[seat.Index]; duplicate {
				return nil, ErrBadSnapshot
			}
			if seat.PlayerID != "" {
				if blankPlayer(seat.PlayerID) || players[seat.PlayerID] {
					return nil, ErrBadSnapshot
				}
				players[seat.PlayerID] = true
			}
			if seat.Role == RoleDriver {
				drivers++
			}
			v.seats[seat.Index] = &seatSlot{index: seat.Index, role: seat.Role, player: seat.PlayerID}
			v.order = append(v.order, seat.Index)
		}
		if drivers != 1 {
			return nil, ErrBadSnapshot
		}
		sort.Ints(v.order)

		previousSeq := int64(0)
		for _, event := range sv.Events {
			if event.VehicleID != sv.ID || blankPlayer(event.PlayerID) || event.Seq <= previousSeq || event.Seq > envelope.Seq {
				return nil, ErrBadSnapshot
			}
			if _, exists := v.seats[event.SeatIndex]; !exists {
				return nil, ErrBadSnapshot
			}
			previousSeq = event.Seq
			v.events = append(v.events, event)
		}

		for playerID, ref := range sv.LastSeats {
			if blankPlayer(playerID) {
				return nil, ErrBadSnapshot
			}
			seat, exists := v.seats[ref.Index]
			if !exists || seat.role != ref.Role {
				return nil, ErrBadSnapshot
			}
			v.lastSeats[playerID] = seatRef{index: ref.Index, role: ref.Role}
		}
		for _, playerID := range sv.Spectators {
			if blankPlayer(playerID) || players[playerID] || v.spectators[playerID] {
				return nil, ErrBadSnapshot
			}
			v.spectators[playerID] = true
		}

		if sv.Glide != nil {
			if blankPlayer(sv.Glide.DriverID) || sv.Glide.SinceSeq < 0 || sv.Glide.SinceSeq > envelope.Seq || sv.Glide.StartedUnixNano <= 0 {
				return nil, ErrBadSnapshot
			}
			driverSeat := v.driverSeat()
			if driverSeat == nil || driverSeat.player != "" || v.occupiedCount() == 0 {
				return nil, ErrBadSnapshot
			}
			v.glide = &glideWindow{
				driverID: sv.Glide.DriverID,
				sinceSeq: sv.Glide.SinceSeq,
				started:  time.Unix(0, sv.Glide.StartedUnixNano),
			}
		}
		restored[sv.ID] = v
	}
	return restored, nil
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *VehicleBoardingGuard) Restore(data []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	envelope, err := decodeSnapshot(data)
	if err != nil {
		return err
	}
	restored, err := g.buildRestoredState(envelope)
	if err != nil {
		return err
	}
	g.vehicles = restored
	g.seq = envelope.Seq
	return nil
}
