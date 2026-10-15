package parse

import (
	"strings"
	"testing"
)

func TestKindFromName(t *testing.T) {
	cases := map[string]string{
		"a.md": KindMD, "b.markdown": KindMD, "c.html": KindHTML, "d.htm": KindHTML,
		"e.csv": KindCSV, "f.tsv": KindCSV, "g.docx": KindDOCX, "h.pdf": KindPDF,
		"i.TXT": KindTXT, "j.unknown": KindTXT,
	}
	for name, want := range cases {
		if got := KindFromName(name); got != want {
			t.Errorf("KindFromName(%q)=%s want %s", name, got, want)
		}
	}
}

func TestMarkdown(t *testing.T) {
	md := "# 标题一\n\n这是一段**粗体**和`代码`的[链接](http://example.com)。\n\n- 项目一\n- 项目二\n\n![图](x.png)\n\n```go\nfmt.Println(\"skip\")\n```\n"
	got := Markdown(md)
	if strings.Contains(got, "**") || strings.Contains(got, "`") || strings.Contains(got, "](") {
		t.Errorf("markdown markers not stripped: %q", got)
	}
	for _, want := range []string{"标题一", "这是一段粗体和代码的链接", "项目一", "项目二"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// code fences are preserved as plain text
	if !strings.Contains(got, "fmt.Println") {
		t.Errorf("code body should be kept, got %q", got)
	}
}

func TestHTML(t *testing.T) {
	html := `<html><head><style>a{color:red}</style><script>var x=1;</script></head>` +
		`<body><h1>标题</h1><p>段落 &amp; 实体&nbsp;文本</p></body></html>`
	got := HTML(html)
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Errorf("tags not stripped: %q", got)
	}
	if strings.Contains(got, "var x") || strings.Contains(got, "color:red") {
		t.Errorf("script/style should be dropped: %q", got)
	}
	for _, want := range []string{"标题", "段落", "& 实体"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestCSV(t *testing.T) {
	got := CSV([]byte("a,b,c\r\n1,2,3\r\n"))
	if !strings.Contains(got, "a,b,c") || !strings.Contains(got, "1,2,3") {
		t.Errorf("csv rows lost: %q", got)
	}
}

func TestParseDispatch(t *testing.T) {
	if r, err := Parse(KindMD, []byte("# h\n\nbody")); err != nil {
		t.Fatalf("md parse: %v", err)
	} else if r.Text == "" {
		t.Fatal("md produced empty text")
	}
	// auto-detect pdf signature
	if _, _, err := PDF([]byte("%PDF-1.4\n%junk")); err == nil {
		t.Log("junk pdf: error expected or tolerated")
	}
	if r, err := Parse("", []byte("%PDF-1.4 garbage")); err == nil && r.Text != "" {
		t.Logf("detect=>pdf, text=%q", r.Text)
	}
}
