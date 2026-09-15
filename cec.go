package main

// The Linux CEC UAPI (linux/cec.h), the subset a playback device needs.
//
// Kept in a portable file so the numbers and the message helpers are unit
// tested on the build host; the device I/O is in cec_linux.go. Everything
// here mirrors the header byte for byte — the ioctl numbers below are the
// ones the stock decoder (PPApp) is seen calling in its own CEC driver, which
// is the best confirmation available that this firmware's 4.4 kernel carries
// the mainline layout.

import (
	"fmt"
	"unsafe"
)

const (
	cecMaxLogAddrs = 4
	cecMaxMsgSize  = 16

	cecLogAddrInvalid      = 0xff
	cecLogAddrUnregistered = 15
	cecLogAddrBroadcast    = 15
	cecPhysAddrInvalid     = 0xffff

	// struct cec_caps.capabilities
	cecCapPhysAddr    = 1 << 0
	cecCapLogAddrs    = 1 << 1
	cecCapTransmit    = 1 << 2
	cecCapPassthrough = 1 << 3
	cecCapRC          = 1 << 4

	// CEC_S_MODE
	cecModeNoInitiator         = 0x0
	cecModeInitiator           = 0x1
	cecModeExclInitiator       = 0x2
	cecModeNoFollower          = 0x00
	cecModeFollower            = 0x10
	cecModeExclFollower        = 0x20
	cecModeExclFollowerPassthr = 0x30
	cecModeMonitor             = 0xe0

	// struct cec_log_addrs
	cecLogAddrTypeTV           = 0
	cecLogAddrTypeRecord       = 1
	cecLogAddrTypeTuner        = 2
	cecLogAddrTypePlayback     = 3
	cecLogAddrTypeAudioSystem  = 4
	cecLogAddrTypeSpecific     = 5
	cecLogAddrTypeUnregistered = 6

	cecOpPrimDevtypeTV          = 0
	cecOpPrimDevtypeRecord      = 1
	cecOpPrimDevtypeTuner       = 3
	cecOpPrimDevtypePlayback    = 4
	cecOpPrimDevtypeAudioSystem = 5
	cecOpPrimDevtypeSwitch      = 6
	cecOpPrimDevtypeProcessor   = 7

	cecOpAllDevtypePlayback = 0x10

	cecOpCecVersion13a = 4
	cecOpCecVersion14  = 5
	cecOpCecVersion20  = 6

	cecVendorIDNone = 0xffffffff

	cecLogAddrsFlAllowUnregFallback = 1 << 0
	cecLogAddrsFlAllowRCPassthru    = 1 << 1

	// struct cec_event.event
	cecEventStateChange = 1
	cecEventLostMsgs    = 2

	// struct cec_msg.rx_status / tx_status
	cecTxStatusOK         = 1 << 0
	cecTxStatusArbLost    = 1 << 1
	cecTxStatusNack       = 1 << 2
	cecTxStatusLowDrive   = 1 << 3
	cecTxStatusError      = 1 << 4
	cecTxStatusMaxRetries = 1 << 5
	cecRxStatusOK         = 1 << 0
	cecRxStatusTimeout    = 1 << 1
	cecRxStatusFeatAbort  = 1 << 2
)

// Opcodes.
const (
	cecMsgFeatureAbort          = 0x00
	cecMsgImageViewOn           = 0x04
	cecMsgTextViewOn            = 0x0d
	cecMsgGiveDeckStatus        = 0x1a
	cecMsgDeckStatus            = 0x1b
	cecMsgSetMenuLanguage       = 0x32
	cecMsgStandby               = 0x36
	cecMsgPlay                  = 0x41
	cecMsgDeckControl           = 0x42
	cecMsgUserControlPressed    = 0x44
	cecMsgUserControlReleased   = 0x45
	cecMsgGiveOSDName           = 0x46
	cecMsgSetOSDName            = 0x47
	cecMsgSetOSDString          = 0x64
	cecMsgRoutingChange         = 0x80
	cecMsgRoutingInformation    = 0x81
	cecMsgActiveSource          = 0x82
	cecMsgGivePhysicalAddr      = 0x83
	cecMsgReportPhysicalAddr    = 0x84
	cecMsgRequestActiveSource   = 0x85
	cecMsgSetStreamPath         = 0x86
	cecMsgDeviceVendorID        = 0x87
	cecMsgVendorCommand         = 0x89
	cecMsgVendorRemoteButtonDn  = 0x8a
	cecMsgVendorRemoteButtonUp  = 0x8b
	cecMsgGiveDeviceVendorID    = 0x8c
	cecMsgMenuRequest           = 0x8d
	cecMsgMenuStatus            = 0x8e
	cecMsgGiveDevicePowerStatus = 0x8f
	cecMsgReportPowerStatus     = 0x90
	cecMsgGetMenuLanguage       = 0x91
	cecMsgInactiveSource        = 0x9d
	cecMsgCecVersion            = 0x9e
	cecMsgGetCecVersion         = 0x9f
	cecMsgVendorCommandWithID   = 0xa0
	cecMsgGiveFeatures          = 0xa5
	cecMsgReportFeatures        = 0xa6
	cecMsgAbort                 = 0xff
)

// Operands.
const (
	cecOpAbortUnrecognizedOp = 0
	cecOpAbortRefused        = 4

	cecOpMenuRequestActivate   = 0
	cecOpMenuRequestDeactivate = 1
	cecOpMenuRequestQuery      = 2
	cecOpMenuStateActivated    = 0
	cecOpMenuStateDeactivated  = 1

	cecOpPowerStatusOn = 0

	cecOpDeckInfoPlay    = 0x11
	cecOpDeckInfoStill   = 0x14
	cecOpDeckInfoNoMedia = 0x19
	cecOpDeckInfoStop    = 0x1a

	cecOpDeckCtlSkipFwd = 1
	cecOpDeckCtlSkipRev = 2
	cecOpDeckCtlStop    = 3
	cecOpDeckCtlEject   = 4

	cecOpPlayModePlayFwd = 0x24
	cecOpPlayModeStill   = 0x25
)

// User Control codes (<User Control Pressed> operand), CEC 1.4 table 27.
const (
	cecUICmdSelect        = 0x00
	cecUICmdUp            = 0x01
	cecUICmdDown          = 0x02
	cecUICmdLeft          = 0x03
	cecUICmdRight         = 0x04
	cecUICmdRootMenu      = 0x09
	cecUICmdSetupMenu     = 0x0a
	cecUICmdContentsMenu  = 0x0b
	cecUICmdExit          = 0x0d
	cecUICmdMediaTopMenu  = 0x10
	cecUICmdNumber0       = 0x20
	cecUICmdNumber9       = 0x29
	cecUICmdEnter         = 0x2b
	cecUICmdChannelUp     = 0x30
	cecUICmdChannelDown   = 0x31
	cecUICmdDisplayInfo   = 0x35
	cecUICmdPageUp        = 0x37
	cecUICmdPageDown      = 0x38
	cecUICmdPlay          = 0x44
	cecUICmdStop          = 0x45
	cecUICmdPause         = 0x46
	cecUICmdRewind        = 0x48
	cecUICmdFastForward   = 0x49
	cecUICmdEject         = 0x4a
	cecUICmdForward       = 0x4b
	cecUICmdBackward      = 0x4c
	cecUICmdPlayFunction  = 0x60
	cecUICmdPausePlayFunc = 0x61
	cecUICmdStopFunction  = 0x64
	cecUICmdF1Blue        = 0x71
	cecUICmdF2Red         = 0x72
	cecUICmdF3Green       = 0x73
	cecUICmdF4Yellow      = 0x74
)

// The structs, laid out exactly as the header has them. Sizes are checked in
// cec_test.go against the ioctl encodings PPApp uses.

type cecCaps struct {
	Driver            [32]byte
	Name              [32]byte
	AvailableLogAddrs uint32
	Capabilities      uint32
	Version           uint32
}

type cecLogAddrs struct {
	LogAddr           [cecMaxLogAddrs]uint8
	LogAddrMask       uint16
	CecVersion        uint8
	NumLogAddrs       uint8
	VendorID          uint32
	Flags             uint32
	OsdName           [15]byte
	PrimaryDeviceType [cecMaxLogAddrs]uint8
	LogAddrType       [cecMaxLogAddrs]uint8
	AllDeviceTypes    [cecMaxLogAddrs]uint8
	Features          [cecMaxLogAddrs][12]uint8
}

type cecMsg struct {
	TxTs          uint64
	RxTs          uint64
	Len           uint32
	Timeout       uint32
	Sequence      uint32
	Flags         uint32
	Msg           [cecMaxMsgSize]uint8
	Reply         uint8
	RxStatus      uint8
	TxStatus      uint8
	TxArbLostCnt  uint8
	TxNackCnt     uint8
	TxLowDriveCnt uint8
	TxErrorCnt    uint8
}

type cecEvent struct {
	Ts    uint64
	Event uint32
	Flags uint32
	// The union: for CEC_EVENT_STATE_CHANGE the first two u16 are phys_addr
	// and log_addr_mask; for LOST_MSGS the first u32 is the count.
	Raw [16]uint32
}

// ioctl numbers, built the way <asm-generic/ioctl.h> builds them for arm64.
func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

var (
	cecAdapGCaps     = ioc(iocRead|iocWrite, 'a', 0, unsafe.Sizeof(cecCaps{}))
	cecAdapGPhysAddr = ioc(iocRead, 'a', 1, 2)
	cecAdapGLogAddrs = ioc(iocRead, 'a', 3, unsafe.Sizeof(cecLogAddrs{}))
	cecAdapSLogAddrs = ioc(iocRead|iocWrite, 'a', 4, unsafe.Sizeof(cecLogAddrs{}))
	cecTransmit      = ioc(iocRead|iocWrite, 'a', 5, unsafe.Sizeof(cecMsg{}))
	cecReceive       = ioc(iocRead|iocWrite, 'a', 6, unsafe.Sizeof(cecMsg{}))
	cecDQEvent       = ioc(iocRead|iocWrite, 'a', 7, unsafe.Sizeof(cecEvent{}))
	cecSMode         = ioc(iocWrite, 'a', 9, 4)
)

// Message helpers.

func (m *cecMsg) initiator() uint8   { return m.Msg[0] >> 4 }
func (m *cecMsg) destination() uint8 { return m.Msg[0] & 0xf }
func (m *cecMsg) isBroadcast() bool  { return m.destination() == cecLogAddrBroadcast }

func (m *cecMsg) opcode() (uint8, bool) {
	if m.Len < 2 {
		return 0, false
	}
	return m.Msg[1], true
}

func (m *cecMsg) operand(i int) (uint8, bool) {
	if int(m.Len) < 3+i {
		return 0, false
	}
	return m.Msg[2+i], true
}

// newCECMsg builds an outgoing message from `from` to `to`.
func newCECMsg(from, to, opcode uint8, params ...uint8) cecMsg {
	var m cecMsg
	m.Msg[0] = from<<4 | to&0xf
	m.Msg[1] = opcode
	n := 2
	for _, p := range params {
		if n >= cecMaxMsgSize {
			break
		}
		m.Msg[n] = p
		n++
	}
	m.Len = uint32(n)
	return m
}

// physAddrString formats a physical address the way cec-ctl does, e.g. 1.0.0.0.
func physAddrString(pa uint16) string {
	if pa == cecPhysAddrInvalid {
		return "f.f.f.f (invalid)"
	}
	return fmt.Sprintf("%d.%d.%d.%d", pa>>12, (pa>>8)&0xf, (pa>>4)&0xf, pa&0xf)
}

// Names for the log.
var cecOpcodeNames = map[uint8]string{
	cecMsgFeatureAbort: "Feature Abort", cecMsgImageViewOn: "Image View On",
	cecMsgTextViewOn: "Text View On", cecMsgGiveDeckStatus: "Give Deck Status",
	cecMsgDeckStatus: "Deck Status", cecMsgSetMenuLanguage: "Set Menu Language",
	cecMsgStandby: "Standby", cecMsgPlay: "Play", cecMsgDeckControl: "Deck Control",
	cecMsgUserControlPressed: "User Control Pressed", cecMsgUserControlReleased: "User Control Released",
	cecMsgGiveOSDName: "Give OSD Name", cecMsgSetOSDName: "Set OSD Name",
	cecMsgSetOSDString: "Set OSD String", cecMsgRoutingChange: "Routing Change",
	cecMsgRoutingInformation: "Routing Information", cecMsgActiveSource: "Active Source",
	cecMsgGivePhysicalAddr: "Give Physical Addr", cecMsgReportPhysicalAddr: "Report Physical Addr",
	cecMsgRequestActiveSource: "Request Active Source", cecMsgSetStreamPath: "Set Stream Path",
	cecMsgDeviceVendorID: "Device Vendor ID", cecMsgVendorCommand: "Vendor Command",
	cecMsgVendorRemoteButtonDn: "Vendor Remote Button Down", cecMsgVendorRemoteButtonUp: "Vendor Remote Button Up",
	cecMsgGiveDeviceVendorID: "Give Device Vendor ID", cecMsgMenuRequest: "Menu Request",
	cecMsgMenuStatus: "Menu Status", cecMsgGiveDevicePowerStatus: "Give Device Power Status",
	cecMsgReportPowerStatus: "Report Power Status", cecMsgGetMenuLanguage: "Get Menu Language",
	cecMsgInactiveSource: "Inactive Source", cecMsgCecVersion: "CEC Version",
	cecMsgGetCecVersion: "Get CEC Version", cecMsgVendorCommandWithID: "Vendor Command With ID",
	cecMsgGiveFeatures: "Give Features", cecMsgReportFeatures: "Report Features", cecMsgAbort: "Abort",
}

func cecOpcodeName(op uint8) string {
	if n, ok := cecOpcodeNames[op]; ok {
		return n
	}
	return fmt.Sprintf("opcode 0x%02x", op)
}
