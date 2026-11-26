// Package task relays node logs to a central collector using RFC 5424
// framing, peer verification and a bounded on-disk queue.
package task

import (
	"context"
	"errors"
	"time"
)

// errNotImplemented is returned by every behaviour that still needs work.
var errNotImplemented = errors.New("not implemented")

// Framing modes supported by the relay.
const (
	FramingOctetCounted   = "octet_counted"
	FramingNonTransparent = "non_transparent"
)

// Errors surfaced by the relay.
var (
	ErrBadPolicy     = errors.New("relay policy is invalid")
	ErrBadMessage    = errors.New("message is invalid")
	ErrQueueFull     = errors.New("relay queue is full")
	ErrPeerUntrusted = errors.New("peer certificate chain is not trusted")
	ErrSNIMismatch   = errors.New("peer certificate does not match the server name")
	ErrBadSnapshot   = errors.New("snapshot payload invalid")
)

// Message is one log record handed to the relay.
type Message struct {
	Source      string
	Priority    int
	AppName     string
	MsgID       string
	Timestamp   time.Time
	Body        []byte
	OriginalLen int
}

// RelayPolicy is the operator configuration of the relay.
type RelayPolicy struct {
	Framing             string
	MaxBodyBytes        int
	QueueMaxMessages    int
	QueueMaxBytes       int
	HighPrioritySources []string
	HighPriorityReserve int
	AllowedCAs          []string
	ServerName          string
}

// PeerState is the result of the last certificate verification.
type PeerState struct {
	Verified   bool
	ServerName string
	CA         string
	Generation int
}

// CertInfo is one certificate presented by the collector.
type CertInfo struct {
	CA       string
	Subject  string
	DNSNames []string
	NotAfter time.Time
}

// SyslogRelayGuard owns the queue, the framing and the peer state.
type SyslogRelayGuard struct {
	cfg RelayPolicy
}

// NewSyslogRelayGuard builds a relay with the given policy.
func NewSyslogRelayGuard(p RelayPolicy) (*SyslogRelayGuard, error) {
	return nil, errNotImplemented
}

// Write normalises and queues one message.
func (g *SyslogRelayGuard) Write(msg Message) error {
	return errNotImplemented
}

// Flush moves the pending queue to the in-flight batch.
func (g *SyslogRelayGuard) Flush(ctx context.Context) error {
	return errNotImplemented
}

// Peer reports the last verified peer.
func (g *SyslogRelayGuard) Peer() PeerState {
	return PeerState{}
}

// SetPolicy swaps the relay policy without dropping queued messages.
func (g *SyslogRelayGuard) SetPolicy(p RelayPolicy) error {
	return errNotImplemented
}

// Normalize validates one message and truncates an oversized body.
func (g *SyslogRelayGuard) Normalize(msg Message) (Message, error) {
	return Message{}, errNotImplemented
}

// Frame renders one message with octet counted framing.
func (g *SyslogRelayGuard) Frame(msg Message) ([]byte, error) {
	return nil, errNotImplemented
}

// FrameNonTransparent renders one message with newline framing.
func (g *SyslogRelayGuard) FrameNonTransparent(msg Message) ([]byte, error) {
	return nil, errNotImplemented
}

// VerifyPeer checks a presented chain against the allowed CAs and SNI.
func (g *SyslogRelayGuard) VerifyPeer(chain []CertInfo, serverName string) (PeerState, error) {
	return PeerState{}, errNotImplemented
}

// QueueDepth reports the pending message count and body bytes.
func (g *SyslogRelayGuard) QueueDepth() (int, int) {
	return 0, 0
}

// Pending returns a copy of the pending queue, oldest first.
func (g *SyslogRelayGuard) Pending() []Message {
	return nil
}

// Inflight returns a copy of the unacknowledged batch, oldest first.
func (g *SyslogRelayGuard) Inflight() []Message {
	return nil
}

// Ack removes one delivered message from the in-flight batch.
func (g *SyslogRelayGuard) Ack(msgID string) error {
	return errNotImplemented
}

// Snapshot serialises the queue, the in-flight batch and the id watermark.
func (g *SyslogRelayGuard) Snapshot() ([]byte, error) {
	return nil, errNotImplemented
}

// Restore replaces the whole state with a snapshot, all or nothing.
func (g *SyslogRelayGuard) Restore(data []byte) error {
	return errNotImplemented
}
