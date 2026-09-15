package main

// The CEC follower: what bdplay says on the bus while it is the playback
// device, and how the TV's remote turns into Keys.
//
// The kernel answers the identity questions itself (physical address, vendor
// ID, CEC version, OSD name — see cec_linux.go). Everything else directed at
// our logical address lands here, and per the spec anything we do not
// understand gets a <Feature Abort> back, otherwise the TV waits out a
// timeout on every unsupported request and some models mark the device
// unresponsive.

import (
	"sync"
	"time"
)

// cecAdapter is the device as the follower sees it. cecDevice implements it;
// tests substitute a recorder.
type cecAdapter interface {
	Claim(osdName string, timeout time.Duration) error
	Transmit(to, opcode uint8, params ...uint8) error
	Run(stop <-chan struct{}, onMsg func(cecMsg))
	LogAddr() uint8
	PhysAddr() uint16
	Close() error
}

// cecFollower turns bus traffic into keys and answers the status requests a
// TV sends a playback device.
type cecFollower struct {
	dev cecAdapter
	log *Logger

	// onKey receives every recognised remote key, on the adapter goroutine.
	onKey func(Key)
	// deckStatus reports what we are doing, for <Give Deck Status>.
	deckStatus func() uint8

	mu     sync.Mutex
	active bool // we consider ourselves the active source
}

func newCECFollower(dev cecAdapter, log *Logger, onKey func(Key), deckStatus func() uint8) *cecFollower {
	return &cecFollower{dev: dev, log: log, onKey: onKey, deckStatus: deckStatus}
}

// announce broadcasts <Active Source>: the TV switches to our input if it is
// elsewhere, and from then on routes its remote to us. Sent on entry and
// whenever the TV asks who is active.
func (f *cecFollower) announce() {
	pa := f.dev.PhysAddr()
	if pa == cecPhysAddrInvalid {
		f.log.Warn("cec: not announcing as active source: physical address unknown (no EDID?)")
		return
	}
	f.mu.Lock()
	f.active = true
	f.mu.Unlock()
	if err := f.dev.Transmit(cecLogAddrBroadcast, cecMsgActiveSource, uint8(pa>>8), uint8(pa)); err != nil {
		f.log.Warn("cec: %v", err)
	}
}

// handle processes one received message.
func (f *cecFollower) handle(m cecMsg) {
	op, ok := m.opcode()
	if !ok {
		return // a bare polling message
	}
	from := m.initiator()
	broadcast := m.isBroadcast()

	switch op {
	case cecMsgUserControlPressed:
		cmd, ok := m.operand(0)
		if !ok {
			return
		}
		if k := keyFromUICommand(cmd); k != KeyNone {
			f.onKey(k)
		} else {
			f.log.Info("cec: ignoring user control 0x%02x from %d", cmd, from)
		}
	case cecMsgUserControlReleased:
		// Presses are acted on immediately; releases carry nothing for us.
	case cecMsgDeckControl:
		if mode, ok := m.operand(0); ok {
			if k := keyFromDeckControl(mode); k != KeyNone {
				f.onKey(k)
			}
		}
	case cecMsgPlay:
		if mode, ok := m.operand(0); ok {
			if k := keyFromPlay(mode); k != KeyNone {
				f.onKey(k)
			}
		}
	case cecMsgGiveDeckStatus:
		f.reply(from, cecMsgDeckStatus, f.deckStatus())
	case cecMsgMenuRequest:
		// Some TVs will not forward remote keys until the device says its
		// menu is active. It always is: the browser is a menu.
		state := uint8(cecOpMenuStateActivated)
		if t, ok := m.operand(0); ok && t == cecOpMenuRequestDeactivate {
			state = cecOpMenuStateDeactivated
		}
		f.reply(from, cecMsgMenuStatus, state)
	case cecMsgGiveDevicePowerStatus:
		f.reply(from, cecMsgReportPowerStatus, cecOpPowerStatusOn)
	case cecMsgRequestActiveSource:
		f.mu.Lock()
		active := f.active
		f.mu.Unlock()
		if active {
			f.announce()
		}
	case cecMsgSetStreamPath:
		// The TV switched to a physical address; if it is ours, confirm.
		hi, ok1 := m.operand(0)
		lo, ok2 := m.operand(1)
		if ok1 && ok2 && uint16(hi)<<8|uint16(lo) == f.dev.PhysAddr() {
			f.announce()
		}
	case cecMsgActiveSource:
		// Somebody else became the active source; the TV will stop routing
		// keys to us. Nothing to do but remember it.
		if hi, ok := m.operand(0); ok {
			if lo, ok := m.operand(1); ok && uint16(hi)<<8|uint16(lo) != f.dev.PhysAddr() {
				f.mu.Lock()
				f.active = false
				f.mu.Unlock()
			}
		}
	case cecMsgStandby, cecMsgRoutingChange, cecMsgRoutingInformation,
		cecMsgReportPhysicalAddr, cecMsgDeviceVendorID, cecMsgSetMenuLanguage,
		cecMsgReportPowerStatus, cecMsgCecVersion, cecMsgReportFeatures,
		cecMsgFeatureAbort, cecMsgInactiveSource, cecMsgSetOSDName, cecMsgDeckStatus,
		cecMsgMenuStatus:
		// Informational, or replies to things we did not ask.
	default:
		if broadcast || from == cecLogAddrUnregistered {
			return
		}
		f.log.Info("cec: %s from %d not supported, aborting", cecOpcodeName(op), from)
		f.reply(from, cecMsgFeatureAbort, op, cecOpAbortUnrecognizedOp)
	}
}

func (f *cecFollower) reply(to, opcode uint8, params ...uint8) {
	if to == cecLogAddrUnregistered {
		return // cannot address the unregistered initiator
	}
	if err := f.dev.Transmit(to, opcode, params...); err != nil {
		f.log.Warn("cec: %v", err)
	}
}
