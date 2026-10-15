// Package chunk splits document text into retrieval-friendly chunks
// (DESIGN §6.4). It prefers paragraph/sentence boundaries over hard cuts,
// applies an overlap window so context is not lost at seams, and stamps each
// chunk with a content hash used for dedupe and re-ingest snapshots.
package chunk

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Chunk is one retrievable unit.
type Chunk struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
	Title string `json:"title,omitempty"`
	Hash  string `json:"hash"`
}

// Options controls chunking.
type Options struct {
	Size     int // target characters per chunk (0 => 800)
	Overlap  int // characters carried over between chunks (0 => 80)
	MaxChars int // hard cap per chunk (0 => Size*2)
}

func (o Options) norm() Options {
	if o.Size <= 0 {
		o.Size = 800
	}
	if o.Overlap < 0 {
		o.Overlap = 0
	}
	if o.Overlap >= o.Size {
		o.Overlap = o.Size / 5
	}
	if o.MaxChars <= 0 {
		o.MaxChars = o.Size * 2
	}
	return o
}

// Split divides text into chunks.
func Split(text string, opt Options) []Chunk {
	opt = opt.norm()
	// Build paragraph units; long paragraphs are further split by sentence.
	var units []string
	for _, para := range strings.Split(text, "\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if len([]rune(para)) <= opt.Size {
			units = append(units, para)
			continue
		}
		units = append(units, splitBySentence(para, opt.Size)...)
	}
	if len(units) == 0 {
		return nil
	}
	var out []Chunk
	var cur []rune
	// flush emits the accumulated chunk, then retains its tail as the overlap
	// seed for the next chunk so context is not lost at seams.
	flush := func() {
		if len(cur) == 0 {
			return
		}
		t := strings.TrimSpace(string(cur))
		if t != "" {
			out = append(out, Chunk{Index: len(out), Text: t, Hash: hash(t)})
		}
		if opt.Overlap > 0 && len(cur) > opt.Overlap {
			cur = append([]rune{}, cur[len(cur)-opt.Overlap:]...)
		} else {
			cur = cur[:0]
		}
	}
	for _, u := range units {
		ur := []rune(u)
		if len(cur)+len(ur) > opt.Size && len(cur) > 0 {
			flush()
		} else if len(cur) > 0 {
			// keep paragraph/sentence boundaries readable
			cur = append(cur, '\n')
		}
		cur = append(cur, ur...)
		// hard cap: never exceed MaxChars in one chunk
		for len(cur) > opt.MaxChars {
			head := cur[:opt.Size]
			out = append(out, Chunk{Index: len(out), Text: strings.TrimSpace(string(head)), Hash: hash(string(head))})
			keep := opt.Overlap
			if keep > len(cur)-opt.Size {
				keep = len(cur) - opt.Size
			}
			cur = append([]rune{}, cur[opt.Size-keep:]...)
		}
	}
	flush()
	return out
}

func splitBySentence(para string, size int) []string {
	runes := []rune(para)
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for i := 0; i < len(runes); i++ {
		cur = append(cur, runes[i])
		isEnd := runes[i] == '。' || runes[i] == '！' || runes[i] == '？' ||
			runes[i] == '.' || runes[i] == '!' || runes[i] == '?' || runes[i] == ';' || runes[i] == '；'
		if isEnd || len(cur) >= size {
			flush()
		}
	}
	flush()
	return out
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
