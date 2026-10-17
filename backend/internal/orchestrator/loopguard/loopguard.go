// Package loopguard implements the loop watchdog for the agentic tool loop.
//
// The model drives a loop of "call tool -> feed result back -> call again".
// A confused model can spin forever: repeating the identical call, hammering
// an endpoint that keeps failing, or re-reading the same unchanged output.
// This package tracks four independent signals and escalates a warning, then
// a hard stop, with a Chinese reason the UI can show verbatim.
package loopguard

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// Signal levels returned by Observe.
const (
	SignalOK   = ""     // nothing wrong
	SignalWarn = "warn" // nearing a threshold — surface a hint, keep going
	SignalStop = "stop" // hard fuse — terminate the loop
)

// Tuning thresholds.
const (
	// RepeatSameCall stops on this many consecutive identical tool calls.
	RepeatSameCall = 3
	// WarnRepeatSameCall warns one step before the hard fuse.
	WarnRepeatSameCall = 2
	// MaxFailStreak stops after this many consecutive tool failures.
	MaxFailStreak = 4
	// MaxSameResult stops when this many consecutive results are byte-identical
	// (even with different arguments — e.g. a page that returns the same error).
	MaxSameResult = 3
)

// State is the watchdog state across loop iterations.
type State struct {
	Steps    int
	MaxSteps int

	lastSignature string
	repeatRun     int

	failStreak   int
	lastFailName string

	lastResult string
	sameResult int
}

// New builds a watchdog. maxSteps is the ceiling on loop iterations; values
// <= 0 fall back to 30.
func New(maxSteps int) *State {
	if maxSteps <= 0 {
		maxSteps = 30
	}
	return &State{MaxSteps: maxSteps}
}

// Signature hashes a tool name plus its raw arguments so two calls with the
// same intent collapse to one signature regardless of key ordering.
func Signature(name, argsJSON string) string {
	trim := strings.Join(strings.Fields(argsJSON), "")
	sum := sha1.Sum([]byte(name + "|" + trim))
	return name + ":" + hex.EncodeToString(sum[:8])
}

// resultKey normalizes tool output for change detection. Only the leading
// portion matters; a stable prefix of identical output is a strong stall
// signal while avoiding noise from timestamps in a long tail.
func resultKey(result string) string {
	r := strings.TrimSpace(result)
	if len(r) > 400 {
		r = r[:400]
	}
	return r
}

// Tick is called once per loop iteration (before the model is invoked). It
// enforces the step ceiling.
func (s *State) Tick() (signal, reason string) {
	s.Steps++
	if s.Steps > s.MaxSteps {
		return SignalStop, "已达到最大工具循环步数（" + itoa(s.MaxSteps) + "），任务可能过于复杂或陷入循环，已自动停止。请缩小任务范围或换个说法重试。"
	}
	return SignalOK, ""
}

// Observe is called after a tool call resolves. failed indicates the tool
// itself returned an error; result is its textual output.
func (s *State) Observe(name, signature, result string, failed bool) (signal, reason string) {
	// 1) Consecutive identical calls.
	if signature == s.lastSignature && signature != "" {
		s.repeatRun++
	} else {
		s.repeatRun = 0
	}
	s.lastSignature = signature

	// 2) Consecutive failures (same tool).
	if failed {
		if name == s.lastFailName {
			s.failStreak++
		} else {
			s.failStreak = 1
		}
		s.lastFailName = name
	} else {
		s.failStreak = 0
		s.lastFailName = ""
	}

	// 3) Byte-identical results in a row (no forward progress).
	key := resultKey(result)
	if key != "" && key == s.lastResult {
		s.sameResult++
	} else {
		s.sameResult = 0
	}
	s.lastResult = key

	// Hard fuses first.
	if s.repeatRun >= RepeatSameCall {
		return SignalStop, "检测到连续 " + itoa(RepeatSameCall+1) + " 次执行完全相同的工具调用（" + name + "），判定为陷入死循环，已停止。请换一种方式，或检查参数是否正确。"
	}
	if s.failStreak >= MaxFailStreak {
		return SignalStop, "工具 " + name + " 已连续失败 " + itoa(MaxFailStreak) + " 次，继续重试无意义，已停止。请检查依赖或权限后重试。"
	}
	if s.sameResult >= MaxSameResult {
		return SignalStop, "连续 " + itoa(MaxSameResult+1) + " 次工具返回完全相同的结果，没有任何新进展，判定为空转，已停止。"
	}

	// Soft warnings.
	if s.repeatRun >= WarnRepeatSameCall {
		return SignalWarn, "检测到正在重复相同的工具调用，若继续无进展将自动停止。"
	}
	if s.failStreak == MaxFailStreak-1 {
		return SignalWarn, "工具 " + name + " 已连续失败多次，再失败将自动停止。"
	}
	return SignalOK, ""
}

// Reset clears the no-progress signals when a tool call produced clearly new,
// useful output (e.g. a successful call after a string of failures). Not
// strictly required — Observe already decays — but exposed for completeness.
func (s *State) Reset() {
	s.repeatRun = 0
	s.failStreak = 0
	s.sameResult = 0
	s.lastResult = ""
	s.lastSignature = ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
