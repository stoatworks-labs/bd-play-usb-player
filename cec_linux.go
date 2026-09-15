//go:build linux

package main

// /dev/cec0 on the device: open, claim a Playback logical address, and pump
// messages. Pure syscalls — no cgo, no libcec — so the static arm64 build
// stays the way it is.
//
// Ownership of the adapter is the whole point. BirdDog's decoder (PPApp) opens
// the same node as an *exclusive* follower for its own source-list OSD, so
// while it runs nobody else can follow. bdplay only opens the device after it
// has stopped BirdDogRunner to take the display, and closes it again before
// starting BirdDogRunner back up — PPApp's CEC_S_MODE would otherwise fail
// with EBUSY and its OSD would be dead until the next restart.

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type cecDevice struct {
	fd  int
	log *Logger

	mu       sync.Mutex
	logAddr  uint8
	physAddr uint16
	osdName  string
}

// openCECDevice opens the adapter as a plain (non-exclusive) follower and
// initiator. Non-exclusive because there is nothing to defend against once
// PPApp is stopped, and passthrough off so the kernel keeps answering the
// housekeeping requests it knows how to answer (physical address, vendor ID,
// CEC version, OSD name).
func openCECDevice(path string, log *Logger) (*cecDevice, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	d := &cecDevice{fd: fd, log: log, logAddr: cecLogAddrInvalid, physAddr: cecPhysAddrInvalid}

	var caps cecCaps
	if err := d.ioctl(cecAdapGCaps, unsafe.Pointer(&caps)); err != nil {
		d.closeFD()
		return nil, fmt.Errorf("CEC_ADAP_G_CAPS: %w", err)
	}
	need := uint32(cecCapLogAddrs | cecCapTransmit)
	if caps.Capabilities&need != need {
		d.closeFD()
		return nil, fmt.Errorf("%s (%s) cannot claim addresses or transmit (caps 0x%x)",
			cstr(caps.Driver[:]), cstr(caps.Name[:]), caps.Capabilities)
	}
	log.Info("cec: %s via %s, caps 0x%x", cstr(caps.Name[:]), cstr(caps.Driver[:]), caps.Capabilities)

	mode := uint32(cecModeInitiator | cecModeFollower)
	if err := d.ioctl(cecSMode, unsafe.Pointer(&mode)); err != nil {
		d.closeFD()
		return nil, fmt.Errorf("CEC_S_MODE: %w (is BirdDog's decoder still running?)", err)
	}
	return d, nil
}

// Claim configures the adapter as a CEC 1.4 Playback device with the given
// OSD name, and waits for the kernel to finish the logical-address poll.
//
// The adapter usually still carries PPApp's configuration at this point, and
// the kernel refuses a new configuration on a configured adapter — so clear
// first, exactly as PPApp itself does on startup.
func (d *cecDevice) Claim(osdName string, timeout time.Duration) error {
	var clear cecLogAddrs
	if err := d.ioctl(cecAdapSLogAddrs, unsafe.Pointer(&clear)); err != nil {
		return fmt.Errorf("CEC_ADAP_S_LOG_ADDRS (clear): %w", err)
	}

	la := cecLogAddrs{
		CecVersion:  cecOpCecVersion14,
		NumLogAddrs: 1,
		VendorID:    cecVendorIDNone,
		// Fall back to the unregistered address rather than failing if all
		// three playback addresses are taken by other kit on the bus. The TV
		// cannot address us then, but broadcasts still arrive.
		Flags: cecLogAddrsFlAllowUnregFallback,
	}
	la.LogAddr[0] = cecLogAddrUnregistered
	la.PrimaryDeviceType[0] = cecOpPrimDevtypePlayback
	la.LogAddrType[0] = cecLogAddrTypePlayback
	la.AllDeviceTypes[0] = cecOpAllDevtypePlayback
	copy(la.OsdName[:len(la.OsdName)-1], osdName)
	if err := d.ioctl(cecAdapSLogAddrs, unsafe.Pointer(&la)); err != nil {
		return fmt.Errorf("CEC_ADAP_S_LOG_ADDRS: %w", err)
	}
	d.mu.Lock()
	d.osdName = osdName
	d.mu.Unlock()

	// The claim runs on a kernel thread (it polls the bus); with O_NONBLOCK
	// the ioctl returns at once and the result arrives as a state-change
	// event. Poll for it rather than wiring the event loop up early.
	deadline := time.Now().Add(timeout)
	for {
		d.refreshState()
		if d.LogAddr() != cecLogAddrInvalid {
			d.log.Info("cec: claimed logical address %d as %q, physical address %s",
				d.LogAddr(), osdName, physAddrString(d.PhysAddr()))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no logical address after %s (physical address %s)", timeout, physAddrString(d.PhysAddr()))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// refreshState re-reads the adapter's addresses.
func (d *cecDevice) refreshState() {
	var la cecLogAddrs
	var pa uint16 = cecPhysAddrInvalid
	_ = d.ioctl(cecAdapGPhysAddr, unsafe.Pointer(&pa))
	if err := d.ioctl(cecAdapGLogAddrs, unsafe.Pointer(&la)); err != nil {
		return
	}
	d.mu.Lock()
	d.physAddr = pa
	if la.NumLogAddrs > 0 {
		d.logAddr = la.LogAddr[0]
	} else {
		d.logAddr = cecLogAddrInvalid
	}
	d.mu.Unlock()
}

func (d *cecDevice) LogAddr() uint8 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.logAddr
}

func (d *cecDevice) PhysAddr() uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.physAddr
}

// Transmit queues a message from our logical address. Non-blocking: the
// result comes back as a message with a tx_status if anyone cares, which for
// replies to a TV we do not — the TV re-asks if it must.
func (d *cecDevice) Transmit(to, opcode uint8, params ...uint8) error {
	from := d.LogAddr()
	if from == cecLogAddrInvalid {
		from = cecLogAddrUnregistered
	}
	m := newCECMsg(from, to, opcode, params...)
	if err := d.ioctl(cecTransmit, unsafe.Pointer(&m)); err != nil {
		return fmt.Errorf("CEC_TRANSMIT %s: %w", cecOpcodeName(opcode), err)
	}
	return nil
}

// Run pumps the adapter until stop is closed, handing every received message
// to onMsg on this goroutine. Events only update the cached addresses.
func (d *cecDevice) Run(stop <-chan struct{}, onMsg func(cecMsg)) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		readable, urgent, err := d.wait(200 * time.Millisecond)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			d.log.Warn("cec: select: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if urgent {
			d.drainEvents()
		}
		if readable {
			d.drainMessages(onMsg)
		}
	}
}

func (d *cecDevice) drainEvents() {
	for {
		var ev cecEvent
		if err := d.ioctl(cecDQEvent, unsafe.Pointer(&ev)); err != nil {
			return // EAGAIN: queue empty
		}
		switch ev.Event {
		case cecEventStateChange:
			pa := uint16(ev.Raw[0] & 0xffff)
			mask := uint16(ev.Raw[0] >> 16)
			d.log.Info("cec: state change: physical address %s, logical address mask 0x%04x", physAddrString(pa), mask)
			d.refreshState()
		case cecEventLostMsgs:
			d.log.Warn("cec: %d message(s) lost", ev.Raw[0])
		}
	}
}

func (d *cecDevice) drainMessages(onMsg func(cecMsg)) {
	for {
		var m cecMsg
		if err := d.ioctl(cecReceive, unsafe.Pointer(&m)); err != nil {
			return
		}
		if m.RxStatus&cecRxStatusOK == 0 && m.TxStatus != 0 {
			// Our own transmit's result, not a received message.
			if m.TxStatus&cecTxStatusOK == 0 {
				if op, ok := m.opcode(); ok {
					d.log.Warn("cec: %s to %d not delivered (tx_status 0x%02x)", cecOpcodeName(op), m.destination(), m.TxStatus)
				}
			}
			continue
		}
		onMsg(m)
	}
}

// Close clears our logical address and closes the adapter. Clearing matters:
// PPApp reconfigures on start anyway, but if it does not come back the bus
// should not keep answering as a USB player that no longer exists.
func (d *cecDevice) Close() error {
	var clear cecLogAddrs
	_ = d.ioctl(cecAdapSLogAddrs, unsafe.Pointer(&clear))
	return d.closeFD()
}

func (d *cecDevice) closeFD() error {
	if d.fd < 0 {
		return nil
	}
	err := syscall.Close(d.fd)
	d.fd = -1
	return err
}

func (d *cecDevice) ioctl(req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(d.fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// wait blocks until a message (readable) or an event (urgent) is pending, or
// the timeout passes. The CEC framework signals pending events as an
// exceptional condition, which is what the third fd set is.
func (d *cecDevice) wait(timeout time.Duration) (readable, urgent bool, err error) {
	var r, e syscall.FdSet
	fdSet(&r, d.fd)
	fdSet(&e, d.fd)
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	n, err := syscall.Select(d.fd+1, &r, nil, &e, &tv)
	if err != nil {
		return false, false, err
	}
	if n == 0 {
		return false, false, nil
	}
	return fdIsSet(&r, d.fd), fdIsSet(&e, d.fd), nil
}

func fdSet(s *syscall.FdSet, fd int) {
	s.Bits[fd/64] |= 1 << (uint(fd) % 64)
}

func fdIsSet(s *syscall.FdSet, fd int) bool {
	return s.Bits[fd/64]&(1<<(uint(fd)%64)) != 0
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// cecDeviceExists is a cheap pre-check so the log can say why the remote is
// not available without an open attempt.
func cecDeviceExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// openCEC is the platform hook the session uses; see cec_other.go.
func openCEC(path string, log *Logger) (cecAdapter, error) {
	d, err := openCECDevice(path, log)
	if err != nil {
		return nil, err
	}
	return d, nil
}
