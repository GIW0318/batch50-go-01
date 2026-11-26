// Package task guards the seats of every spawned vehicle: who sits where,
// who holds the driving right, and which dismount events have been emitted.
package task

import (
	"errors"
	"time"
)

// errNotImplemented marks the behaviours that still have to be written.
var errNotImplemented = errors.New("not implemented")

// Seat roles. Every vehicle has exactly one driver seat.
const (
	RoleDriver    = "driver"
	RoleGunner    = "gunner"
	RolePassenger = "passenger"
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

// VehicleBoardingGuard owns the per vehicle seat tables.
type VehicleBoardingGuard struct {
	limits Limits
}

// NewVehicleBoardingGuard builds a guard with the given caps.
func NewVehicleBoardingGuard(limits Limits) *VehicleBoardingGuard {
	return &VehicleBoardingGuard{limits: limits}
}

// Spawn registers one vehicle and its seat layout.
func (g *VehicleBoardingGuard) Spawn(vehicleID string, seats []SeatSpec) error {
	return errNotImplemented
}

// Board places a player into one seat.
func (g *VehicleBoardingGuard) Board(vehicleID, playerID string, seatIndex int) (Boarding, error) {
	return Boarding{}, errNotImplemented
}

// Leave removes a player from the vehicle.
func (g *VehicleBoardingGuard) Leave(vehicleID, playerID string) error {
	return errNotImplemented
}

// TransferDriver hands the driving right to another occupant of the same vehicle.
func (g *VehicleBoardingGuard) TransferDriver(vehicleID, fromPlayer, toPlayer string) error {
	return errNotImplemented
}

// ForceDismount clears every seat at once and returns the pushed players.
func (g *VehicleBoardingGuard) ForceDismount(vehicleID string, reason DismountReason) ([]string, error) {
	return nil, errNotImplemented
}

// SeatOf reports where one player sits.
func (g *VehicleBoardingGuard) SeatOf(vehicleID, playerID string) (SeatState, error) {
	return SeatState{}, errNotImplemented
}

// Occupants lists the taken seats by seat index.
func (g *VehicleBoardingGuard) Occupants(vehicleID string) []SeatState {
	return nil
}

// Events returns a copy of the forced exit log of one vehicle.
func (g *VehicleBoardingGuard) Events(vehicleID string) []DismountEvent {
	return nil
}

// GlideState reports the driverless window of one vehicle.
func (g *VehicleBoardingGuard) GlideState(vehicleID string) (GlideInfo, error) {
	return GlideInfo{}, errNotImplemented
}

// Tick advances the server clock and settles expired glide windows.
func (g *VehicleBoardingGuard) Tick(now time.Time) ([]DismountEvent, error) {
	return nil, errNotImplemented
}

// Reconnect puts a dropped player back on the vehicle.
func (g *VehicleBoardingGuard) Reconnect(vehicleID, playerID string) (Boarding, error) {
	return Boarding{}, errNotImplemented
}

// Snapshot serialises every vehicle, seat and event.
func (g *VehicleBoardingGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *VehicleBoardingGuard) Restore(data []byte) error {
	return errNotImplemented
}
