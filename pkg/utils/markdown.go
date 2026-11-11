package utils

import (
	"bytes"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

var (
	reSpaces           = regexp.MustCompile(`[ \t]+`)
	reNewlines         = regexp.MustCompile(`\n{3,}`)
	reEmptyListItem    = regexp.MustCompile(`(?m)^[-*]\s*$`)
	reImageOnlyLink    = regexp.MustCompile(`\[!\[\]\(<[^>]*>\)\]\(<[^>]*>\)`)
	reEmptyHeader      = regexp.MustCompile(`(?m)^#{1,6}\s*$`)
	reLeadingLineSpace = regexp.MustCompile(`(?m)^([ \t])([^ \t\n])`)
)

var skipTags = map[string]bool{
	"script": true, "style": true, "head": true,
	"noscript": true, "template": true,
	"nav": true, "footer": true, "aside": true, "header": true, "form": true, "dialog": true,
}

func isSafeHref(href string) bool {
	lower := strings.ToLower(strings.TrimSpace(href))
	if strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "vbscript:") ||
		strings.HasPrefix(lower, "data:") {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "" || scheme == "http" || scheme == "https" || scheme == "mailto"
}

func isSafeImageSrc(src string) bool {
	lower := strings.ToLower(strings.TrimSpace(src))
	if strings.HasPrefix(lower, "data:image/") {
		return true
	}
	return isSafeHref(src)
}

func escapeMdAlt(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `[`, `\[`)
	s = strings.ReplaceAll(s, `]`, `\]`)
	return s
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func normalizeAttr(val string) string {
	val = strings.ReplaceAll(val, "\n", "")
	val = strings.ReplaceAll(val, "\r", "")
	val = strings.ReplaceAll(val, "\t", "")
	return strings.TrimSpace(val)
}

func isUnlikelyNode(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	classId := strings.ToLower(getAttr(n, "class") + " " + getAttr(n, "id"))
	if classId == " " {
		return false
	}
	if strings.Contains(classId, "article") || strings.Contains(classId, "main") ||
		strings.Contains(classId, "content") {
		return false
	}
	unlikelyKeywords := []string{
		"menu",
		"nav",
		"footer",
		"sidebar",
		"cookie",
		"banner",
		"sponsor",
		"advert",
		"popup",
		"modal",
		"newsletter",
		"share",
		"social",
	}
	for _, keyword := range unlikelyKeywords {
		if strings.Contains(classId, keyword) {
			return true
		}
	}
	return false
}

type converter struct {
	stack      []*bytes.Buffer
	linkHrefs  []string
	linkStates []bool
	emphStack  []string // 记录 "**"、"*"、"~~"，用于缓冲强调内容
	olCounters []int
	inPre      bool
	listDepth  int
}

func newConverter() *converter {
	return &converter{
		stack: []*bytes.Buffer{{}},
	}
}

func (c *converter) write(s string) {
	c.stack[len(c.stack)-1].WriteString(s)
}

func (c *converter) pushBuf() {
	c.stack = append(c.stack, &bytes.Buffer{})
}

func (c *converter) popBuf() string {
	top := c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
	return top.String()
}

func (c *converter) walk(n *html.Node) {
	if n.Type == html.ElementNode {
		if skipTags[n.Data] {
			return
		}
		if isUnlikelyNode(n) {
			return
		}
	}

	if n.Type == html.TextNode {
		text := n.Data
		if !c.inPre {
			text = strings.ReplaceAll(text, "\n", " ")
			text = reSpaces.ReplaceAllString(text, " ")
		}
		if text != "" {
			c.write(text)
		}
		return
	}

	if n.Type != html.ElementNode {
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch)
		}
		return
	}

	// 开始标签
	switch n.Data {
	// 缓冲强调内容，以便对内部文本执行 TrimSpace，
	// 避免跨边界的正则匹配问题。
	case "b", "strong":
		c.emphStack = append(c.emphStack, "**")
		c.pushBuf()
	case "i", "em":
		c.emphStack = append(c.emphStack, "*")
		c.pushBuf()
	case "del", "s":
		c.emphStack = append(c.emphStack, "~~")
		c.pushBuf()

	case "a":
		href := normalizeAttr(getAttr(n, "href"))
		if href != "" && !isSafeHref(href) {
			href = "#"
		}
		hasHref := href != ""
		c.linkStates = append(c.linkStates, hasHref)
		if hasHref {
			c.linkHrefs = append(c.linkHrefs, href)
			c.pushBuf()
		}

	case "h1":
		c.write("\n\n# ")
	case "h2":
		c.write("\n\n## ")
	case "h3":
		c.write("\n\n### ")
	case "h4":
		c.write("\n\n#### ")
	case "h5":
		c.write("\n\n##### ")
	case "h6":
		c.write("\n\n###### ")

	case "p":
		c.write("\n\n")
	case "br":
		c.write("\n")
	case "hr":
		c.write("\n\n---\n\n")

	case "ol":
		c.olCounters = append(c.olCounters, 1)
		// 仅在最外层列表写入前导换行。
		if c.listDepth == 0 {
			c.write("\n")
		}
		c.listDepth++
	case "ul":
		if c.listDepth == 0 {
			c.write("\n")
		}
		c.listDepth++
	case "li":
		c.write("\n")
		if c.listDepth > 1 {
			c.write(strings.Repeat("    ", c.listDepth-1))
		}
		if n.Parent != nil && n.Parent.Data == "ol" && len(c.olCounters) > 0 {
			idx := c.olCounters[len(c.olCounters)-1]
			c.write(strconv.Itoa(idx) + ". ")
			c.olCounters[len(c.olCounters)-1]++
		} else {
			c.write("- ")
		}

	case "pre":
		c.inPre = true
		c.write("\n\n```\n")
	case "code":
		if !c.inPre {
			c.write("`")
		}

	case "blockquote":
		c.pushBuf()
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch)
		}
		inner := strings.TrimSpace(c.popBuf())
		lines := strings.Split(inner, "\n")
		var quoted []string
		for _, l := range lines {
			if strings.TrimSpace(l) == "" {
				quoted = append(quoted, ">")
			} else {
				quoted = append(quoted, "> "+l)
			}
		}
		var deduped []string
		for i, line := range quoted {
			if line == ">" && i > 0 && deduped[len(deduped)-1] == ">" {
				continue
			}
			deduped = append(deduped, line)
		}
		c.write("\n\n" + strings.Join(deduped, "\n") + "\n\n")
		return

	case "img":
		src := normalizeAttr(getAttr(n, "src"))
		if src == "" {
			src = normalizeAttr(getAttr(n, "data-src"))
		}
		if src == "" {
			return
		}
		alt := escapeMdAlt(normalizeAttr(getAttr(n, "alt")))
		if isSafeImageSrc(src) {
			c.write("![" + alt + "](" + src + ")")
		}
		return
	}

	// 遍历子节点
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.walk(ch)
	}

	// 结束标签
	switch n.Data {
	// 弹出缓冲区，去除首尾空白，并用正确的标记包裹。
	case "b", "strong", "i", "em", "del", "s":
		if len(c.emphStack) == 0 {
			break
		}
		marker := c.emphStack[len(c.emphStack)-1]
		c.emphStack = c.emphStack[:len(c.emphStack)-1]
		inner := strings.TrimSpace(c.popBuf())
		if inner != "" {
			c.write(marker + inner + marker)
		}

	case "a":
		if len(c.linkStates) == 0 {
			break
		}
		hasHref := c.linkStates[len(c.linkStates)-1]
		c.linkStates = c.linkStates[:len(c.linkStates)-1]
		if !hasHref {
			break
		}
		href := c.linkHrefs[len(c.linkHrefs)-1]
		c.linkHrefs = c.linkHrefs[:len(c.linkHrefs)-1]
		inner := strings.TrimSpace(c.popBuf())
		if strings.Contains(inner, "\n") {
			lines := strings.Split(inner, "\n")
			linked := false
			for i, l := range lines {
				cleanLine := strings.TrimSpace(l)
				if cleanLine != "" && !strings.HasPrefix(cleanLine, "![") && !linked {
					lines[i] = "[" + cleanLine + "](" + href + ")"
					linked = true
				}
			}
			c.write(strings.Join(lines, "\n"))
		} else {
			c.write("[" + inner + "](" + href + ")")
		}

	case "h1",
		"h2",
		"h3",
		"h4",
		"h5",
		"h6",
		"p",
		"div",
		"section",
		"article",
		"header",
		"footer",
		"aside",
		"nav",
		"figure":
		c.write("\n")

	case "ol":
		c.listDepth--
		if len(c.olCounters) > 0 {
			c.olCounters = c.olCounters[:len(c.olCounters)-1]
		}
		if c.listDepth == 0 {
			c.write("\n")
		}
	case "ul":
		c.listDepth--
		if c.listDepth == 0 {
			c.write("\n")
		}

	case "pre":
		c.inPre = false
		c.write("\n```\n\n")
	case "code":
		if !c.inPre {
			c.write("`")
		}
	}
}

func HtmlToMarkdown(htmlStr string) (string, error) {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return "", err
	}

	c := newConverter()
	c.walk(doc)

	res := c.stack[0].String()

	// 后处理
	res = reImageOnlyLink.ReplaceAllString(res, "")
	res = reEmptyListItem.ReplaceAllString(res, "")
	res = reEmptyHeader.ReplaceAllString(res, "")

	lines := strings.Split(res, "\n")
	var cleanLines []string
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		cleanTest := strings.TrimSpace(line)
		if cleanTest == "[](</>)" || cleanTest == "[](#)" || cleanTest == "-" {
			cleanLines = append(cleanLines, "")
			continue
		}
		cleanLines = append(cleanLines, line)
	}
	res = strings.Join(cleanLines, "\n")

	res = strings.TrimSpace(res)
	res = reNewlines.ReplaceAllString(res, "\n\n")

	// 对于不是列表缩进的行，去掉一个前导空格。
	// "(?m)^([ \t])([^ \t\n])" 恰好匹配行首的一个空格或制表符，其后紧跟
	// 一个非空白字符，因此 "    - nested"（4 个空格）不会被改动。
	res = reLeadingLineSpace.ReplaceAllString(res, "$2")

	return res, nil
}
