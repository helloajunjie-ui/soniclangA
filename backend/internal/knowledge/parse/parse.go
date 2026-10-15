// Package parse extracts plain text from uploaded documents for the knowledge
// RAG pipeline (DESIGN §6.4 / A4). md/txt/html/csv are parsed fully; docx and
// text-based pdf get a minimal extractor (scan/image pdfs are unsupported in
// v1, OCR deferred). Zero third-party dependencies: docx via archive/zip, pdf
// via compress/flate.
package parse

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Supported document kinds.
const (
	KindTXT  = "txt"
	KindMD   = "md"
	KindHTML = "html"
	KindCSV  = "csv"
	KindDOCX = "docx"
	KindPDF  = "pdf"
)

// KindFromName infers the document kind from a filename, defaulting to txt.
func KindFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".md"), strings.HasSuffix(n, ".markdown"):
		return KindMD
	case strings.HasSuffix(n, ".html"), strings.HasSuffix(n, ".htm"):
		return KindHTML
	case strings.HasSuffix(n, ".csv"), strings.HasSuffix(n, ".tsv"):
		return KindCSV
	case strings.HasSuffix(n, ".docx"):
		return KindDOCX
	case strings.HasSuffix(n, ".pdf"):
		return KindPDF
	default:
		return KindTXT
	}
}

// Result is the outcome of parsing one document.
type Result struct {
	Text      string
	PageCount int
}

// Parse extracts plain text from data of the given kind. kind may be "" to
// auto-detect from a leading magic signature.
func Parse(kind string, data []byte) (Result, error) {
	if kind == "" {
		kind = detect(data)
	}
	var (
		text  string
		pages int
		err   error
	)
	switch kind {
	case KindMD:
		text = Markdown(string(data))
	case KindHTML:
		text = HTML(string(data))
	case KindCSV:
		text = CSV(data)
	case KindDOCX:
		text, err = Docx(data)
	case KindPDF:
		text, pages, err = PDF(data)
	case KindTXT, "":
		text = string(data)
	default:
		text = string(data)
	}
	if err != nil {
		return Result{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Result{}, fmt.Errorf("no extractable text (kind=%s)", kind)
	}
	return Result{Text: text, PageCount: pages}, nil
}

func detect(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return KindPDF
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return KindDOCX
	}
	head := strings.ToLower(string(data[:min(512, len(data))]))
	switch {
	case strings.Contains(head, "<html"), strings.Contains(head, "<!doctype html"):
		return KindHTML
	}
	return KindTXT
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- markdown ----

// Markdown strips common markdown syntax, keeping the readable text.
func Markdown(s string) string {
	var out strings.Builder
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			out.WriteString(t)
			out.WriteString("\n")
			continue
		}
		t = stripMDLine(t)
		if t == "" {
			continue
		}
		out.WriteString(t)
		out.WriteString("\n")
	}
	return collapseBlanks(out.String())
}

func stripMDLine(t string) string {
	// headings / blockquote / list markers
	for len(t) > 0 && (t[0] == '#' || t[0] == '>') {
		t = strings.TrimLeft(t, "#> ")
	}
	for _, m := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(t, m) {
			t = t[len(m):]
			break
		}
	}
	// ordered list "1. "
	if i := strings.Index(t, ". "); i > 0 && i <= 3 && allDigits(t[:i]) {
		t = t[i+2:]
	}
	t = strings.TrimLeft(t, " ")
	// images then links
	t = replaceInline(t, "![", "](", ")")
	t = replaceInline(t, "[", "](", ")")
	// emphasis / code markers
	for _, r := range []string{"**", "__", "`", "*", "_", "~~"} {
		t = strings.ReplaceAll(t, r, "")
	}
	// table separators
	if strings.Trim(t, "-|: ") == "" {
		return ""
	}
	t = strings.ReplaceAll(t, "|", " ")
	return strings.TrimSpace(t)
}

func replaceInline(s, open, mid, close string) string {
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i+len(open):], mid)
		if j < 0 {
			return s
		}
		j += i + len(open)
		k := strings.Index(s[j+len(mid):], close)
		if k < 0 {
			return s
		}
		k += j + len(mid)
		inner := s[i+len(open) : j]
		s = s[:i] + inner + s[k+len(close):]
	}
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---- html ----

// HTML strips tags and unescapes entities, dropping script/style content.
func HTML(s string) string {
	var out strings.Builder
	skip := 0 // depth of script/style
	i := 0
	for i < len(s) {
		if s[i] == '<' {
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				break
			}
			tag := strings.ToLower(s[i+1 : i+end])
			name := tagName(tag)
			switch name {
			case "script", "style":
				if !strings.HasPrefix(tag, "/") {
					skip++
				} else if skip > 0 {
					skip--
				}
			case "br", "/p", "/div", "/li", "/tr", "/h1", "/h2", "/h3", "/h4", "/h5", "/h6", "/table":
				if skip == 0 {
					out.WriteString("\n")
				}
			}
			i += end + 1
			continue
		}
		if skip == 0 {
			out.WriteByte(s[i])
		}
		i++
	}
	return unescape(collapseBlanks(out.String()))
}

func tagName(tag string) string {
	t := strings.TrimLeft(tag, "/")
	if sp := strings.IndexAny(t, " \t\r\n"); sp >= 0 {
		t = t[:sp]
	}
	return t
}

func unescape(s string) string {
	r := strings.NewReplacer(
		"&nbsp;", " ", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'",
		"&apos;", "'", "&amp;", "&",
	)
	return r.Replace(s)
}

// ---- csv / tsv ----

// CSV renders rows as "a, b, c" lines.
func CSV(data []byte) string {
	text := string(data)
	if strings.Contains(text, "\r\n") {
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	var out strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out.WriteString(strings.ReplaceAll(line, "\t", ", "))
		out.WriteString("\n")
	}
	return out.String()
}

// ---- docx ----

// Docx extracts text from a .docx (OOXML) container.
func Docx(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx: not a zip container: %w", err)
	}
	var xml []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			xml, err = io.ReadAll(io.LimitReader(rc, 64<<20))
			rc.Close()
			if err != nil {
				return "", err
			}
			break
		}
	}
	if xml == nil {
		return "", errors.New("docx: word/document.xml not found")
	}
	// paragraph -> newline, tab -> space, drop all tags
	var out strings.Builder
	i := 0
	for i < len(xml) {
		if xml[i] == '<' {
			end := bytes.IndexByte(xml[i:], '>')
			if end < 0 {
				break
			}
			tag := string(xml[i+1 : i+end])
			if tag == "/w:p" || tag == "/w:tr" || tag == "/w:br" {
				out.WriteString("\n")
			} else if tag == "/w:tab" {
				out.WriteString("\t")
			}
			i += end + 1
			continue
		}
		out.WriteByte(xml[i])
		i++
	}
	return unescape(collapseBlanks(out.String())), nil
}

// ---- pdf (minimal text extraction) ----

// PDF extracts text from a text-based PDF. It decodes every stream (FlateDecode
// when filtered) and pulls the operands of Tj/TJ/'/" operators. Scanned or
// image-only PDFs yield little/no text — unsupported in v1 (DESIGN A4).
func PDF(data []byte) (string, int, error) {
	var streams []byte
	for _, seg := range splitStreams(data) {
		if dec, ok := inflateIfNeeded(seg.raw, seg.header); ok {
			streams = append(streams, dec...)
		}
	}
	if len(streams) == 0 {
		return "", 0, errors.New("pdf: no decodable content stream")
	}
	text := pdfText(streams)
	pages := bytes.Count(data, []byte("/Type /Page")) + bytes.Count(data, []byte("/Type/Page"))
	if pages == 0 {
		pages = 1
	}
	return text, pages, nil
}

type streamSeg struct {
	header string // object dictionary bytes preceding "stream"
	raw    []byte
}

func splitStreams(data []byte) []streamSeg {
	var out []streamSeg
	i := 0
	for {
		s := bytes.Index(data[i:], []byte("stream"))
		if s < 0 {
			break
		}
		s += i
		// header = bytes back to the nearest "obj"
		hStart := bytes.LastIndex(data[:s], []byte(" obj"))
		if hStart < 0 {
			hStart = 0
		}
		header := string(data[hStart:s])
		// skip EOL after "stream"
		j := s + len("stream")
		if j < len(data) && data[j] == '\r' {
			j++
		}
		if j < len(data) && data[j] == '\n' {
			j++
		}
		e := bytes.Index(data[j:], []byte("endstream"))
		if e < 0 {
			break
		}
		raw := data[j : j+e]
		raw = bytes.TrimSuffix(raw, []byte("\n"))
		raw = bytes.TrimSuffix(raw, []byte("\r"))
		out = append(out, streamSeg{header: header, raw: raw})
		i = j + e + len("endstream")
	}
	return out
}

func inflateIfNeeded(raw []byte, header string) ([]byte, bool) {
	if !strings.Contains(header, "FlateDecode") {
		return raw, true
	}
	fr := flate.NewReader(bytes.NewReader(raw))
	defer fr.Close()
	dec, err := io.ReadAll(io.LimitReader(fr, 128<<20))
	if err != nil && len(dec) == 0 {
		return nil, false
	}
	return dec, true
}

// pdfText walks a content stream and collects text-show operands.
func pdfText(content []byte) string {
	var out strings.Builder
	i := 0
	n := len(content)
	lineBreak := func() { out.WriteString("\n") }
	for i < n {
		c := content[i]
		switch {
		case c == '(':
			s, next := readLiteral(content, i)
			i = next
			if tok := peekToken(content, i); tok == "Tj" || tok == "TJ" || tok == "'" || tok == "\"" {
				out.WriteString(s)
				out.WriteString(" ")
				i += len(tok)
			}
		case c == '<' && i+1 < n && content[i+1] != '<':
			s, next := readHexString(content, i)
			i = next
			if tok := peekToken(content, i); tok == "Tj" || tok == "TJ" || tok == "'" || tok == "\"" {
				out.WriteString(s)
				out.WriteString(" ")
				i += len(tok)
			}
		case c == '<' && i+1 < n && content[i+1] == '<':
			// inline dictionary — skip to matching ">>"
			e := bytes.Index(content[i:], []byte(">>"))
			if e < 0 {
				i = n
			} else {
				i += e + 2
			}
		case c == 'T':
			if tok := peekToken(content, i); tok == "T*" || tok == "Td" || tok == "TD" || tok == "BT" {
				lineBreak()
				i += len(tok)
				continue
			}
			i++
		default:
			i++
		}
	}
	return collapseBlanks(out.String())
}

func peekToken(content []byte, i int) string {
	j := i
	for j < len(content) && isSpace(content[j]) {
		j++
	}
	k := j
	for k < len(content) && !isSpace(content[k]) && content[k] != '(' && content[k] != '<' && content[k] != '[' && content[k] != ']' {
		k++
	}
	return string(content[j:k])
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}

// readLiteral reads a PDF literal string starting at '(' and returns its text.
func readLiteral(b []byte, i int) (string, int) {
	var out []byte
	depth := 0
	i++ // opening '('
	for i < len(b) {
		c := b[i]
		if c == '\\' && i+1 < len(b) {
			n := b[i+1]
			switch n {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b', 'f':
				out = append(out, ' ')
			case '\n':
				// line continuation
			case '\r':
				if i+2 < len(b) && b[i+2] == '\n' {
					i++
				}
			default:
				if n >= '0' && n <= '7' {
					oct := string(n)
					for k := 0; k < 2 && i+2+k < len(b) && b[i+2+k] >= '0' && b[i+2+k] <= '7'; k++ {
						oct += string(b[i+2+k])
					}
					if v, err := parseOct(oct); err == nil {
						out = append(out, byte(v))
					}
					i += len(oct) - 1
				} else {
					out = append(out, n)
				}
			}
			i += 2
			continue
		}
		if c == '(' {
			depth++
			out = append(out, c)
			i++
			continue
		}
		if c == ')' {
			if depth == 0 {
				i++
				break
			}
			depth--
			out = append(out, c)
			i++
			continue
		}
		out = append(out, c)
		i++
	}
	return string(out), i
}

func readHexString(b []byte, i int) (string, int) {
	i++ // '<'
	start := i
	for i < len(b) && b[i] != '>' {
		i++
	}
	hex := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			return r
		}
		return -1
	}, string(b[start:i]))
	if i < len(b) {
		i++
	}
	if len(hex)%2 == 1 {
		hex += "0"
	}
	out := make([]byte, 0, len(hex)/2)
	for j := 0; j+1 < len(hex); j += 2 {
		var v byte
		for _, ch := range []byte{hex[j], hex[j+1]} {
			v <<= 4
			switch {
			case ch >= '0' && ch <= '9':
				v |= ch - '0'
			case ch >= 'a' && ch <= 'f':
				v |= ch - 'a' + 10
			case ch >= 'A' && ch <= 'F':
				v |= ch - 'A' + 10
			}
		}
		if v == 0 {
			continue
		}
		out = append(out, v)
	}
	return string(out), i
}

func parseOct(s string) (int, error) {
	v := 0
	for _, r := range s {
		v = v*8 + int(r-'0')
	}
	return v, nil
}

// ---- shared ----

func collapseBlanks(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, l := range lines {
		t := strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(t) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, t)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
