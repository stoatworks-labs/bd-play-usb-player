package main

// Remote-control keys, abstracted away from how they arrived.
//
// A TV forwards its remote to the active HDMI device as CEC <User Control
// Pressed> messages, but not every TV forwards every key and some send
// transport keys as <Deck Control> / <Play> instead. Everything is folded
// into this small set so the session logic has one vocabulary, and so the
// same keys can be injected over the REST API — which is also how the whole
// thing is tested without a television.

import (
	"fmt"
	"strings"
)

type Key int

const (
	KeyNone Key = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeySelect
	KeyBack // Exit / Return
	KeyPageUp
	KeyPageDown
	KeyPlay
	KeyPause
	KeyStop
	KeyNext
	KeyPrev
	KeyInfo
	KeyRed
	KeyGreen
	KeyYellow
	KeyBlue
	KeyEject
)

var keyNames = map[Key]string{
	KeyUp: "up", KeyDown: "down", KeyLeft: "left", KeyRight: "right",
	KeySelect: "select", KeyBack: "back", KeyPageUp: "pageup", KeyPageDown: "pagedown",
	KeyPlay: "play", KeyPause: "pause", KeyStop: "stop", KeyNext: "next", KeyPrev: "prev",
	KeyInfo: "info", KeyRed: "red", KeyGreen: "green", KeyYellow: "yellow", KeyBlue: "blue",
	KeyEject: "eject",
}

func (k Key) String() string {
	if n, ok := keyNames[k]; ok {
		return n
	}
	return fmt.Sprintf("key(%d)", int(k))
}

// ParseKey accepts the names above plus a few aliases people type.
func ParseKey(s string) (Key, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "ok", "enter":
		return KeySelect, true
	case "exit", "return", "escape", "esc":
		return KeyBack, true
	case "ff", "forward", "skip":
		return KeyNext, true
	case "rew", "rewind", "backward", "previous":
		return KeyPrev, true
	}
	for k, n := range keyNames {
		if n == s {
			return k, true
		}
	}
	return KeyNone, false
}

// keyFromUICommand maps a <User Control Pressed> operand to a Key. Anything
// not listed is deliberately dropped: volume, power and channel keys belong to
// the TV, and number keys have no meaning in a file list.
func keyFromUICommand(cmd uint8) Key {
	switch cmd {
	case cecUICmdSelect, cecUICmdEnter:
		return KeySelect
	case cecUICmdUp:
		return KeyUp
	case cecUICmdDown:
		return KeyDown
	case cecUICmdLeft:
		return KeyLeft
	case cecUICmdRight:
		return KeyRight
	case cecUICmdExit:
		return KeyBack
	case cecUICmdRootMenu, cecUICmdSetupMenu, cecUICmdContentsMenu, cecUICmdMediaTopMenu:
		// "Menu" on the remote: treat as back-to-the-list, the nearest thing
		// to a menu we have.
		return KeyBack
	case cecUICmdPageUp, cecUICmdChannelUp:
		return KeyPageUp
	case cecUICmdPageDown, cecUICmdChannelDown:
		return KeyPageDown
	case cecUICmdPlay, cecUICmdPlayFunction:
		return KeyPlay
	case cecUICmdPause, cecUICmdPausePlayFunc:
		return KeyPause
	case cecUICmdStop, cecUICmdStopFunction:
		return KeyStop
	case cecUICmdFastForward, cecUICmdForward:
		return KeyNext
	case cecUICmdRewind, cecUICmdBackward:
		return KeyPrev
	case cecUICmdDisplayInfo:
		return KeyInfo
	case cecUICmdF2Red:
		return KeyRed
	case cecUICmdF3Green:
		return KeyGreen
	case cecUICmdF4Yellow:
		return KeyYellow
	case cecUICmdF1Blue:
		return KeyBlue
	case cecUICmdEject:
		return KeyEject
	}
	return KeyNone
}

// keyFromDeckControl maps a <Deck Control> operand. Some TVs (and most
// AV receivers) drive playback devices this way rather than with user
// control codes.
func keyFromDeckControl(mode uint8) Key {
	switch mode {
	case cecOpDeckCtlSkipFwd:
		return KeyNext
	case cecOpDeckCtlSkipRev:
		return KeyPrev
	case cecOpDeckCtlStop:
		return KeyStop
	case cecOpDeckCtlEject:
		return KeyEject
	}
	return KeyNone
}

// keyFromPlay maps a <Play> operand.
func keyFromPlay(mode uint8) Key {
	switch mode {
	case cecOpPlayModePlayFwd:
		return KeyPlay
	case cecOpPlayModeStill:
		return KeyPause
	}
	return KeyNone
}
