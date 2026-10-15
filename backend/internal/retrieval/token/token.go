// Package token estimates token counts with a cheap heuristic (DESIGN §5.2
// retrieval.Estimate): CJK ~0.6 tokens/char, ASCII ~0.25 tokens/char, +3 per
// message. The signature is fixed so context-window, memory and task budgets
// all call the same implementation (no per-module reimplementation).
package token

import "unicode"

// Estimate returns an approximate token count for text.
func Estimate(text string) int {
	cjk, ascii := 0, 0
	for _, r := range text {
		switch {
		case r >= 0x4e00 && r <= 0x9fff, r >= 0x3400 && r <= 0x4dbf:
			cjk++
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			ascii++
		case unicode.Is(unicode.Han, r):
			cjk++
		}
	}
	return int(float64(cjk)*0.6 + float64(ascii)*0.25 + 3)
}
