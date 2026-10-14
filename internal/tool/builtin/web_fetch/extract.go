package web_fetch

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"github.com/markusmobius/go-trafilatura"
	"golang.org/x/net/html"

	"github.com/polarisagi/polaris/pkg/apperr"
)

var (
	spaceRe = regexp.MustCompile(`\s+`)
	tagRe   = regexp.MustCompile(`<[^>]*>`)
)

// extractMarkdown 提取网页正文并转换为 Markdown，包含标题提取与回退机制。
func extractMarkdown(htmlBytes []byte, baseURL string, selector string) (title, md string, err error) {
	targetHTML := htmlBytes
	if selector != "" {
		cropped, cropErr := cropBySelector(htmlBytes, selector)
		if cropErr != nil {
			return "", "", cropErr
		}
		targetHTML = cropped
	}

	conv := converter.NewConverter(converter.WithPlugins(table.NewTablePlugin()))
	title, md = extractTrafilatura(targetHTML, baseURL, conv)

	if title == "" {
		title = extractHTMLTitle(targetHTML)
	}

	if len([]rune(strings.TrimSpace(md))) < 200 {
		if fallbackMD := fallbackFullPageMarkdown(targetHTML, baseURL, conv); fallbackMD != "" {
			md = fallbackMD
		}
	}

	return strings.TrimSpace(title), strings.TrimSpace(md), nil
}

// cropBySelector 根据 tag / .class / #id 裁剪目标节点 HTML。
func cropBySelector(htmlBytes []byte, selector string) ([]byte, error) {
	trimmedSelector := strings.TrimSpace(selector)
	if strings.ContainsAny(trimmedSelector, " >[:],") {
		return nil, apperr.New(apperr.CodeInvalidInput, "unsupported selector format: only tag, .class, #id are supported")
	}

	doc, parseErr := html.Parse(bytes.NewReader(htmlBytes))
	if parseErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to parse html for selector", parseErr)
	}

	matched := findNodeBySelector(doc, trimmedSelector)
	if matched == nil {
		return nil, apperr.New(apperr.CodeNotFound, fmt.Sprintf("no element found matching selector: %s", selector))
	}

	var buf bytes.Buffer
	if renderErr := html.Render(&buf, matched); renderErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to render selected element", renderErr)
	}
	return buf.Bytes(), nil
}

// extractTrafilatura 使用 trafilatura 抽取主要正文结构。
func extractTrafilatura(targetHTML []byte, baseURL string, conv *converter.Converter) (title, md string) {
	opts := trafilatura.Options{
		EnableFallback: true,
		ExcludeTables:  false,
		IncludeLinks:   true,
		IncludeImages:  false,
	}
	if u, parseErr := url.Parse(baseURL); parseErr == nil {
		opts.OriginalURL = u
	}

	res, extErr := trafilatura.Extract(bytes.NewReader(targetHTML), opts)
	if extErr != nil || res == nil {
		return "", ""
	}

	title = strings.TrimSpace(res.Metadata.Title)
	if res.ContentNode != nil {
		mdBytes, convErr := conv.ConvertNode(res.ContentNode, converter.WithDomain(baseURL))
		if convErr == nil {
			md = string(mdBytes)
		}
	}
	if md == "" && res.ContentText != "" {
		md = res.ContentText
	}
	return title, md
}

// fallbackFullPageMarkdown 清理干扰元素后转换整页。
func fallbackFullPageMarkdown(targetHTML []byte, baseURL string, conv *converter.Converter) string {
	doc, parseErr := html.Parse(bytes.NewReader(targetHTML))
	if parseErr != nil {
		return ""
	}
	cleanNodeForFallback(doc)
	mdBytes, convErr := conv.ConvertNode(doc, converter.WithDomain(baseURL))
	if convErr != nil {
		return ""
	}
	return string(mdBytes)
}

// extractHTMLTitle 从 HTML 中提取 <title> 标签文本。
func extractHTMLTitle(htmlBytes []byte) string {
	doc, err := html.Parse(bytes.NewReader(htmlBytes))
	if err != nil {
		return ""
	}
	var title string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "title") {
			var sb strings.Builder
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.TextNode {
					sb.WriteString(c.Data)
				}
			}
			title = strings.TrimSpace(sb.String())
			return
		}
		for c := n.FirstChild; c != nil && title == ""; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title
}

// findNodeBySelector 支持 tag, .class, #id。
func findNodeBySelector(n *html.Node, sel string) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && matchSelector(n, sel) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if matched := findNodeBySelector(c, sel); matched != nil {
			return matched
		}
	}
	return nil
}

func matchSelector(n *html.Node, sel string) bool {
	if strings.HasPrefix(sel, ".") {
		return hasNodeClass(n, strings.TrimPrefix(sel, "."))
	}
	if strings.HasPrefix(sel, "#") {
		return hasNodeID(n, strings.TrimPrefix(sel, "#"))
	}
	return strings.EqualFold(n.Data, sel)
}

func hasNodeClass(n *html.Node, targetClass string) bool {
	for _, attr := range n.Attr {
		if attr.Key == "class" {
			for _, c := range strings.Fields(attr.Val) {
				if c == targetClass {
					return true
				}
			}
		}
	}
	return false
}

func hasNodeID(n *html.Node, targetID string) bool {
	for _, attr := range n.Attr {
		if attr.Key == "id" && attr.Val == targetID {
			return true
		}
	}
	return false
}

// cleanNodeForFallback 移除整页回退时的无关非正文元素。
func cleanNodeForFallback(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode && isNoiseTag(c.Data) {
			n.RemoveChild(c)
			c = next
			continue
		}
		cleanNodeForFallback(c)
		c = next
	}
}

func isNoiseTag(tag string) bool {
	lower := strings.ToLower(tag)
	switch lower {
	case "script", "style", "nav", "footer", "header", "aside", "noscript":
		return true
	default:
		return false
	}
}

// extractLinks 提取页面中的所有超链接并绝对化与去重（上限 200 条）。
func extractLinks(htmlBytes []byte, baseURL string) (string, error) {
	doc, err := html.Parse(bytes.NewReader(htmlBytes))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "failed to parse html for links", err)
	}

	base, _ := url.Parse(baseURL)
	seen := make(map[string]bool)
	var lines []string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(lines) >= 200 {
			return
		}
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "a") {
			if linkLine, absURL, ok := formatAnchorLink(n, base); ok && !seen[absURL] {
				seen[absURL] = true
				lines = append(lines, linkLine)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return strings.Join(lines, "\n"), nil
}

func formatAnchorLink(n *html.Node, base *url.URL) (line, absURL string, ok bool) {
	var href string
	for _, attr := range n.Attr {
		if attr.Key == "href" {
			href = strings.TrimSpace(attr.Val)
			break
		}
	}
	if href == "" || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "#") {
		return "", "", false
	}

	abs := href
	if base != nil {
		if parsed, err := base.Parse(href); err == nil {
			abs = parsed.String()
		}
	}

	text := extractNodeText(n)
	text = strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))
	if text == "" {
		text = abs
	}
	return fmt.Sprintf("- [%s](%s)", text, abs), abs, true
}

// extractNodeText 提取节点内的纯文本内容。
func extractNodeText(n *html.Node) string {
	if n == nil {
		return ""
	}
	if n.Type == html.TextNode {
		return n.Data
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sb.WriteString(extractNodeText(c))
	}
	return sb.String()
}

// htmlToText 将 HTML 标签清洗为纯文本。
func htmlToText(htmlBytes []byte) string {
	str := string(htmlBytes)
	str = tagRe.ReplaceAllString(str, " ")
	return strings.TrimSpace(spaceRe.ReplaceAllString(str, " "))
}
