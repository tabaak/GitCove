package web

// Code is coloured and Markdown rendered on the server, so both work with GitHub down and no
// script on the page.

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"net/url"
	"path"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	gmtext "github.com/yuin/goldmark/text"
)

// maxHighlight is the largest file coloured; Chroma's regexps get slow on bigger ones, as GitHub's do.
const maxHighlight = 256 << 10

// lexer is the lexer for a file name, or nil if Chroma knows no language for it.
func lexer(name string) chroma.Lexer {
	if l := lexers.Match(path.Base(name)); l != nil {
		return chroma.Coalesce(l)
	}
	return nil
}

// highlight splits code into lines of HTML, each token in a span whose class names its kind,
// such as "hl-k" for a keyword. Without a lexer the lines are only escaped.
func highlight(l chroma.Lexer, code string) []template.HTML {
	var it chroma.Iterator
	if l != nil && len(code) <= maxHighlight {
		it, _ = l.Tokenise(nil, code)
	}
	if it == nil {
		it = chroma.Literator(chroma.Token{Type: chroma.Text, Value: code})
	}
	var lines []template.HTML
	for _, toks := range chroma.SplitTokensIntoLines(it.Tokens()) {
		var b strings.Builder
		for _, t := range toks {
			v := html.EscapeString(strings.TrimSuffix(t.Value, "\n"))
			if c := chroma.StandardTypes[t.Type]; c != "" && v != "" {
				fmt.Fprintf(&b, `<span class="hl-%s">%s</span>`, c, v)
			} else {
				b.WriteString(v)
			}
		}
		lines = append(lines, template.HTML(b.String()))
	}
	return lines
}

// isMarkdown says whether a file is shown rendered, as GitHub shows README.md.
func isMarkdown(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown", ".mdown", ".mkd":
		return true
	}
	return false
}

// md leaves out raw HTML and javascript: links, as goldmark does unless told it's safe, so a
// README can't run script here.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// markdown renders a Markdown file at dir in a repo's branch. Relative links point into the
// branch, as on GitHub.
func markdown(src, repo, branch, dir string) (template.HTML, error) {
	doc := md.Parser().Parse(gmtext.NewReader([]byte(src)))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := n.(*ast.Link); ok && entering {
			u, err := url.Parse(string(l.Destination))
			if err == nil && u.Scheme == "" && u.Host == "" && u.Path != "" && !strings.HasPrefix(u.Path, "/") {
				u.Path = "/" + repo + "/blob/" + branch + "/" + strings.TrimPrefix(path.Join(dir, u.Path), "/")
				l.Destination = []byte(u.String())
			}
		}
		// shortcut: relative images stay broken, as there's no raw file route; add one if READMEs need them.
		return ast.WalkContinue, nil
	})
	var b bytes.Buffer
	err := md.Renderer().Render(&b, []byte(src), doc)
	return template.HTML(b.String()), err
}
