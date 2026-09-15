package main

import (
	"sync"
	"testing"
	"time"
	"unsafe"
)

// The ioctl numbers the stock decoder's own CEC driver is seen using, read
// out of PPApp 1.0.34 (immediate operands of its ioctl calls). If these ever
// differ from ours, the struct layouts in cec.go are wrong for the device.
func TestCECIoctlNumbersMatchTheDevice(t *testing.T) {
	cases := map[string][2]uintptr{
		"CEC_ADAP_G_CAPS":      {cecAdapGCaps, 0xc04c6100},
		"CEC_ADAP_G_PHYS_ADDR": {cecAdapGPhysAddr, 0x80026101},
		"CEC_ADAP_G_LOG_ADDRS": {cecAdapGLogAddrs, 0x805c6103},
		"CEC_ADAP_S_LOG_ADDRS": {cecAdapSLogAddrs, 0xc05c6104},
		"CEC_TRANSMIT":         {cecTransmit, 0xc0386105},
		"CEC_RECEIVE":          {cecReceive, 0xc0386106},
		"CEC_DQEVENT":          {cecDQEvent, 0xc0506107},
		"CEC_S_MODE":           {cecSMode, 0x40046109},
	}
	for name, c := range cases {
		if c[0] != c[1] {
			t.Errorf("%s = %#x, want %#x", name, c[0], c[1])
		}
	}
}

func TestCECStructSizes(t *testing.T) {
	if s := unsafe.Sizeof(cecCaps{}); s != 76 {
		t.Errorf("cec_caps is %d bytes, want 76", s)
	}
	if s := unsafe.Sizeof(cecLogAddrs{}); s != 92 {
		t.Errorf("cec_log_addrs is %d bytes, want 92", s)
	}
	if s := unsafe.Sizeof(cecMsg{}); s != 56 {
		t.Errorf("cec_msg is %d bytes, want 56", s)
	}
	if s := unsafe.Sizeof(cecEvent{}); s != 80 {
		t.Errorf("cec_event is %d bytes, want 80", s)
	}
	// Field offsets the claim relies on.
	var la cecLogAddrs
	base := uintptr(unsafe.Pointer(&la))
	if off := uintptr(unsafe.Pointer(&la.OsdName)) - base; off != 16 {
		t.Errorf("osd_name at %d, want 16", off)
	}
	if off := uintptr(unsafe.Pointer(&la.PrimaryDeviceType)) - base; off != 31 {
		t.Errorf("primary_device_type at %d, want 31", off)
	}
	if off := uintptr(unsafe.Pointer(&la.LogAddrType)) - base; off != 35 {
		t.Errorf("log_addr_type at %d, want 35", off)
	}
	if off := uintptr(unsafe.Pointer(&la.AllDeviceTypes)) - base; off != 39 {
		t.Errorf("all_device_types at %d, want 39", off)
	}
}

func TestCECMsgHelpers(t *testing.T) {
	m := newCECMsg(4, 0, cecMsgUserControlPressed, cecUICmdUp)
	if m.Len != 3 || m.Msg[0] != 0x40 || m.Msg[1] != 0x44 || m.Msg[2] != 0x01 {
		t.Fatalf("unexpected encoding %+v", m)
	}
	if m.initiator() != 4 || m.destination() != 0 || m.isBroadcast() {
		t.Errorf("addresses wrong: %d -> %d", m.initiator(), m.destination())
	}
	op, ok := m.operand(0)
	if !ok || op != cecUICmdUp {
		t.Errorf("operand(0) = %#x,%v", op, ok)
	}
	if _, ok := m.operand(1); ok {
		t.Error("operand(1) should be absent")
	}
	poll := cecMsg{Len: 1}
	if _, ok := poll.opcode(); ok {
		t.Error("a polling message has no opcode")
	}
	b := newCECMsg(4, cecLogAddrBroadcast, cecMsgActiveSource, 0x10, 0x00)
	if !b.isBroadcast() {
		t.Error("destination 15 is broadcast")
	}
	if physAddrString(0x1000) != "1.0.0.0" || physAddrString(0x2340) != "2.3.4.0" {
		t.Error("physAddrString formatting")
	}
}

// fakeCEC records what the follower transmits and lets tests inject traffic.
type fakeCEC struct {
	mu     sync.Mutex
	sent   []cecMsg
	closed bool
	la     uint8
	pa     uint16
	name   string
	onMsg  func(cecMsg)
	claims int
}

func newFakeCEC() *fakeCEC { return &fakeCEC{la: 4, pa: 0x1000} }

func (f *fakeCEC) Claim(osdName string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.name = osdName
	f.claims++
	return nil
}

func (f *fakeCEC) Transmit(to, opcode uint8, params ...uint8) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, newCECMsg(f.la, to, opcode, params...))
	return nil
}

func (f *fakeCEC) Run(stop <-chan struct{}, onMsg func(cecMsg)) {
	f.mu.Lock()
	f.onMsg = onMsg
	f.mu.Unlock()
	<-stop
}

func (f *fakeCEC) LogAddr() uint8   { return f.la }
func (f *fakeCEC) PhysAddr() uint16 { return f.pa }
func (f *fakeCEC) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// inject delivers a message as if the bus had received it. Waits for Run to
// have installed the handler.
func (f *fakeCEC) inject(t *testing.T, m cecMsg) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		h := f.onMsg
		f.mu.Unlock()
		if h != nil {
			h(m)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("CEC loop never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fakeCEC) transmitted() []cecMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]cecMsg, len(f.sent))
	copy(out, f.sent)
	return out
}

func TestFollowerTurnsRemoteKeysIntoKeys(t *testing.T) {
	dev := newFakeCEC()
	var got []Key
	f := newCECFollower(dev, NewLogger(), func(k Key) { got = append(got, k) }, func() uint8 { return cecOpDeckInfoStop })

	f.handle(newCECMsg(0, 4, cecMsgUserControlPressed, cecUICmdDown))
	f.handle(newCECMsg(0, 4, cecMsgUserControlReleased))
	f.handle(newCECMsg(0, 4, cecMsgUserControlPressed, cecUICmdSelect))
	f.handle(newCECMsg(0, 4, cecMsgUserControlPressed, cecUICmdF2Red))
	f.handle(newCECMsg(0, 4, cecMsgUserControlPressed, 0x41)) // volume up: the TV's business
	f.handle(newCECMsg(0, 4, cecMsgDeckControl, cecOpDeckCtlStop))
	f.handle(newCECMsg(0, 4, cecMsgPlay, cecOpPlayModePlayFwd))

	want := []Key{KeyDown, KeySelect, KeyRed, KeyStop, KeyPlay}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %v, want %v", i, got[i], want[i])
		}
	}
	if n := len(dev.transmitted()); n != 0 {
		t.Errorf("keys should not transmit anything, sent %d", n)
	}
}

func TestFollowerAnswersStatusRequests(t *testing.T) {
	dev := newFakeCEC()
	status := uint8(cecOpDeckInfoPlay)
	f := newCECFollower(dev, NewLogger(), func(Key) {}, func() uint8 { return status })

	f.handle(newCECMsg(0, 4, cecMsgGiveDeckStatus, 1))
	f.handle(newCECMsg(0, 4, cecMsgMenuRequest, cecOpMenuRequestActivate))
	f.handle(newCECMsg(0, 4, cecMsgGiveDevicePowerStatus))
	f.handle(newCECMsg(0, 4, cecMsgSetOSDString, 0, 'h', 'i'))                         // unsupported, directed
	f.handle(newCECMsg(0, cecLogAddrBroadcast, cecMsgRoutingChange, 0x10, 0, 0x20, 0)) // broadcast: silence
	f.handle(newCECMsg(cecLogAddrUnregistered, 4, cecMsgVendorCommand, 1))             // unregistered initiator: silence

	sent := dev.transmitted()
	if len(sent) != 4 {
		t.Fatalf("sent %d messages, want 4: %+v", len(sent), sent)
	}
	check := func(i int, opcode uint8, params ...uint8) {
		t.Helper()
		m := sent[i]
		if m.destination() != 0 || m.Msg[1] != opcode {
			t.Errorf("reply %d = %s to %d, want %s to 0", i, cecOpcodeName(m.Msg[1]), m.destination(), cecOpcodeName(opcode))
		}
		for j, p := range params {
			if got, _ := m.operand(j); got != p {
				t.Errorf("reply %d operand %d = %#x, want %#x", i, j, got, p)
			}
		}
	}
	check(0, cecMsgDeckStatus, cecOpDeckInfoPlay)
	check(1, cecMsgMenuStatus, cecOpMenuStateActivated)
	check(2, cecMsgReportPowerStatus, cecOpPowerStatusOn)
	check(3, cecMsgFeatureAbort, cecMsgSetOSDString, cecOpAbortUnrecognizedOp)
}

func TestFollowerAnnouncesWhenTheTVSwitchesToUs(t *testing.T) {
	dev := newFakeCEC()
	f := newCECFollower(dev, NewLogger(), func(Key) {}, func() uint8 { return cecOpDeckInfoStop })

	f.handle(newCECMsg(0, cecLogAddrBroadcast, cecMsgSetStreamPath, 0x20, 0x00)) // someone else's input
	if n := len(dev.transmitted()); n != 0 {
		t.Fatalf("should not announce for another physical address, sent %d", n)
	}
	f.handle(newCECMsg(0, cecLogAddrBroadcast, cecMsgSetStreamPath, 0x10, 0x00)) // ours
	sent := dev.transmitted()
	if len(sent) != 1 || sent[0].Msg[1] != cecMsgActiveSource || !sent[0].isBroadcast() {
		t.Fatalf("expected a broadcast Active Source, got %+v", sent)
	}
	if hi, _ := sent[0].operand(0); hi != 0x10 {
		t.Errorf("Active Source carries %#x, want our 1.0.0.0", hi)
	}
	// Once active, Request Active Source is answered; after another device
	// claims it, it is not.
	f.handle(newCECMsg(0, cecLogAddrBroadcast, cecMsgRequestActiveSource))
	if len(dev.transmitted()) != 2 {
		t.Error("Request Active Source should be answered while active")
	}
	f.handle(newCECMsg(8, cecLogAddrBroadcast, cecMsgActiveSource, 0x20, 0x00))
	f.handle(newCECMsg(0, cecLogAddrBroadcast, cecMsgRequestActiveSource))
	if len(dev.transmitted()) != 2 {
		t.Error("should stay quiet once another device is the active source")
	}
}

func TestKeyNamesRoundTrip(t *testing.T) {
	for k, n := range keyNames {
		got, ok := ParseKey(n)
		if !ok || got != k {
			t.Errorf("ParseKey(%q) = %v,%v want %v", n, got, ok, k)
		}
	}
	for alias, want := range map[string]Key{"OK": KeySelect, "enter": KeySelect, "exit": KeyBack, "ff": KeyNext, "rew": KeyPrev} {
		if got, ok := ParseKey(alias); !ok || got != want {
			t.Errorf("ParseKey(%q) = %v,%v want %v", alias, got, ok, want)
		}
	}
	if _, ok := ParseKey("volume"); ok {
		t.Error("volume is not a key of ours")
	}
	if KeyNone.String() != "key(0)" || KeyRed.String() != "red" {
		t.Error("Key.String")
	}
}

func TestUICommandMapping(t *testing.T) {
	cases := map[uint8]Key{
		cecUICmdSelect: KeySelect, cecUICmdEnter: KeySelect,
		cecUICmdExit: KeyBack, cecUICmdRootMenu: KeyBack,
		cecUICmdChannelUp: KeyPageUp, cecUICmdPageDown: KeyPageDown,
		cecUICmdPlay: KeyPlay, cecUICmdStop: KeyStop, cecUICmdPause: KeyPause,
		cecUICmdForward: KeyNext, cecUICmdFastForward: KeyNext,
		cecUICmdBackward: KeyPrev, cecUICmdRewind: KeyPrev,
		cecUICmdF1Blue: KeyBlue, cecUICmdF2Red: KeyRed, cecUICmdF3Green: KeyGreen, cecUICmdF4Yellow: KeyYellow,
		0x41: KeyNone, 0x40: KeyNone, cecUICmdNumber0: KeyNone,
	}
	for cmd, want := range cases {
		if got := keyFromUICommand(cmd); got != want {
			t.Errorf("keyFromUICommand(%#x) = %v, want %v", cmd, got, want)
		}
	}
}
