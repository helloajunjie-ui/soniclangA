package chunk

import (
	"strings"
	"testing"
)

func TestSplitRespectsSize(t *testing.T) {
	long := strings.Repeat("这是一段用于测试的中文文本内容。", 60)
	chunks := Split(long, Options{Size: 200, Overlap: 40})
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	tooBig := 0
	for _, c := range chunks {
		if len([]rune(c.Text)) > 200*2 {
			tooBig++
		}
		if c.Hash == "" {
			t.Error("chunk missing hash")
		}
	}
	if tooBig > 0 {
		t.Errorf("%d chunks exceed hard cap", tooBig)
	}
}

func TestSplitIndicesAndOverlap(t *testing.T) {
	text := "第一段落内容。\n第二段落内容。\n第三段落内容。\n第四段落内容。\n"
	chunks := Split(text, Options{Size: 20, Overlap: 8})
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("chunk index mismatch: got %d want %d", c.Index, i)
		}
	}
	// consecutive chunks must share the overlap tail
	if len(chunks) >= 2 {
		a, b := chunks[0].Text, chunks[1].Text
		ar := []rune(a)
		// slice by rune (not byte); trim because a leading separator is
		// stripped when the carried tail becomes the next chunk's start.
		tail := strings.TrimSpace(string(ar[len(ar)-8:]))
		if !strings.HasPrefix(b, tail) {
			t.Errorf("overlap not carried: tail=%q next=%q", tail, b)
		}
	}
}

func TestSplitKeepsParagraphBoundaries(t *testing.T) {
	// paragraphs must not be glued together: "架构AgentHub" would be a bug.
	text := "架构\n\nAgentHub 采用零依赖设计。\n\n存储层使用 JSON。"
	chunks := Split(text, Options{Size: 800, Overlap: 0})
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	got := chunks[0].Text
	if !strings.Contains(got, "架构\nAgentHub") {
		t.Errorf("paragraph boundary lost: %q", got)
	}
	if strings.Contains(got, "架构AgentHub") {
		t.Errorf("paragraphs glued together: %q", got)
	}
}

func TestSplitEmpty(t *testing.T) {
	if got := Split("", Options{}); got != nil {
		t.Errorf("empty text should yield nil, got %v", got)
	}
	if got := Split("   \n\n  ", Options{}); got != nil {
		t.Errorf("blank text should yield nil, got %v", got)
	}
}

func TestSplitSingleChunk(t *testing.T) {
	chunks := Split("短文本。", Options{Size: 800})
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Text != "短文本。" {
		t.Errorf("unexpected text %q", chunks[0].Text)
	}
}
