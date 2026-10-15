package envprobe

import (
	"fmt"
	"strings"
)

// Render turns a Report into the text the model reads.
//
// The order is the flow's order — what can start now, what needs preparation,
// what is not here yet — because the model acts on the first line that applies
// and stops reading. A flat software list would leave it to re-derive the
// decision that Probe already made.
func Render(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "本机环境探测（%s，%s）\n查询：%s\n\n", r.Platform, r.ScannedAt, r.Query)

	switch {
	case len(r.Ready) > 0:
		b.WriteString("可以直接开工（已安装，且能无界面驱动）：\n")
		for _, p := range r.Ready {
			fmt.Fprintf(&b, "  - %s%s", p.Name, ver(p.Version))
			if p.Path != "" {
				fmt.Fprintf(&b, "  %s", p.Path)
			}
			if p.DriveNote != "" {
				fmt.Fprintf(&b, "\n      用法：%s", p.DriveNote)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	default:
		b.WriteString("可以直接开工：无\n\n")
	}

	if len(r.NeedsInit) > 0 {
		b.WriteString("装了，但只能操作界面（需要先花点时间摸清怎么驱动，先让用户等一下）：\n")
		for _, p := range r.NeedsInit {
			fmt.Fprintf(&b, "  - %s%s", p.Name, ver(p.Version))
			if p.Path != "" {
				fmt.Fprintf(&b, "  %s", p.Path)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(r.Missing) > 0 {
		b.WriteString("本机没有（要么帮用户装，要么让用户自己装）：\n")
		for _, w := range r.Missing {
			fmt.Fprintf(&b, "  - %s：%s\n      安装：%s\n", w.Name, w.Why, w.Install)
		}
		if r.MissingMore > 0 {
			fmt.Fprintf(&b, "  （另有 %d 个同类工具未列出，需要时再查）\n", r.MissingMore)
		}
		b.WriteString("\n")
	}

	if r.NoMatch {
		b.WriteString("这条需求没有匹配到内置工具表：请直接问用户想用什么软件，或从下面的已装列表里挑。\n\n")
	}
	if len(r.Others) > 0 {
		fmt.Fprintf(&b, "本机还装了：%s\n", strings.Join(r.Others, "、"))
	}
	return strings.TrimRight(b.String(), "\n")
}

func ver(v string) string {
	if v == "" {
		return ""
	}
	return " " + v
}
